package nostr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	gostr "github.com/getAlby/go-nostr"
	"github.com/test-go/testify/assert"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func notificationSubscription(relayURL, walletPubkey, connPubkey string) Subscription {
	return subscriptionFromFilter(relayURL, notificationFilter(walletPubkey, connPubkey, ""))
}

func TestBatchable(t *testing.T) {
	sub := notificationSubscription("wss://relay.example", "wallet", "conn")
	assert.True(t, batchable(&sub), "NWC notification subscriptions are batched")

	limit := sub
	limit.Limit = 10
	assert.False(t, batchable(&limit), "a limit asks for stored events")

	twoAuthors := sub
	twoAuthors.Authors = &[]string{"a", "b"}
	assert.False(t, batchable(&twoAuthors))

	storedKind := sub
	storedKind.Kinds = &[]int{1}
	assert.False(t, batchable(&storedKind), "since matters for kinds relays store")

	otherTag := sub
	otherTag.Tags = &gostr.TagMap{"e": []string{"x"}}
	assert.False(t, batchable(&otherTag))
}

func addMembers(batch *subscriptionBatch, from, to int) []Subscription {
	var subs []Subscription
	for i := from; i < to; i++ {
		sub := notificationSubscription("wss://relay.example", fmt.Sprintf("wallet%d", i%2), fmt.Sprintf("conn%04d", i))
		batch.add(sub.Uuid, batchMember{subscription: sub, subscriptionType: WebhookSubscriptionType})
		subs = append(subs, sub)
	}
	return subs
}

func noLiveReqs(*batchChunk) bool  { return false }
func allLiveReqs(*batchChunk) bool { return true }

func TestBatchPlanChunksPubkeys(t *testing.T) {
	batch := newSubscriptionBatch(nil, "wss://relay.example", []int{LEGACY_NIP_47_NOTIFICATION_KIND})
	addMembers(batch, 0, 450)
	// a duplicate subscription for the same connection shares its #p value
	dup := notificationSubscription("wss://relay.example", "wallet0", "conn0000")
	batch.add(dup.Uuid, batchMember{subscription: dup, subscriptionType: WebhookSubscriptionType})

	_, sends, _ := batch.plan(noLiveReqs)

	assert.Len(t, sends, 3)
	total := 0
	for _, send := range sends {
		assert.True(t, len(send.filter.Tags["p"]) <= batchMaxPubkeys)
		assert.Equal(t, []string{"wallet0", "wallet1"}, send.filter.Authors)
		total += len(send.filter.Tags["p"])
	}
	assert.Equal(t, 450, total)
}

func TestBatchPlanOnlyResendsChangedChunks(t *testing.T) {
	batch := newSubscriptionBatch(nil, "wss://relay.example", []int{LEGACY_NIP_47_NOTIFICATION_KIND})
	subs := addMembers(batch, 0, 350)
	batch.plan(noLiveReqs)

	_, sends, _ := batch.plan(allLiveReqs)
	assert.Len(t, sends, 0, "nothing changed")

	addMembers(batch, 350, 351)
	_, sends, _ = batch.plan(allLiveReqs)
	assert.Len(t, sends, 1, "a new #p value only touches the chunk with room")

	batch.remove(subs[0].Uuid, "conn0000")
	_, sends, _ = batch.plan(allLiveReqs)
	assert.Len(t, sends, 1, "a removed #p value only touches its own chunk")

	current, sends, _ := batch.plan(noLiveReqs)
	assert.Len(t, sends, len(current), "chunks without a live REQ are all sent again")
}

func TestBatchPlanResendsChunksAfterFailedSend(t *testing.T) {
	batch := newSubscriptionBatch(nil, "wss://relay.example", []int{LEGACY_NIP_47_NOTIFICATION_KIND})
	addMembers(batch, 0, 450)
	batch.plan(noLiveReqs)

	// new #p values land in the last chunk; the send fails before it goes out,
	// while that chunk's old REQ is still live
	addMembers(batch, 450, 451)
	_, sends, _ := batch.plan(allLiveReqs)
	assert.Len(t, sends, 1)
	batch.redo(sends)

	_, sends, _ = batch.plan(allLiveReqs)
	assert.Len(t, sends, 1, "the unsent chunk is planned again")
}

