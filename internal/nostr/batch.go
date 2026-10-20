package nostr

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getAlby/go-nostr"
	"github.com/getAlby/go-nostr/nip11"
	"github.com/sirupsen/logrus"
)

const (
	// Most #p values one batched REQ asks for. A full REQ is ~26KB (200
	// authors and 200 #p values), well under the 128KB message limit of strfry
	// and nostr-rs-relay, the strictest common relays.
	batchMaxPubkeys = 200
	// A relay that answers a batched REQ with CLOSED may limit filter size, so
	// the batch halves its chunk size, down to this floor.
	batchMinPubkeys = 25
	// The concurrent REQ cap per connection assumed when a relay doesn't
	// publish one (NIP-11 limitation.max_subscriptions). Relays cap REQs per
	// connection (nostream at 10, Primal's strfry at 20, Rizful's at 100) and
	// refuse the rest with a NOTICE that names no subscription, so a refused
	// REQ can't be detected. Batches therefore open their own connections and
	// stay under the cap, with one slot to spare for the moment a re-sent
	// chunk's new REQ overlaps its old one.
	batchMaxReqsPerConn = 10
	// Upper bound on chunks per connection, however high a relay's cap.
	batchMaxChunksPerConn = 50
	// After this many failed connection attempts in a row, a relay counts as
	// unreachable and is tried again after batchUnreachableRetry.
	batchMaxConnectFailures = 10
	// Membership changes (new subscriptions, cancels, the restore at startup)
	// are collected for this long before REQs are re-sent, so a burst of
	// changes costs a single round.
	batchDebounce = 2 * time.Second
	// How long event ids are remembered, to drop the duplicates that arrive
	// while REQs overlap or catch up.
	batchSeenTTL = 10 * time.Minute
	// How long to wait before trying an unreachable relay again, so dead
	// relays cost little without being dropped for good.
	batchUnreachableRetry = 30 * time.Minute
	// A REQ that replaces one that failed asks for events from this long before
	// the failure, so nothing published in between is missed.
	batchCatchUp = 30 * time.Second
	// A batch's first REQs ask for events from this long ago, to pick up what
	// was published while this process (re)started (strfry keeps ephemeral
	// events for 5 minutes, other relays store NWC events). Kept short:
	// seen ids don't survive a restart, so anything the previous process
	// already delivered in this window is delivered again.
	batchStartupCatchUp = 1 * time.Minute
)

// batchResubscribeDelay is the backoff after a batch's REQs fail; a variable
// so tests don't wait for the full jitter window.
var batchResubscribeDelay = resubscribeDelay

// batchable reports whether a subscription is a plain "events by this author
// tagged with this pubkey" filter on ephemeral kinds, which is what NWC
// notification subscriptions look like. Those share REQs with the others on
// the same relay. The batch picks the since of its REQs itself (see
// batchCatchUp and batchStartupCatchUp), so a subscription's own since is not
// used.
func batchable(subscription *Subscription) bool {
	if subscription.Ids != nil && len(*subscription.Ids) > 0 {
		return false
	}
	if subscription.Limit != 0 || subscription.Search != "" || !subscription.Until.IsZero() {
		return false
	}
	if subscription.Authors == nil || len(*subscription.Authors) != 1 {
		return false
	}
	if subscription.Kinds == nil || len(*subscription.Kinds) == 0 {
		return false
	}
	for _, kind := range *subscription.Kinds {
		if kind < 20000 || kind >= 30000 {
			return false
		}
	}
	if subscription.Tags == nil || len(*subscription.Tags) != 1 {
		return false
	}
	p, ok := (*subscription.Tags)["p"]
	return ok && len(p) == 1
}

type batchMember struct {
	subscription     Subscription
	subscriptionType PersistentSubscriptionType
}

func (m *batchMember) author() string { return (*m.subscription.Authors)[0] }
func (m *batchMember) p() string      { return (*m.subscription.Tags)["p"][0] }

// matches reports whether an event belongs to this subscription. Catch-up
// REQs can return events from before a subscription existed (when it joined a
// chunk that is catching up); those are dropped, with batchCatchUp of slack
// for clock skew between the wallet and us.
func (m *batchMember) matches(event *nostr.Event) bool {
	if event.PubKey != m.author() || !slices.Contains(*m.subscription.Kinds, event.Kind) {
		return false
	}
	return m.subscription.CreatedAt.IsZero() || !event.CreatedAt.Time().Before(m.subscription.CreatedAt.Add(-batchCatchUp))
}

