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
		sub := notificationSubscription("wss://relay.example", fmt.Sprintf("wallet%d", i%2), fmt.Sprintf("conn%03d", i))
		batch.add(sub.Uuid, batchMember{subscription: sub, subscriptionType: WebhookSubscriptionType})
		subs = append(subs, sub)
	}
	return subs
}

func noLiveReqs(*batchChunk) bool  { return false }
func allLiveReqs(*batchChunk) bool { return true }

func TestBatchPlanChunksPubkeys(t *testing.T) {
	batch := newSubscriptionBatch(nil, "wss://relay.example", []int{LEGACY_NIP_47_NOTIFICATION_KIND})
	addMembers(batch, 0, 250)
	// a duplicate subscription for the same connection shares its #p value
	dup := notificationSubscription("wss://relay.example", "wallet0", "conn000")
	batch.add(dup.Uuid, batchMember{subscription: dup, subscriptionType: WebhookSubscriptionType})

	_, filters := batch.plan(noLiveReqs)

	assert.Len(t, filters, 3)
	total := 0
	for _, filter := range filters {
		assert.True(t, len(filter.Tags["p"]) <= batchMaxPubkeys)
		assert.Equal(t, []string{"wallet0", "wallet1"}, filter.Authors)
		total += len(filter.Tags["p"])
	}
	assert.Equal(t, 250, total)
}

func TestBatchPlanOnlyResendsChangedChunks(t *testing.T) {
	batch := newSubscriptionBatch(nil, "wss://relay.example", []int{LEGACY_NIP_47_NOTIFICATION_KIND})
	subs := addMembers(batch, 0, 150)
	batch.plan(noLiveReqs)

	_, filters := batch.plan(allLiveReqs)
	assert.Len(t, filters, 0, "nothing changed")

	addMembers(batch, 150, 151)
	_, filters = batch.plan(allLiveReqs)
	assert.Len(t, filters, 1, "a new #p value only touches the chunk with room")

	batch.remove(subs[0].Uuid, "conn000")
	_, filters = batch.plan(allLiveReqs)
	assert.Len(t, filters, 1, "a removed #p value only touches its own chunk")

	current, filters := batch.plan(noLiveReqs)
	assert.Len(t, filters, len(current), "chunks without a live REQ are all sent again")
}

func TestBatchShrinkHalvesChunks(t *testing.T) {
	batch := newSubscriptionBatch(nil, "wss://relay.example", []int{LEGACY_NIP_47_NOTIFICATION_KIND})
	addMembers(batch, 0, 100)

	batch.shrink()
	current, _ := batch.plan(noLiveReqs)
	assert.Len(t, current, 2)

	for i := 0; i < 5; i++ {
		batch.shrink()
	}
	current, _ = batch.plan(noLiveReqs)
	assert.Len(t, current, 100/batchMinPubkeys, "chunks don't shrink below the floor")
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

// fakeRelay is a minimal relay that, like relay-nwc.rizful.com, refuses more
// than maxReqs concurrent REQs per connection, and can push events to the
// open REQs whose filters match.
type fakeRelay struct {
	maxReqs int

	mu      sync.Mutex
	conns   map[*websocket.Conn]map[string]gostr.Filters
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
			r.mu.Unlock()
			conn.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`["EOSE",%q]`, subID)))
		case "CLOSE":
			r.mu.Lock()
			delete(r.conns[conn], subID)
			r.mu.Unlock()
		}
	}
}

func (r *fakeRelay) openReqs() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := 0
	for _, subs := range r.conns {
		total += len(subs)
	}
	return total
}

func (r *fakeRelay) publish(event *gostr.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for conn, subs := range r.conns {
		for subID, filters := range subs {
			if filters.Match(event) {
				msg, _ := json.Marshal([]interface{}{"EVENT", subID, event})
				conn.Write(context.Background(), websocket.MessageText, msg)
			}
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

func TestBatchedSubscriptionsAgainstRelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	batchResubscribeDelay = func(int) time.Duration { return 10 * time.Millisecond }
	t.Cleanup(func() { batchResubscribeDelay = resubscribeDelay })

	relay, server := newFakeRelay(101)
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
	notify := func(connPubkey string) {
		event := gostr.Event{
			PubKey:    walletPubkey,
			CreatedAt: gostr.Now(),
			Kind:      LEGACY_NIP_47_NOTIFICATION_KIND,
			Tags:      gostr.Tags{{"p", connPubkey}},
			Content:   "encrypted",
		}
		assert.NoError(t, event.Sign(walletKey))
		relay.publish(&event)
	}

	// 250 subscriptions would need 250 REQs one by one; the relay refuses any past 101
	connPubkeys := make([]string, 250)
	for i := range connPubkeys {
		connPubkeys[i], _ = gostr.GetPublicKey(gostr.GeneratePrivateKey())
		sub := notificationSubscription(relayURL, walletPubkey, connPubkeys[i])
		sub.WebhookUrl = fmt.Sprintf("%s/conn/%d", webhook.URL, i)
		svc.restoreSubscription(sub, WebhookSubscriptionType)
	}

	waitFor(t, 15*time.Second, "250 subscriptions should share 3 REQs", func() bool { return relay.openReqs() == 3 })
	notify(connPubkeys[200])
	waitFor(t, 5*time.Second, "the notification reaches its subscription's webhook", func() bool { return hits("/conn/200") == 1 })
	webhookMu.Lock()
	assert.Len(t, webhookHits, 1, "and no other subscription's")
	webhookMu.Unlock()

	// the relay restarts: every REQ is sent again
	relay.dropConnections()
	waitFor(t, 15*time.Second, "REQs are sent again after a disconnect", func() bool { return relay.openReqs() == 3 })
	notify(connPubkeys[201])
	waitFor(t, 5*time.Second, "notifications arrive after the reconnect", func() bool { return hits("/conn/201") == 1 })

	// the relay rejects a REQ: the batch halves its chunks and sends them again
	relay.closeOneReq("error: filter too large")
	waitFor(t, 15*time.Second, "chunks are halved after CLOSED", func() bool { return relay.openReqs() == 5 })
	notify(connPubkeys[202])
	waitFor(t, 5*time.Second, "notifications arrive after the CLOSED", func() bool { return hits("/conn/202") == 1 })
	assert.Equal(t, 0, relay.refused)

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
	waitFor(t, 15*time.Second, "REQs are closed once the batch is empty", func() bool { return relay.openReqs() == 0 })
	svc.batchesMutex.Lock()
	assert.Len(t, svc.batches, 0)
	svc.batchesMutex.Unlock()
}