func TestBatchSpreadsChunksOverConnections(t *testing.T) {
	batch := newSubscriptionBatch(nil, "wss://relay.example", []int{LEGACY_NIP_47_NOTIFICATION_KIND})
	batch.maxPubkeys = 1
	addMembers(batch, 0, 25)

	perConn := make(map[int]int)
	current, _, _ := batch.plan(noLiveReqs)
	for chunk := range current {
		perConn[chunk.conn]++
	}
	// one slot per connection stays free for a re-sent chunk's overlapping REQ
	assert.Equal(t, map[int]int{0: 9, 1: 9, 2: 7}, perConn)
}

func TestBatchSetReqLimitFromNIP11(t *testing.T) {
	batch := newSubscriptionBatch(nil, "wss://relay.example", []int{LEGACY_NIP_47_NOTIFICATION_KIND})
	batch.maxPubkeys = 1
	addMembers(batch, 0, 25)

	chunksPerConn := func() map[int]int {
		perConn := make(map[int]int)
		current, _, _ := batch.plan(noLiveReqs)
		for chunk := range current {
			perConn[chunk.conn]++
		}
		return perConn
	}

	batch.setReqLimit(5)
	assert.Equal(t, map[int]int{0: 4, 1: 4, 2: 4, 3: 4, 4: 4, 5: 4, 6: 1}, chunksPerConn())

	batch.setReqLimit(100_000)
	assert.Equal(t, map[int]int{0: 25}, chunksPerConn(), "a high cap is bounded by batchMaxChunksPerConn")
}

func TestClosedForFilterSize(t *testing.T) {
	assert.True(t, closedForFilterSize("ERROR: bad req: arr too big"))         // strfry
	assert.True(t, closedForFilterSize("ERROR: total filter items too large")) // strfry
	assert.True(t, closedForFilterSize("Too many filter values: 2600"))        // nostream
	assert.False(t, closedForFilterSize("rate-limited: slow down"))
	assert.False(t, closedForFilterSize("auth-required: please authenticate"))
	assert.False(t, closedForFilterSize("restricted: not allowed"))
}

func TestBatchesOnSkipsOurRelaysAndRefusingOnes(t *testing.T) {
	svc := newTestService(context.Background())

	assert.False(t, svc.batchesOn(testDefaultRelay), "default relays only accept single-value filters")
	assert.False(t, svc.batchesOn(""), "no relay means the default relay")
	assert.True(t, svc.batchesOn("wss://relay.example"))

	svc.batchesMutex.Lock()
	svc.unbatchedRelays = map[string]bool{gostr.NormalizeURL("wss://relay.example"): true}
	svc.batchesMutex.Unlock()
	assert.False(t, svc.batchesOn("wss://relay.example"), "a relay that refused batched REQs")
}

func TestClosedRefusesBatching(t *testing.T) {
	assert.True(t, closedRefusesBatching("blocked: Request rejected"))
	assert.True(t, closedRefusesBatching("restricted: not allowed"))
	assert.True(t, closedRefusesBatching("invalid: too many authors"))
	assert.False(t, closedRefusesBatching("invalid: filter too large"), "size is handled by shrinking")
	assert.False(t, closedRefusesBatching("rate-limited: slow down"))
	assert.False(t, closedRefusesBatching("auth-required: please authenticate"))
}

func TestBatchRunStopsWhileRelayUnreachable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	svc := newTestService(context.Background())
	batch := newSubscriptionBatch(svc, testUnreachableRelay, []int{LEGACY_NIP_47_NOTIFICATION_KIND})
	addMembers(batch, 0, 1)

	done := make(chan struct{})
	go func() {
		defer close(done)
		batch.run(ctx)
	}()
	time.Sleep(batchDebounce + 500*time.Millisecond) // connecting is under way and failing
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a stopped batch kept trying to connect")
	}
}

