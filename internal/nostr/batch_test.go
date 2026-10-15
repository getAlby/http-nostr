package nostr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

	_, sends := batch.plan(noLiveReqs)

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

	_, sends := batch.plan(allLiveReqs)
	assert.Len(t, sends, 0, "nothing changed")

	addMembers(batch, 350, 351)
	_, sends = batch.plan(allLiveReqs)
	assert.Len(t, sends, 1, "a new #p value only touches the chunk with room")

	batch.remove(subs[0].Uuid, "conn0000")
	_, sends = batch.plan(allLiveReqs)
	assert.Len(t, sends, 1, "a removed #p value only touches its own chunk")

	current, sends := batch.plan(noLiveReqs)
	assert.Len(t, sends, len(current), "chunks without a live REQ are all sent again")
}

func TestBatchSpreadsChunksOverConnections(t *testing.T) {
	batch := newSubscriptionBatch(nil, "wss://relay.example", []int{LEGACY_NIP_47_NOTIFICATION_KIND})
	batch.maxPubkeys = 1
	addMembers(batch, 0, 25)

	perConn := make(map[int]int)
	current, _ := batch.plan(noLiveReqs)
	for chunk := range current {
		perConn[chunk.conn]++
	}
	assert.Equal(t, map[int]int{0: batchMaxReqsPerConn, 1: batchMaxReqsPerConn, 2: 5}, perConn)
}

func TestBatchShrinkHalvesChunks(t *testing.T) {
	batch := newSubscriptionBatch(nil, "wss://relay.example", []int{LEGACY_NIP_47_NOTIFICATION_KIND})
	addMembers(batch, 0, 200)

	batch.shrink()
	current, _ := batch.plan(noLiveReqs)
	assert.Len(t, current, 2)

	for i := 0; i < 5; i++ {
		batch.shrink()
	}
	current, _ = batch.plan(noLiveReqs)
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

	assert.Len(t, batch.membersFor(event("wallet1", "conn1")), 1)
	// a REQ combines authors and #p values, so it also lets through pairs
	// nobody subscribed to; those must reach nobody
	assert.Len(t, batch.membersFor(event("wallet2", "conn1")), 0)
	assert.Len(t, batch.membersFor(event("wallet1", "conn2")), 0)
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
// our own relay), returns stored events matching a REQ before EOSE, and pushes
// new events to the open REQs whose filters match.
type fakeRelay struct {
	maxReqs int

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
	relay.publish(notification(connPubkeys[100], gostr.Now()-60))

	// 2,500 subscriptions would need 2,500 REQs one by one; this relay refuses any past 10 per connection
	for i, connPubkey := range connPubkeys {
		sub := notificationSubscription(relayURL, walletPubkey, connPubkey)
		sub.WebhookUrl = fmt.Sprintf("%s/conn/%d", webhook.URL, i)
		svc.restoreSubscription(sub, WebhookSubscriptionType)
	}

	waitFor(t, 15*time.Second, "2,500 subscriptions share 13 REQs over 2 connections", func() bool {
		total, maxPerConn, conns := reqs()
		return total == 13 && maxPerConn <= 10 && conns == 2
	})
	waitFor(t, 5*time.Second, "a notification from before the start is caught up", func() bool { return hits("/conn/100") == 1 })

	relay.publish(notification(connPubkeys[2400], gostr.Now()))
	waitFor(t, 5*time.Second, "a notification reaches its subscription's webhook", func() bool { return hits("/conn/2400") == 1 })
	webhookMu.Lock()
	assert.Len(t, webhookHits, 2, "and no other subscription's")
	webhookMu.Unlock()

	// the relay restarts, and a notification is published before we are back
	relay.dropConnections()
	relay.publish(notification(connPubkeys[201], gostr.Now()))
	waitFor(t, 15*time.Second, "REQs are sent again after a disconnect", func() bool {
		total, _, _ := reqs()
		return total == 13
	})
	waitFor(t, 5*time.Second, "the notification published while disconnected is caught up", func() bool { return hits("/conn/201") == 1 })

	// the relay rejects a REQ: the batch halves its chunks and sends them again
	relay.closeOneReq("error: filter too large")
	waitFor(t, 15*time.Second, "chunks are halved after CLOSED, still at most 10 per connection", func() bool {
		total, maxPerConn, conns := reqs()
		return total == 25 && maxPerConn <= 10 && conns == 3
	})
	relay.publish(notification(connPubkeys[202], gostr.Now()))
	waitFor(t, 5*time.Second, "notifications arrive after the CLOSED", func() bool { return hits("/conn/202") == 1 })

	assert.Equal(t, 0, relay.refused)
	for _, path := range []string{"/conn/100", "/conn/201", "/conn/2400"} {
		assert.Equal(t, 1, hits(path), "catch-up overlaps are delivered once: "+path)
	}

	// once every subscription is cancelled the batch closes its REQs and goes away
	svc.subscriptionsMutex.Lock()
	uuids := make([]string, 0, len(svc.subCancelFnMap))
	for uuid := range svc.subCancelFnMap {
		uuids = append(uuids, uuid)
	}
	svc.subscriptionsMutex.Unlock()
	for _, uuid := range uuids {
		svc.cancelSubscription(uuid)
	}
	waitFor(t, 15*time.Second, "REQs are closed once the batch is empty", func() bool {
		total, _, _ := reqs()
		return total == 0
	})
	svc.batchesMutex.Lock()
	assert.Len(t, svc.batches, 0)
	svc.batchesMutex.Unlock()
}