// batchChunk is the set of #p values one REQ covers, and the batch connection
// that REQ goes over. A #p value stays in its chunk until its last
// subscription goes, so a change only re-sends the REQ of the chunk it touched.
type batchChunk struct {
	ps    map[string]struct{}
	conn  int
	dirty bool
}

// batchReq is a chunk's REQ on the relay. retired is set before the batch
// closes it on purpose, so any other end (disconnect, CLOSED) is a failure.
type batchReq struct {
	sub     *nostr.Subscription
	retired atomic.Bool
}

func (r *batchReq) retire() {
	r.retired.Store(true)
	r.sub.Unsub()
}

type batchSend struct {
	chunk  *batchChunk
	filter nostr.Filter
}

// subscriptionBatch holds the batchable subscriptions of one relay and kind
// set, and keeps a few REQs open on the relay that together cover all of them.
type subscriptionBatch struct {
	svc      *Service
	relayURL string
	kinds    []int
	key      string
	stop     context.CancelFunc
	backoff  func(attempt int) time.Duration
	unbatch  sync.Once

	mu            sync.Mutex
	byP           map[string]map[string]batchMember // #p value -> subscription uuid -> member
	chunks        []*batchChunk
	chunkOf       map[string]*batchChunk
	maxPubkeys    int
	chunksPerConn int
	rechunked     bool      // chunks were rebuilt after a CLOSED; their REQs catch up
	downSince     time.Time // when the first REQ that is not yet replaced failed
	changed       chan struct{}

	seenMu    sync.Mutex
	seen      map[string]struct{}
	seenPrev  map[string]struct{}
	seenSince time.Time
}

func newSubscriptionBatch(svc *Service, relayURL string, kinds []int) *subscriptionBatch {
	b := &subscriptionBatch{
		svc:           svc,
		relayURL:      relayURL,
		kinds:         kinds,
		byP:           make(map[string]map[string]batchMember),
		chunkOf:       make(map[string]*batchChunk),
		maxPubkeys:    batchMaxPubkeys,
		chunksPerConn: batchMaxReqsPerConn - 1,
		changed:       make(chan struct{}, 1),
		backoff:       batchResubscribeDelay,
	}
	return b
}

func batchKey(relayURL string, kinds []int) string {
	sorted := slices.Clone(kinds)
	sort.Ints(sorted)
	parts := make([]string, len(sorted))
	for i, kind := range sorted {
		parts[i] = strconv.Itoa(kind)
	}
	return nostr.NormalizeURL(relayURL) + "|" + strings.Join(parts, ",")
}

// batchesOn reports whether batchable subscriptions on this relay share
// REQs. Not on the default relays: they only accept filters with a single
// author, #p or #e, and have no REQ cap per connection anyway. Nor on relays
// that refused a batched REQ earlier (see unbatch). A subscription without a
// relay uses the first default relay, like getRelayConnection.
func (svc *Service) batchesOn(relayURL string) bool {
	if relayURL == "" || svc.isDefaultRelay(relayURL) {
		return false
	}
	svc.batchesMutex.Lock()
	defer svc.batchesMutex.Unlock()
	return !svc.unbatchedRelays[nostr.NormalizeURL(relayURL)]
}

// addToBatch adds a batchable subscription to its relay's batch and returns
// the function that removes it again. A batch is created with its first
// subscription and stopped with its last.
func (svc *Service) addToBatch(subscription Subscription, subscriptionType PersistentSubscriptionType) func() {
	relayURL := nostr.NormalizeURL(subscription.RelayUrl)
	key := batchKey(relayURL, *subscription.Kinds)
	member := batchMember{subscription: subscription, subscriptionType: subscriptionType}

	svc.batchesMutex.Lock()
	defer svc.batchesMutex.Unlock()

	if svc.batches == nil {
		svc.batches = make(map[string]*subscriptionBatch)
	}
	batch, exists := svc.batches[key]
	if !exists {
		batch = newSubscriptionBatch(svc, relayURL, *subscription.Kinds)
		batch.key = key
		var ctx context.Context
		ctx, batch.stop = context.WithCancel(svc.Ctx)
		svc.batches[key] = batch
		go batch.run(ctx)
	}
	batch.add(subscription.Uuid, member)

	return func() {
		svc.batchesMutex.Lock()
		defer svc.batchesMutex.Unlock()

		if batch.remove(subscription.Uuid, member.p()) && svc.batches[key] == batch {
			delete(svc.batches, key)
			batch.stop()
		}
	}
}

