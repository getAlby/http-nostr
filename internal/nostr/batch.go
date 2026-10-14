package nostr

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/getAlby/go-nostr"
	"github.com/sirupsen/logrus"
)

const (
	// Most #p values one batched REQ asks for. Relays cap concurrent REQs per
	// connection (some at ~100), so subscriptions on the same relay share a few
	// REQs instead of holding one each. Kept moderate so a filter stays well
	// within relays' message size limits.
	batchMaxPubkeys = 200
	// Membership changes (new subscriptions, cancels, the restore at startup)
	// are collected for this long before the REQs are rebuilt, so a burst of
	// changes costs a single rebuild.
	batchDebounce = 2 * time.Second
	// How long event ids are remembered, to drop the duplicates that arrive
	// while the old and new REQs overlap during a rebuild.
	batchSeenTTL = 10 * time.Minute
)

// batchable reports whether a subscription is a plain "events by this author
// tagged with this pubkey" filter on ephemeral kinds, which is what NWC
// notification subscriptions look like. Those share REQs with the others on
// the same relay. Relays don't store ephemeral events, so dropping the
// subscription's own since doesn't change what it receives.
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
	if event.PubKey != m.author() || !slices.Contains(*m.subscription.Kinds, event.Kind) {
		return false
	}
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "p" && tag[1] == m.p() {
			return true
		}
	}
	return false
}

// subscriptionBatch holds the batchable subscriptions of one relay and kind
// set, and keeps a few REQs open on the relay that together cover all of them.
type subscriptionBatch struct {
	svc      *Service
	relayURL string
	kinds    []int

	mu      sync.Mutex
	members map[string]batchMember // by subscription uuid
	changed chan struct{}

	seenMu sync.Mutex
	seen   map[string]time.Time
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
// the function that removes it again.
func (svc *Service) addToBatch(subscription Subscription, subscriptionType PersistentSubscriptionType) func() {
	key := batchKey(subscription.RelayUrl, *subscription.Kinds)

	svc.batchesMutex.Lock()
	if svc.batches == nil {
		svc.batches = make(map[string]*subscriptionBatch)
	}
	batch, exists := svc.batches[key]
	if !exists {
		batch = &subscriptionBatch{
			svc:      svc,
			relayURL: subscription.RelayUrl,
			kinds:    *subscription.Kinds,
			members:  make(map[string]batchMember),
			changed:  make(chan struct{}, 1),
			seen:     make(map[string]time.Time),
		}
		svc.batches[key] = batch
		go batch.run(svc.Ctx)
	}
	svc.batchesMutex.Unlock()

	batch.mu.Lock()
	batch.members[subscription.Uuid] = batchMember{subscription: subscription, subscriptionType: subscriptionType}
	batch.mu.Unlock()
	batch.notifyChanged()

	return func() {
		batch.mu.Lock()
		delete(batch.members, subscription.Uuid)
		batch.mu.Unlock()
		batch.notifyChanged()
	}
}

func (b *subscriptionBatch) notifyChanged() {
	select {
	case b.changed <- struct{}{}:
	default:
	}
}

// filters returns the REQ filters covering all current members: their #p
// values in chunks of batchMaxPubkeys, each with the authors of the members
// in that chunk. Members are matched exactly when events are dispatched, so
// an event that only matches the combined filter reaches nobody.
func (b *subscriptionBatch) filters() nostr.Filters {
	b.mu.Lock()
	authorsByP := make(map[string]map[string]struct{})
	for _, member := range b.members {
		if authorsByP[member.p()] == nil {
			authorsByP[member.p()] = make(map[string]struct{})
		}
		authorsByP[member.p()][member.author()] = struct{}{}
	}
	b.mu.Unlock()

	ps := make([]string, 0, len(authorsByP))
	for p := range authorsByP {
		ps = append(ps, p)
	}
	sort.Strings(ps)

	since := nostr.Now()
	var filters nostr.Filters
	for start := 0; start < len(ps); start += batchMaxPubkeys {
		chunk := ps[start:min(start+batchMaxPubkeys, len(ps))]

		authorSet := make(map[string]struct{})
		for _, p := range chunk {
			for author := range authorsByP[p] {
				authorSet[author] = struct{}{}
			}
		}
		authors := make([]string, 0, len(authorSet))
		for author := range authorSet {
			authors = append(authors, author)
		}
		sort.Strings(authors)

		filters = append(filters, nostr.Filter{
			Kinds:   b.kinds,
			Authors: authors,
			Tags:    nostr.TagMap{"p": chunk},
			Since:   &since,
		})
	}
	return filters
}

// run keeps the batch's REQs in line with its members until ctx is done:
// rebuilding them after membership changes, and resubscribing with the usual
// jittered backoff when the relay drops them. Unlike a single subscription it
// never gives up on an unreachable relay - it is one retry loop per relay, not
// per subscription, and its members resume as soon as the relay is back.
func (b *subscriptionBatch) run(ctx context.Context) {
	var subs []*nostr.Subscription
	defer func() {
		for _, sub := range subs {
			sub.Unsub()
		}
	}()

	failed := make(chan struct{}, 1)
	attempt := 0
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
			if !waitBeforeResubscribe(ctx, attempt) {
				return
			}
		}

		if !sleepCtx(ctx, batchDebounce) {
			return
		}
		select {
		case <-b.changed:
		default:
		}

		filters := b.filters()
		if len(filters) == 0 {
			for _, sub := range subs {
				sub.Unsub()
			}
			subs = nil
			continue
		}

		relay, err := b.svc.getRelayConnection(ctx, b.relayURL)
		if err != nil {
			b.svc.Logger.WithError(err).WithFields(logrus.Fields{
				"relay_url": b.relayURL,
			}).Error("Failed to connect to relay for batched subscriptions")
			b.signal(failed)
			continue
		}

		newSubs := make([]*nostr.Subscription, 0, len(filters))
		for _, filter := range filters {
			sub, err := relay.Subscribe(ctx, nostr.Filters{filter})
			if err != nil {
				b.svc.Logger.WithError(err).WithFields(logrus.Fields{
					"relay_url": b.relayURL,
				}).Error("Failed to subscribe batched filter")
				break
			}
			newSubs = append(newSubs, sub)
		}
		if len(newSubs) < len(filters) {
			for _, sub := range newSubs {
				sub.Unsub()
			}
			b.signal(failed)
			continue
		}

		// the new REQs are open before the old ones close, so nothing is missed
		// in between; the overlap is deduplicated in dispatch
		for _, sub := range subs {
			sub.Unsub()
		}
		subs = newSubs
		subscribedAt = time.Now()
		for _, sub := range subs {
			go b.forward(ctx, sub, failed)
		}

		b.svc.Logger.WithFields(logrus.Fields{
			"relay_url":     b.relayURL,
			"kinds":         b.kinds,
			"subscriptions": b.size(),
			"reqs":          len(subs),
		}).Info("Batched subscriptions subscribed")
	}
}