func TestBatchShrinkHalvesChunks(t *testing.T) {
	batch := newSubscriptionBatch(nil, "wss://relay.example", []int{LEGACY_NIP_47_NOTIFICATION_KIND})
	addMembers(batch, 0, 200)

	batch.shrink()
	current, _, _ := batch.plan(noLiveReqs)
	assert.Len(t, current, 2)

	for i := 0; i < 5; i++ {
		batch.shrink()
	}
	current, _, _ = batch.plan(noLiveReqs)
	assert.Len(t, current, 200/batchMinPubkeys, "chunks don't shrink below the floor")
}

func TestBatchCatchUpFrom(t *testing.T) {
	batch := newSubscriptionBatch(nil, "wss://relay.example", nil)
	now := time.Now()

	assert.Equal(t, now.Add(-batchStartupCatchUp), batch.catchUpFrom(false, now), "a fresh start catches up on the restart")
	assert.Equal(t, now, batch.catchUpFrom(true, now), "nothing failed, nothing to catch up on")

	down := now.Add(-time.Minute)
	batch.markDown(down)
	batch.markDown(now) // only the first failure counts
	assert.Equal(t, down.Add(-batchCatchUp), batch.catchUpFrom(true, now))

	batch.clearDown()
	assert.Equal(t, now, batch.catchUpFrom(true, now))
}

func TestBatchMembersForMatchesExactly(t *testing.T) {
	batch := newSubscriptionBatch(nil, "wss://relay.example", []int{LEGACY_NIP_47_NOTIFICATION_KIND})
	sub := notificationSubscription("wss://relay.example", "wallet1", "conn1")
	batch.add(sub.Uuid, batchMember{subscription: sub})
	other := notificationSubscription("wss://relay.example", "wallet2", "conn2")
	batch.add(other.Uuid, batchMember{subscription: other})

	event := func(author, p string) *gostr.Event {
		return &gostr.Event{PubKey: author, Kind: LEGACY_NIP_47_NOTIFICATION_KIND, Tags: gostr.Tags{{"p", p}}}
	}

	assert.Len(t, batch.membersFor(event("wallet1", "conn1")), 1, "events of restored subscriptions match regardless of age")
	// a REQ combines authors and #p values, so it also lets through pairs
	// nobody subscribed to; those must reach nobody
	assert.Len(t, batch.membersFor(event("wallet2", "conn1")), 0)
	assert.Len(t, batch.membersFor(event("wallet1", "conn2")), 0)
}

func TestBatchMemberIgnoresEventsFromBeforeItExisted(t *testing.T) {
	sub := notificationSubscription("wss://relay.example", "wallet1", "conn1")
	sub.CreatedAt = time.Now()
	member := batchMember{subscription: sub}
	event := func(age time.Duration) *gostr.Event {
		return &gostr.Event{PubKey: "wallet1", Kind: LEGACY_NIP_47_NOTIFICATION_KIND, CreatedAt: gostr.Timestamp(time.Now().Add(-age).Unix())}
	}

	assert.True(t, member.matches(event(0)))
	assert.True(t, member.matches(event(10*time.Second)), "clock skew is tolerated")
	assert.False(t, member.matches(event(2*time.Minute)), "a catch-up event from before the subscription")
}

func TestBatchDropsDuplicateEvents(t *testing.T) {
	batch := newSubscriptionBatch(nil, "wss://relay.example", nil)

	assert.False(t, batch.seenBefore("event1"))
	assert.True(t, batch.seenBefore("event1"))
	assert.False(t, batch.seenBefore("event2"))

	// ids from the previous period are still remembered
	batch.seenSince = time.Now().Add(-batchSeenTTL - time.Second)
	assert.True(t, batch.seenBefore("event1"))
}

// fakeRelay is a minimal relay that refuses more than maxReqs concurrent REQs
// per connection with a NOTICE (like strfry and nostream), stores every
// published event (like strfry does with ephemeral events for 5 minutes, and
// others), returns stored events matching a REQ before EOSE, and pushes
// new events to the open REQs whose filters match.
type fakeRelay struct {
	maxReqs      int
	publishNIP11 bool // advertise maxReqs as limitation.max_subscriptions
	// like some relays (incl. the default ones): refuse filters without a
	// single author, #p or #e with CLOSED "blocked: Request rejected"
	singleValueFilters bool
	rejectConnect      atomic.Bool

	mu      sync.Mutex
	conns   map[*websocket.Conn]map[string]gostr.Filters
	stored  []*gostr.Event
	refused int
}