func (b *subscriptionBatch) add(uuid string, member batchMember) {
	b.mu.Lock()
	defer b.mu.Unlock()

	p := member.p()
	members, exists := b.byP[p]
	if !exists {
		members = make(map[string]batchMember)
		b.byP[p] = members
		chunk := b.chunkWithRoom()
		chunk.ps[p] = struct{}{}
		chunk.dirty = true
		b.chunkOf[p] = chunk
	} else {
		authorKnown := false
		for _, other := range members {
			if other.author() == member.author() {
				authorKnown = true
				break
			}
		}
		// the chunk's REQ doesn't ask for this author yet
		if !authorKnown {
			b.chunkOf[p].dirty = true
		}
	}
	members[uuid] = member
	b.notifyChanged()
}

// remove drops a subscription and reports whether the batch is now empty.
// A chunk keeps asking for the authors of removed subscriptions until it is
// re-sent for another reason; those events simply match nobody.
func (b *subscriptionBatch) remove(uuid, p string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	members := b.byP[p]
	delete(members, uuid)
	if len(members) == 0 {
		delete(b.byP, p)
		if chunk := b.chunkOf[p]; chunk != nil {
			delete(chunk.ps, p)
			chunk.dirty = true
			delete(b.chunkOf, p)
		}
	}
	b.notifyChanged()
	return len(b.byP) == 0
}

// chunkWithRoom returns a chunk that can take another #p value, or a new one
// on the first connection that has fewer than chunksPerConn chunks.
func (b *subscriptionBatch) chunkWithRoom() *batchChunk {
	perConn := make(map[int]int)
	for _, chunk := range b.chunks {
		if len(chunk.ps) < b.maxPubkeys {
			return chunk
		}
		perConn[chunk.conn]++
	}
	conn := 0
	for perConn[conn] >= b.chunksPerConn {
		conn++
	}
	chunk := &batchChunk{ps: make(map[string]struct{}), conn: conn}
	b.chunks = append(b.chunks, chunk)
	return chunk
}

// shrink halves the chunk size after a relay refused a batched REQ, and
// re-chunks every #p value. The new chunks have no REQ yet, so all are sent.
func (b *subscriptionBatch) shrink() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.maxPubkeys <= batchMinPubkeys {
		return
	}
	b.maxPubkeys = max(b.maxPubkeys/2, batchMinPubkeys)

	ps := make([]string, 0, len(b.byP))
	for p := range b.byP {
		ps = append(ps, p)
	}
	sort.Strings(ps)
	b.chunks = nil
	b.chunkOf = make(map[string]*batchChunk)
	for _, p := range ps {
		chunk := b.chunkWithRoom()
		chunk.ps[p] = struct{}{}
		b.chunkOf[p] = chunk
	}
	b.rechunked = true
}

// setReqLimit applies a relay's published cap on concurrent REQs per
// connection, keeping one slot to spare, and spreads the existing chunks over
// connections accordingly. Called before the batch's first REQs go out.
func (b *subscriptionBatch) setReqLimit(maxSubscriptions int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.chunksPerConn = min(max(maxSubscriptions-1, 1), batchMaxChunksPerConn)
	for i, chunk := range b.chunks {
		chunk.conn = i / b.chunksPerConn
	}
}

// closedRefusesBatching reports whether a relay's CLOSED reason refuses the
// batched filter itself (NIP-01 prefixes blocked, restricted, invalid), as
// relays that only accept single-value filters do. Size
// complaints are handled by shrinking instead.
func closedRefusesBatching(reason string) bool {
	if closedForFilterSize(reason) {
		return false
	}
	reason = strings.ToLower(reason)
	return strings.HasPrefix(reason, "blocked:") || strings.HasPrefix(reason, "restricted:") ||
		strings.HasPrefix(reason, "invalid:")
}

