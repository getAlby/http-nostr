package nostr

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getAlby/go-nostr"
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
	// Most batched REQs on one connection. Relays cap concurrent REQs per
	// connection (nostream at 10, Primal's strfry at 20, Rizful's at 100) and
	// refuse the rest with a NOTICE that names no subscription, so a refused
	// REQ can't be detected. Batches therefore open their own connections and
	// stay under the lowest common cap.
	batchMaxReqsPerConn = 10
	// Membership changes (new subscriptions, cancels, the restore at startup)
	// are collected for this long before REQs are re-sent, so a burst of
	// changes costs a single round.
	batchDebounce = 2 * time.Second
	// How long event ids are remembered, to drop the duplicates that arrive
	// while REQs overlap or catch up.
	batchSeenTTL = 10 * time.Minute
	// How long to wait before trying a relay again once relayConnectWithBackoff
	// gave up on it, so dead relays cost little without being dropped for good.
	batchUnreachableRetry = 30 * time.Minute
	// A REQ that replaces one that failed asks for events from this long before
	// the failure, so nothing published in between is missed.
	batchCatchUp = 30 * time.Second
	// A batch's first REQs ask for events from this long ago, to pick up what
	// was published while this process (re)started. strfry keeps ephemeral
	// events for 5 minutes; our own relay stores NWC events too.
	batchStartupCatchUp = 5 * time.Minute
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

func (m *batchMember) matches(event *nostr.Event) bool {
	return event.PubKey == m.author() && slices.Contains(*m.subscription.Kinds, event.Kind)
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
	stop     context.CancelFunc
	backoff  func(attempt int) time.Duration

	mu         sync.Mutex
	byP        map[string]map[string]batchMember // #p value -> subscription uuid -> member
	chunks     []*batchChunk
	chunkOf    map[string]*batchChunk
	maxPubkeys int
	downSince  time.Time // when the first REQ that is not yet replaced failed
	changed    chan struct{}

	seenMu    sync.Mutex
	seen      map[string]struct{}
	seenPrev  map[string]struct{}
	seenSince time.Time
}

func newSubscriptionBatch(svc *Service, relayURL string, kinds []int) *subscriptionBatch {
	return &subscriptionBatch{
		svc:        svc,
		relayURL:   relayURL,
		kinds:      kinds,
		byP:        make(map[string]map[string]batchMember),
		chunkOf:    make(map[string]*batchChunk),
		maxPubkeys: batchMaxPubkeys,
		changed:    make(chan struct{}, 1),
		backoff:    batchResubscribeDelay,
	}
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

// addToBatch adds a batchable subscription to its relay's batch and returns
// the function that removes it again. A batch is created with its first
// subscription and stopped with its last.
func (svc *Service) addToBatch(subscription Subscription, subscriptionType PersistentSubscriptionType) func() {
	key := batchKey(subscription.RelayUrl, *subscription.Kinds)
	member := batchMember{subscription: subscription, subscriptionType: subscriptionType}

	svc.batchesMutex.Lock()
	defer svc.batchesMutex.Unlock()

	if svc.batches == nil {
		svc.batches = make(map[string]*subscriptionBatch)
	}
	batch, exists := svc.batches[key]
	if !exists {
		batch = newSubscriptionBatch(svc, subscription.RelayUrl, *subscription.Kinds)
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
// on the first connection that has fewer than batchMaxReqsPerConn chunks.
func (b *subscriptionBatch) chunkWithRoom() *batchChunk {
	perConn := make(map[int]int)
	for _, chunk := range b.chunks {
		if len(chunk.ps) < b.maxPubkeys {
			return chunk
		}
		perConn[chunk.conn]++
	}
	conn := 0
	for perConn[conn] >= batchMaxReqsPerConn {
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
}

func (b *subscriptionBatch) notifyChanged() {
	select {
	case b.changed <- struct{}{}:
	default:
	}
}

// plan returns the current chunks and the filters (without since) of those
// whose REQ has to be (re)sent: the changed ones and those without a live
// REQ. Empty chunks are dropped.
func (b *subscriptionBatch) plan(live func(*batchChunk) bool) (current map[*batchChunk]struct{}, sends []batchSend) {
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
	return current, sends
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
// backoff. REQs go over the batch's own connections, at most
// batchMaxReqsPerConn each. Unlike a single subscription it never gives up on
// an unreachable relay: it retries every batchUnreachableRetry, and its
// subscriptions resume once the relay is back.
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

	failed := make(chan struct{}, 1)
	attempt := 0
	everLive := false
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

		current, sends := b.plan(func(chunk *batchChunk) bool {
			req := reqs[chunk]
			return req != nil && req.sub.Context.Err() == nil
		})
		usedConns := make(map[int]struct{})
		for chunk := range current {
			usedConns[chunk.conn] = struct{}{}
		}
		for chunk, req := range reqs {
			// REQs that ended (disconnect, CLOSED) go before their replacements are
			// sent, so a capped relay doesn't count them twice
			if _, stillThere := current[chunk]; !stillThere || req.sub.Context.Err() != nil {
				req.retire()
				delete(reqs, chunk)
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
		for _, send := range sends {
			conn := conns[send.chunk.conn]
			if conn == nil || !conn.IsConnected() {
				var err error
				conn, err = b.svc.relayConnectWithBackoff(b.relayURL)
				if err != nil {
					b.svc.Logger.WithError(err).WithFields(logrus.Fields{
						"relay_url": b.relayURL,
					}).Error("Failed to connect to relay for batched subscriptions")
					if errors.Is(err, ErrRelayUnreachable) && !sleepCtx(ctx, batchUnreachableRetry) {
						return
					}
					b.signal(failed)
					break
				}
				conns[send.chunk.conn] = conn
			}

			// a chunk whose REQ is still open only needs events from now on; the
			// others catch up on what they may have missed
			filter := send.filter
			filter.Since = &sinceNow
			if reqs[send.chunk] == nil {
				filter.Since = &catchUp
			}

			sub, err := conn.Subscribe(ctx, nostr.Filters{filter})
			if err != nil {
				b.svc.Logger.WithError(err).WithFields(logrus.Fields{
					"relay_url": b.relayURL,
				}).Error("Failed to subscribe batched filter")
				b.signal(failed)
				break
			}
			req := &batchReq{sub: sub}
			go b.forward(ctx, req, failed)
			// the new REQ is open before the old one closes, so nothing is missed in
			// between; the overlap is deduplicated in dispatch
			if old := reqs[send.chunk]; old != nil {
				old.retire()
			}
			reqs[send.chunk] = req
			sent++
		}
		if sent == len(sends) {
			everLive = true
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
				b.shrink()
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