func newFakeRelay(maxReqs int) (*fakeRelay, *httptest.Server) {
	relay := &fakeRelay{maxReqs: maxReqs, conns: make(map[*websocket.Conn]map[string]gostr.Filters)}
	return relay, httptest.NewServer(http.HandlerFunc(relay.serve))
}

func (r *fakeRelay) serve(w http.ResponseWriter, req *http.Request) {
	if req.Header.Get("Accept") == "application/nostr+json" {
		if !r.publishNIP11 {
			http.NotFound(w, req)
			return
		}
		fmt.Fprintf(w, `{"limitation":{"max_subscriptions":%d}}`, r.maxReqs)
		return
	}
	if r.rejectConnect.Load() {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	conn, err := websocket.Accept(w, req, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(1 << 20)
	r.mu.Lock()
	r.conns[conn] = make(map[string]gostr.Filters)
	r.mu.Unlock()

	ctx := req.Context()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			r.mu.Lock()
			delete(r.conns, conn)
			r.mu.Unlock()
			return
		}
		var msg []json.RawMessage
		if json.Unmarshal(data, &msg) != nil || len(msg) < 2 {
			continue
		}
		var typ, subID string
		json.Unmarshal(msg[0], &typ)
		json.Unmarshal(msg[1], &subID)

		switch typ {
		case "REQ":
			var filters gostr.Filters
			for _, raw := range msg[2:] {
				var filter gostr.Filter
				json.Unmarshal(raw, &filter)
				filters = append(filters, filter)
			}
			if r.singleValueFilters && !acceptsSingleValueFilters(filters) {
				out, _ := json.Marshal([]string{"CLOSED", subID, "blocked: Request rejected"})
				conn.Write(ctx, websocket.MessageText, out)
				continue
			}
			r.mu.Lock()
			if len(r.conns[conn]) >= r.maxReqs {
				r.refused++
				r.mu.Unlock()
				conn.Write(ctx, websocket.MessageText, []byte(`["NOTICE","ERROR: too many concurrent REQs"]`))
				continue
			}
			r.conns[conn][subID] = filters
			var replay [][]byte
			for _, event := range r.stored {
				if filters.Match(event) {
					out, _ := json.Marshal([]interface{}{"EVENT", subID, event})
					replay = append(replay, out)
				}
			}
			r.mu.Unlock()
			for _, out := range replay {
				conn.Write(ctx, websocket.MessageText, out)
			}
			conn.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`["EOSE",%q]`, subID)))
		case "CLOSE":
			r.mu.Lock()
			delete(r.conns[conn], subID)
			r.mu.Unlock()
		}
	}
}

func acceptsSingleValueFilters(filters gostr.Filters) bool {
	for _, filter := range filters {
		if len(filter.Authors) != 1 && len(filter.Tags["p"]) != 1 && len(filter.Tags["e"]) != 1 {
			return false
		}
	}
	return true
}

func (r *fakeRelay) openReqs() (total, maxPerConn, conns int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, subs := range r.conns {
		total += len(subs)
		maxPerConn = max(maxPerConn, len(subs))
		if len(subs) > 0 {
			conns++
		}
	}
	return total, maxPerConn, conns
}

func (r *fakeRelay) refusedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refused
}

func (r *fakeRelay) publish(event *gostr.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stored = append(r.stored, event)
	for conn, subs := range r.conns {
		for subID, filters := range subs {
			if filters.Match(event) {
				msg, _ := json.Marshal([]interface{}{"EVENT", subID, event})
				conn.Write(context.Background(), websocket.MessageText, msg)
			}
		}
	}
}

// dropConnections closes every client connection, as a relay restart would.
func (r *fakeRelay) dropConnections() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for conn := range r.conns {
		conn.Close(websocket.StatusGoingAway, "restart")
		delete(r.conns, conn)
	}
}

// closeOneReq ends one open REQ with CLOSED, as a relay rejecting its filter would.
func (r *fakeRelay) closeOneReq(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for conn, subs := range r.conns {
		for subID := range subs {
			msg, _ := json.Marshal([]string{"CLOSED", subID, reason})
			conn.Write(context.Background(), websocket.MessageText, msg)
			delete(subs, subID)
			return
		}
	}
}