// unbatch gives up batching on a relay that refused a batched REQ: the batch
// stops, its subscriptions go back to one REQ each, and later subscriptions on
// the relay aren't batched while this process runs.
func (svc *Service) unbatch(b *subscriptionBatch) {
	b.unbatch.Do(func() {
		svc.batchesMutex.Lock()
		if svc.unbatchedRelays == nil {
			svc.unbatchedRelays = make(map[string]bool)
		}
		svc.unbatchedRelays[b.relayURL] = true
		if svc.batches[b.key] == b {
			delete(svc.batches, b.key)
		}
		svc.batchesMutex.Unlock()
		b.stop()

		b.mu.Lock()
		var members []batchMember
		for _, byUuid := range b.byP {
			for _, member := range byUuid {
				members = append(members, member)
			}
		}
		b.mu.Unlock()

		svc.Logger.WithFields(logrus.Fields{
			"relay_url":     b.relayURL,
			"subscriptions": len(members),
		}).Warn("Relay refused batched subscriptions, subscribing one by one")

		// subscriptions cancelled in the meantime are no longer in subCancelFnMap
		svc.subscriptionsMutex.Lock()
		defer svc.subscriptionsMutex.Unlock()
		for _, member := range members {
			if _, active := svc.subCancelFnMap[member.subscription.Uuid]; active {
				svc.startSubscriptionLocked(member.subscription, member.subscriptionType, resubscribeDelay(1))
			}
		}
	})
}

// closedForFilterSize reports whether a relay's CLOSED reason says the filter
// was too large, as opposed to rate limits, auth or policy, where shrinking
// chunks would only add REQs.
func closedForFilterSize(reason string) bool {
	reason = strings.ToLower(reason)
	return strings.Contains(reason, "too big") || strings.Contains(reason, "too large") ||
		strings.Contains(reason, "too many filter") || strings.Contains(reason, "too long")
}

func (b *subscriptionBatch) notifyChanged() {
	select {
	case b.changed <- struct{}{}:
	default:
	}
}

// plan returns the current chunks and the filters (without since) of those
// whose REQ has to be (re)sent: the changed ones and those without a live
// REQ. Empty chunks are dropped. rechunked reports whether the chunks were
// rebuilt since the last plan.
func (b *subscriptionBatch) plan(live func(*batchChunk) bool) (current map[*batchChunk]struct{}, sends []batchSend, rechunked bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	current = make(map[*batchChunk]struct{})
	chunks := b.chunks[:0]
	for _, chunk := range b.chunks {
		if len(chunk.ps) == 0 {
			continue
		}
		chunks = append(chunks, chunk)
		current[chunk] = struct{}{}
		if !chunk.dirty && live(chunk) {
			continue
		}
		chunk.dirty = false

		ps := make([]string, 0, len(chunk.ps))
		authorSet := make(map[string]struct{})
		for p := range chunk.ps {
			ps = append(ps, p)
			for _, member := range b.byP[p] {
				authorSet[member.author()] = struct{}{}
			}
		}
		sort.Strings(ps)
		authors := make([]string, 0, len(authorSet))
		for author := range authorSet {
			authors = append(authors, author)
		}
		sort.Strings(authors)

		sends = append(sends, batchSend{chunk: chunk, filter: nostr.Filter{
			Kinds:   b.kinds,
			Authors: authors,
			Tags:    nostr.TagMap{"p": ps},
		}})
	}
	b.chunks = chunks
	rechunked = b.rechunked
	b.rechunked = false
	return current, sends, rechunked
}

// redo marks chunks whose planned REQ was not sent (a connect or subscribe
// failed first), so the next plan sends them even if an older REQ is live.
func (b *subscriptionBatch) redo(sends []batchSend) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, send := range sends {
		send.chunk.dirty = true
	}
}

// markDown records when the first REQ failed, so its replacement can catch
// up from there.
func (b *subscriptionBatch) markDown(at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.downSince.IsZero() {
		b.downSince = at
	}
}

func (b *subscriptionBatch) catchUpFrom(everLive bool, now time.Time) time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !everLive {
		return now.Add(-batchStartupCatchUp)
	}
	if !b.downSince.IsZero() {
		return b.downSince.Add(-batchCatchUp)
	}
	return now
}

