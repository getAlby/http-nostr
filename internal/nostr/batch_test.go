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

func TestBatchFiltersChunkPubkeys(t *testing.T) {
	batch := &subscriptionBatch{kinds: []int{LEGACY_NIP_47_NOTIFICATION_KIND}, members: make(map[string]batchMember)}
	for i := 0; i < 450; i++ {
		wallet := fmt.Sprintf("wallet%d", i%2)
		sub := notificationSubscription("wss://relay.example", wallet, fmt.Sprintf("conn%03d", i))
		batch.members[sub.Uuid] = batchMember{subscription: sub, subscriptionType: WebhookSubscriptionType}
	}
	// a duplicate subscription for the same connection shares its #p value
	dup := notificationSubscription("wss://relay.example", "wallet0", "conn000")
	batch.members[dup.Uuid] = batchMember{subscription: dup, subscriptionType: WebhookSubscriptionType}

	filters := batch.filters()

	assert.Len(t, filters, 3)
	total := 0
	for _, filter := range filters {
		assert.True(t, len(filter.Tags["p"]) <= batchMaxPubkeys)
		assert.Equal(t, []string{"wallet0", "wallet1"}, filter.Authors)
		total += len(filter.Tags["p"])
	}
	assert.Equal(t, 450, total)
}

func TestBatchMemberMatchesExactly(t *testing.T) {
	member := batchMember{subscription: notificationSubscription("wss://relay.example", "wallet1", "conn1")}
	event := func(author, p string) *gostr.Event {
		return &gostr.Event{PubKey: author, Kind: LEGACY_NIP_47_NOTIFICATION_KIND, Tags: gostr.Tags{{"p", p}}}
	}

	assert.True(t, member.matches(event("wallet1", "conn1")))
	// a batched filter combines authors and #p values, so it also lets through
	// pairs nobody subscribed to; those must not reach this member
	assert.False(t, member.matches(event("wallet2", "conn1")))
	assert.False(t, member.matches(event("wallet1", "conn2")))
}

func TestBatchDropsDuplicateEvents(t *testing.T) {
	batch := &subscriptionBatch{seen: make(map[string]time.Time)}

	assert.False(t, batch.seenBefore("event1"))
	assert.True(t, batch.seenBefore("event1"))
	assert.False(t, batch.seenBefore("event2"))
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

func TestBatchedSubscriptionsStayUnderRelayReqLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

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

	// storing the event receipt needs a database; a dry run one just errors (logged) and the webhook is still posted
	db, err := gorm.Open(postgres.New(postgres.Config{DSN: "host=127.0.0.1 port=1"}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	assert.NoError(t, err)

	svc := newTestService(ctx)
	svc.db = db

	walletKey := gostr.GeneratePrivateKey()
	walletPubkey, _ := gostr.GetPublicKey(walletKey)

	// 250 subscriptions would need 250 REQs one by one; the relay refuses any past 101
	connPubkeys := make([]string, 250)
	for i := range connPubkeys {
		connPubkeys[i], _ = gostr.GetPublicKey(gostr.GeneratePrivateKey())
		sub := notificationSubscription(relayURL, walletPubkey, connPubkeys[i])
		sub.WebhookUrl = fmt.Sprintf("%s/conn/%d", webhook.URL, i)
		svc.restoreSubscription(sub, WebhookSubscriptionType)
	}

	waitFor(t, 15*time.Second, "250 subscriptions should share 2 REQs", func() bool { return relay.openReqs() == 2 })
	assert.Equal(t, 0, relay.refused)

	notification := gostr.Event{
		PubKey:    walletPubkey,
		CreatedAt: gostr.Now(),
		Kind:      LEGACY_NIP_47_NOTIFICATION_KIND,
		Tags:      gostr.Tags{{"p", connPubkeys[200]}},
		Content:   "encrypted",
	}
	assert.NoError(t, notification.Sign(walletKey))
	relay.publish(&notification)

	waitFor(t, 5*time.Second, "the notification reaches its subscription's webhook", func() bool {
		webhookMu.Lock()
		defer webhookMu.Unlock()
		return webhookHits["/conn/200"] == 1
	})
	webhookMu.Lock()
	assert.Len(t, webhookHits, 1, "and no other subscription's")
	webhookMu.Unlock()
	assert.Equal(t, 2, relay.openReqs())
	assert.Equal(t, 0, relay.refused)

	// cancelling a subscription shrinks the batch on the next rebuild
	for i := 0; i < 100; i++ {
		svc.subscriptionsMutex.Lock()
		var uuid string
		for id := range svc.subCancelFnMap {
			uuid = id
			break
		}
		svc.subscriptionsMutex.Unlock()
		svc.cancelSubscription(uuid)
	}
	waitFor(t, 15*time.Second, "150 subscriptions fit in 1 REQ", func() bool { return relay.openReqs() == 1 })
}