// waitFor polls cond until it holds or timeout passes.
func waitFor(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestBatchedSubscriptionsAgainstRelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	batchResubscribeDelay = func(int) time.Duration { return 10 * time.Millisecond }
	t.Cleanup(func() { batchResubscribeDelay = resubscribeDelay })

	// capped at 10 REQs per connection, like nostream
	relay, server := newFakeRelay(10)
	defer server.Close()
	relayURL := "ws" + strings.TrimPrefix(server.URL, "http")

	var webhookMu sync.Mutex
	webhookHits := make(map[string]int)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		webhookMu.Lock()
		webhookHits[req.URL.Path]++
		webhookMu.Unlock()
	}))
	defer webhook.Close()
	hits := func(path string) int {
		webhookMu.Lock()
		defer webhookMu.Unlock()
		return webhookHits[path]
	}

	// storing the event receipt needs a database; a dry run one just errors (logged) and the webhook is still posted
	db, err := gorm.Open(postgres.New(postgres.Config{DSN: "host=127.0.0.1 port=1"}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	assert.NoError(t, err)

	svc := newTestService(ctx)
	svc.db = db

	walletKey := gostr.GeneratePrivateKey()
	walletPubkey, _ := gostr.GetPublicKey(walletKey)
	notification := func(connPubkey string, createdAt gostr.Timestamp) *gostr.Event {
		event := gostr.Event{
			PubKey:    walletPubkey,
			CreatedAt: createdAt,
			Kind:      LEGACY_NIP_47_NOTIFICATION_KIND,
			Tags:      gostr.Tags{{"p", connPubkey}},
			Content:   "encrypted",
		}
		assert.NoError(t, event.Sign(walletKey))
		return &event
	}
	reqs := func() (total, maxPerConn, conns int) { return relay.openReqs() }

	connPubkeys := make([]string, 2500)
	for i := range connPubkeys {
		connPubkeys[i], _ = gostr.GetPublicKey(gostr.GeneratePrivateKey())
	}

	// published while this process was down: picked up by the startup catch-up
	relay.publish(notification(connPubkeys[101], gostr.Now()-30))

	// 2,500 subscriptions would need 2,500 REQs one by one; this relay refuses any past 10 per connection
	// subscriptions without a relay use the default relay: ours, which only
	// accepts single-value filters, so they are not batched
	defaultRelay, defaultServer := newFakeRelay(100_000)
	defaultRelay.singleValueFilters = true
	defer defaultServer.Close()
	defaultRelayURL := "ws" + strings.TrimPrefix(defaultServer.URL, "http")
	svc.Cfg.DefaultRelayURLs = []string{defaultRelayURL}
	uuids := make([]string, len(connPubkeys))
	for i, connPubkey := range connPubkeys {
		subRelay := relayURL
		if i%10 == 0 {
			subRelay = ""
		}
		sub := notificationSubscription(subRelay, walletPubkey, connPubkey)
		sub.WebhookUrl = fmt.Sprintf("%s/conn/%d", webhook.URL, i)
		uuids[i] = sub.Uuid
		svc.restoreSubscription(sub, WebhookSubscriptionType)
	}

	waitFor(t, 15*time.Second, "2,250 subscriptions share 12 REQs over 2 connections", func() bool {
		total, maxPerConn, conns := reqs()
		return total == 12 && maxPerConn <= 9 && conns == 2
	})
	waitFor(t, 20*time.Second, "the 250 without a relay keep one REQ each on the default relay", func() bool {
		total, _, _ := defaultRelay.openReqs()
		return total == 250
	})
	waitFor(t, 5*time.Second, "a notification from before the start is caught up", func() bool { return hits("/conn/101") == 1 })

	relay.publish(notification(connPubkeys[2401], gostr.Now()))
	defaultRelay.publish(notification(connPubkeys[2410], gostr.Now())) // a subscription without a relay
	waitFor(t, 5*time.Second, "a notification reaches its subscription's webhook", func() bool { return hits("/conn/2401") == 1 })
	waitFor(t, 5*time.Second, "a subscription without a relay gets notifications from the default relay", func() bool { return hits("/conn/2410") == 1 })
	webhookMu.Lock()
	assert.Len(t, webhookHits, 3, "and no other subscription's")
	webhookMu.Unlock()

	// cancelling a subscription re-sends its chunk on a full connection (9 REQs):
	// the overlapping new REQ must still fit under the relay's cap of 10
	svc.cancelSubscription(uuids[1])
	time.Sleep(batchDebounce + time.Second)
	assert.Equal(t, 0, relay.refusedCount(), "the re-sent chunk's REQ is not refused")
	relay.publish(notification(connPubkeys[2], gostr.Now()))
	waitFor(t, 5*time.Second, "the re-sent chunk still delivers", func() bool { return hits("/conn/2") == 1 })

	// the relay restarts, and a notification is published before we are back
	relay.dropConnections()
	relay.publish(notification(connPubkeys[201], gostr.Now()))
	waitFor(t, 15*time.Second, "REQs are sent again after a disconnect", func() bool {
		total, _, _ := reqs()
		return total == 12
	})
	waitFor(t, 5*time.Second, "the notification published while disconnected is caught up", func() bool { return hits("/conn/201") == 1 })

	// the relay is down for a while: meanwhile a notification is published for an
	// existing subscription, and a new subscription comes in whose connection
	// already had (older) notifications stored on the relay
	relay.rejectConnect.Store(true)
	relay.dropConnections()
	relay.publish(notification(connPubkeys[301], gostr.Now()))
	newConn, _ := gostr.GetPublicKey(gostr.GeneratePrivateKey())
	relay.publish(notification(newConn, gostr.Now()-120))
	newSub := notificationSubscription(relayURL, walletPubkey, newConn)
	newSub.WebhookUrl = webhook.URL + "/new"
	newSub.CreatedAt = time.Now()
	svc.startSubscription(newSub, WebhookSubscriptionType)
	time.Sleep(batchDebounce + time.Second)
	relay.rejectConnect.Store(false)
	waitFor(t, 15*time.Second, "the existing subscription catches up after the outage", func() bool { return hits("/conn/301") == 1 })
	relay.publish(notification(newConn, gostr.Now()))
	waitFor(t, 5*time.Second, "the new subscription gets new notifications", func() bool { return hits("/new") == 1 })
	time.Sleep(500 * time.Millisecond)
	assert.Equal(t, 1, hits("/new"), "but not the ones from before it existed")

	// a CLOSED for another reason (e.g. rate limiting) resubscribes without shrinking
	relay.closeOneReq("rate-limited: slow down")
	waitFor(t, 15*time.Second, "the closed REQ is sent again, chunks unchanged", func() bool {
		total, _, _ := reqs()
		return total == 12
	})

	// the relay rejects a REQ as too large: the batch halves its chunks and sends them again
	relay.closeOneReq("error: filter too large")
	waitFor(t, 15*time.Second, "chunks are halved after CLOSED, still at most 9 per connection", func() bool {
		total, maxPerConn, conns := reqs()
		return total == 23 && maxPerConn <= 9 && conns == 3
	})
	relay.publish(notification(connPubkeys[202], gostr.Now()))
	waitFor(t, 5*time.Second, "notifications arrive after the CLOSED", func() bool { return hits("/conn/202") == 1 })

	assert.Equal(t, 0, relay.refusedCount())
	for _, path := range []string{"/conn/101", "/conn/201", "/conn/301", "/conn/2401", "/conn/2410", "/new"} {
		assert.Equal(t, 1, hits(path), "catch-up overlaps are delivered once: "+path)
	}

	// once every subscription is cancelled the batch closes its REQs and goes away
	svc.subscriptionsMutex.Lock()
	remaining := make([]string, 0, len(svc.subCancelFnMap))
	for uuid := range svc.subCancelFnMap {
		remaining = append(remaining, uuid)
	}
	svc.subscriptionsMutex.Unlock()
	for _, uuid := range remaining {
		svc.cancelSubscription(uuid)
	}
	waitFor(t, 15*time.Second, "REQs are closed once the batch is empty", func() bool {
		total, _, _ := reqs()
		defaultTotal, _, _ := defaultRelay.openReqs()
		return total == 0 && defaultTotal == 0
	})
	svc.batchesMutex.Lock()
	assert.Len(t, svc.batches, 0)
	svc.batchesMutex.Unlock()
}

func TestBatchedSubscriptionsFollowRelayNIP11Limit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// capped at 5 REQs per connection, and says so in NIP-11
	relay, server := newFakeRelay(5)
	relay.publishNIP11 = true
	defer server.Close()
	relayURL := "ws" + strings.TrimPrefix(server.URL, "http")

	svc := newTestService(ctx)
	for i := 0; i < 2500; i++ {
		connPubkey, _ := gostr.GetPublicKey(gostr.GeneratePrivateKey())
		svc.restoreSubscription(notificationSubscription(relayURL, "wallet", connPubkey), WebhookSubscriptionType)
	}

	waitFor(t, 15*time.Second, "13 REQs at most 4 per connection", func() bool {
		total, maxPerConn, conns := relay.openReqs()
		return total == 13 && maxPerConn <= 4 && conns == 4
	})
	assert.Equal(t, 0, relay.refusedCount())
}