func (b *subscriptionBatch) clearDown() {
	b.mu.Lock()
	b.downSince = time.Time{}
	b.mu.Unlock()
}

// run keeps one REQ per chunk open on the relay until ctx is done: re-sending
// a chunk's REQ after it changed, and every failed REQ with the usual jittered
// backoff. REQs go over the batch's own connections, which live as long as
// ctx, at most chunksPerConn each. Unlike a single subscription it never gives
// up on an unreachable relay: after batchMaxConnectFailures it retries every
// batchUnreachableRetry, and its subscriptions resume once the relay is back.
func (b *subscriptionBatch) run(ctx context.Context) {
	reqs := make(map[*batchChunk]*batchReq)
	conns := make(map[int]*nostr.Relay)
	defer func() {
		for _, req := range reqs {
			req.retire()
		}
		for _, conn := range conns {
			conn.Close()
		}
	}()

	// learn the relay's REQ cap before the first REQs go out
	if info, err := nip11.Fetch(ctx, b.relayURL); err == nil && info.Limitation != nil && info.Limitation.MaxSubscriptions > 0 {
		b.setReqLimit(info.Limitation.MaxSubscriptions)
	}

	failed := make(chan struct{}, 1)
	attempt := 0
	connectFailures := 0
	everLive := false
	catchUpAll := false                  // the chunks were rebuilt after a CLOSED
	missed := make(map[*batchChunk]bool) // chunks whose REQ ended before being replaced
	var subscribedAt time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-b.changed:
		case <-failed:
			if time.Since(subscribedAt) >= resubscribeStableAfter {
				attempt = 0
			}
			attempt++
			if !sleepCtx(ctx, b.backoff(attempt)) {
				return
			}
		}

		if !sleepCtx(ctx, batchDebounce) {
			return
		}

		current, sends, rechunked := b.plan(func(chunk *batchChunk) bool {
			req := reqs[chunk]
			return req != nil && req.sub.Context.Err() == nil
		})
		catchUpAll = catchUpAll || rechunked
		usedConns := make(map[int]struct{})
		for chunk := range current {
			usedConns[chunk.conn] = struct{}{}
		}
		for chunk, req := range reqs {
			_, stillThere := current[chunk]
			ended := req.sub.Context.Err() != nil
			// REQs that ended (disconnect, CLOSED) go before their replacements are
			// sent, so a capped relay doesn't count them twice
			if !stillThere || ended {
				req.retire()
				delete(reqs, chunk)
			}
			if stillThere && ended {
				missed[chunk] = true
			}
		}
		for chunk := range missed {
			if _, stillThere := current[chunk]; !stillThere {
				delete(missed, chunk)
			}
		}
		for i, conn := range conns {
			if _, used := usedConns[i]; !used {
				conn.Close()
				delete(conns, i)
			}
		}
		if len(sends) == 0 {
			continue
		}

		now := time.Now()
		catchUp := nostr.Timestamp(b.catchUpFrom(everLive, now).Unix())
		sinceNow := nostr.Timestamp(now.Unix())

		sent := 0
		for i, send := range sends {
			conn := conns[send.chunk.conn]
			if conn == nil || !conn.IsConnected() {
				var err error
				conn, err = b.svc.connectToRelay(ctx, b.relayURL)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					b.redo(sends[i:])
					connectFailures++
					b.svc.Logger.WithError(err).WithFields(logrus.Fields{
						"relay_url": b.relayURL,
						"attempt":   connectFailures,
					}).Error("Failed to connect to relay for batched subscriptions")
					if connectFailures >= batchMaxConnectFailures && !b.svc.isDefaultRelay(b.relayURL) {
						connectFailures = 0
						if !sleepCtx(ctx, batchUnreachableRetry) {
							return
						}
					}
					b.signal(failed)
					break
				}
				connectFailures = 0
				conns[send.chunk.conn] = conn
			}

			// only REQs replacing ones that ended catch up on what they may have
			// missed (all of them on a fresh start or after re-chunking); a chunk
			// whose REQ is still open, or that is new, starts from now
			filter := send.filter
			filter.Since = &sinceNow
			if !everLive || catchUpAll || missed[send.chunk] {
				filter.Since = &catchUp
			}

			sub, err := conn.Subscribe(ctx, nostr.Filters{filter})
			if err != nil {
				b.redo(sends[i:])
				b.svc.Logger.WithError(err).WithFields(logrus.Fields{
					"relay_url": b.relayURL,
				}).Error("Failed to subscribe batched filter")
				b.signal(failed)
				break
			}
			req := &batchReq{sub: sub}
			go b.forward(ctx, req, failed)
			// the new REQ is open before the old one closes, so nothing is missed in
			// between; the overlap is deduplicated in dispatch, and chunksPerConn
			// leaves a slot for it
			if old := reqs[send.chunk]; old != nil {
				old.retire()
			}
			reqs[send.chunk] = req
			delete(missed, send.chunk)
			sent++
		}
		if sent == len(sends) {
			everLive = true
			catchUpAll = false
			subscribedAt = now
			b.clearDown()
		}

		b.svc.Logger.WithFields(logrus.Fields{
			"relay_url":   b.relayURL,
			"kinds":       b.kinds,
			"sent":        sent,
			"reqs":        len(reqs),
			"connections": len(conns),
		}).Info("Batched subscriptions subscribed")
	}
}