// forward dispatches a REQ's events to the members, and reports on failed when
// the relay ends it (disconnect or CLOSED) rather than a rebuild.
func (b *subscriptionBatch) forward(ctx context.Context, sub *nostr.Subscription, failed chan struct{}) {
	for {
		select {
		case event, ok := <-sub.Events:
			if !ok {
				if sub.Relay.Context().Err() != nil {
					b.signal(failed)
				}
				return
			}
			b.dispatch(event)
		case reason := <-sub.ClosedReason:
			b.svc.Logger.WithFields(logrus.Fields{
				"relay_url": b.relayURL,
				"reason":    reason,
			}).Error("Relay closed batched subscription")
			b.signal(failed)
			return
		case <-sub.Relay.Context().Done():
			b.signal(failed)
			return
		case <-sub.Context.Done():
			return
		case <-ctx.Done():
			return
		}
	}
}

func (b *subscriptionBatch) signal(failed chan struct{}) {
	select {
	case failed <- struct{}{}:
	default:
	}
}

func (b *subscriptionBatch) size() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.members)
}

func (b *subscriptionBatch) dispatch(event *nostr.Event) {
	if b.seenBefore(event.ID) {
		return
	}

	b.mu.Lock()
	var matches []batchMember
	for _, member := range b.members {
		if member.matches(event) {
			matches = append(matches, member)
		}
	}
	b.mu.Unlock()

	for _, member := range matches {
		subscription := member.subscription
		b.svc.handleSubscriptionEvent(event, &subscription, member.subscriptionType)
	}
}

func (b *subscriptionBatch) seenBefore(eventID string) bool {
	b.seenMu.Lock()
	defer b.seenMu.Unlock()

	now := time.Now()
	if _, ok := b.seen[eventID]; ok {
		return true
	}
	if len(b.seen) >= 1000 {
		for id, at := range b.seen {
			if now.Sub(at) > batchSeenTTL {
				delete(b.seen, id)
			}
		}
	}
	b.seen[eventID] = now
	return false
}