// A relay that refuses batched filters: the batch falls back to one REQ
// per subscription, and later subscriptions on that relay aren't batched.
func TestBatchFallsBackWhenRelayRefusesBatchedFilters(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	relay, server := newFakeRelay(100_000)
	relay.singleValueFilters = true
	defer server.Close()
	relayURL := "ws" + strings.TrimPrefix(server.URL, "http")

	var webhookMu sync.Mutex
	webhookHits := make(map[string]int)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		webhookMu.Lock()
		webhookHits[req.URL.Path]++
		webhookMu.Unlock()
	}))
	defer webhook.Close()
	db, err := gorm.Open(postgres.New(postgres.Config{DSN: "host=127.0.0.1 port=1"}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	assert.NoError(t, err)

	svc := newTestService(ctx)
	svc.db = db

	// every wallet its own pubkey, so a batched filter has many authors and #p values
	walletKeys := make([]string, 150)
	connPubkeys := make([]string, 150)
	for i := range connPubkeys {
		walletKeys[i] = gostr.GeneratePrivateKey()
		walletPubkey, _ := gostr.GetPublicKey(walletKeys[i])
		connPubkeys[i], _ = gostr.GetPublicKey(gostr.GeneratePrivateKey())
		sub := notificationSubscription(relayURL, walletPubkey, connPubkeys[i])
		sub.WebhookUrl = fmt.Sprintf("%s/conn/%d", webhook.URL, i)
		svc.restoreSubscription(sub, WebhookSubscriptionType)
	}

	waitFor(t, 30*time.Second, "after the refusal every subscription has its own REQ", func() bool {
		total, _, _ := relay.openReqs()
		return total == 150
	})
	svc.batchesMutex.Lock()
	assert.Len(t, svc.batches, 0, "the refused batch is gone")
	assert.True(t, svc.unbatchedRelays[gostr.NormalizeURL(relayURL)])
	svc.batchesMutex.Unlock()

	walletPubkey, _ := gostr.GetPublicKey(walletKeys[42])
	event := gostr.Event{PubKey: walletPubkey, CreatedAt: gostr.Now(), Kind: LEGACY_NIP_47_NOTIFICATION_KIND, Tags: gostr.Tags{{"p", connPubkeys[42]}}, Content: "encrypted"}
	assert.NoError(t, event.Sign(walletKeys[42]))
	relay.publish(&event)
	waitFor(t, 5*time.Second, "notifications are delivered after the fallback", func() bool {
		webhookMu.Lock()
		defer webhookMu.Unlock()
		return webhookHits["/conn/42"] == 1
	})

	// a later subscription on that relay goes straight to its own REQ
	newConn, _ := gostr.GetPublicKey(gostr.GeneratePrivateKey())
	svc.startSubscription(notificationSubscription(relayURL, walletPubkey, newConn), WebhookSubscriptionType)
	waitFor(t, 5*time.Second, "the new subscription gets its own REQ", func() bool {
		total, _, _ := relay.openReqs()
		return total == 151
	})
	svc.batchesMutex.Lock()
	assert.Len(t, svc.batches, 0)
	svc.batchesMutex.Unlock()
}