// forward dispatches a REQ's events to the subscriptions, and reports on
// failed when the REQ ends without the batch having retired it.
func (b *subscriptionBatch) forward(ctx context.Context, req *batchReq, failed chan struct{}) {
	sub := req.sub
	for {
		select {
		case event, ok := <-sub.Events:
			if !ok {
				b.ended(req, failed)
				return
			}
			b.dispatch(event)
		case reason := <-sub.ClosedReason:
			if !req.retired.Load() {
				b.svc.Logger.WithFields(logrus.Fields{
					"relay_url": b.relayURL,
					"reason":    reason,
				}).Error("Relay closed batched subscription")
				if closedRefusesBatching(reason) {
					go b.svc.unbatch(b)
					return
				}
				if closedForFilterSize(reason) {
					b.shrink()
				}
			}
			b.ended(req, failed)
			return
		case <-sub.Context.Done():
			b.ended(req, failed)
			return
		case <-ctx.Done():
			return
		}
	}
}

func (b *subscriptionBatch) ended(req *batchReq, failed chan struct{}) {
	if !req.retired.Load() {
		b.markDown(time.Now())
		b.signal(failed)
	}
}

func (b *subscriptionBatch) signal(failed chan struct{}) {
	select {
	case failed <- struct{}{}:
	default:
	}
}

func (b *subscriptionBatch) dispatch(event *nostr.Event) {
	if b.seenBefore(event.ID) {
		return
	}

	for _, member := range b.membersFor(event) {
		subscription := member.subscription
		b.svc.handleSubscriptionEvent(event, &subscription, member.subscriptionType)
	}
}

// membersFor returns the subscriptions an event belongs to.
func (b *subscriptionBatch) membersFor(event *nostr.Event) []batchMember {
	b.mu.Lock()
	defer b.mu.Unlock()

	var matches []batchMember
	seenPs := make(map[string]struct{})
	for _, tag := range event.Tags {
		if len(tag) < 2 || tag[0] != "p" {
			continue
		}
		if _, dup := seenPs[tag[1]]; dup {
			continue
		}
		seenPs[tag[1]] = struct{}{}
		// a REQ combines authors and #p values, so it also lets through pairs
		// nobody subscribed to; those match no member here
		for _, member := range b.byP[tag[1]] {
			if member.matches(event) {
				matches = append(matches, member)
			}
		}
	}
	return matches
}

// seenBefore reports whether an event id came in within the last
// batchSeenTTL. It keeps two generations of ids and drops the older one each
// period, so checks stay O(1) and memory bounded by the event rate.
func (b *subscriptionBatch) seenBefore(eventID string) bool {
	b.seenMu.Lock()
	defer b.seenMu.Unlock()

	now := time.Now()
	if b.seen == nil || now.Sub(b.seenSince) > batchSeenTTL {
		b.seenPrev = b.seen
		b.seen = make(map[string]struct{})
		b.seenSince = now
	}
	if _, ok := b.seen[eventID]; ok {
		return true
	}
	if _, ok := b.seenPrev[eventID]; ok {
		return true
	}
	b.seen[eventID] = struct{}{}
	return false
}
