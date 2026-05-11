package nostr

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/test-go/testify/assert"
)

// unreachableRelay points at a loopback port that is virtually guaranteed to
// refuse the TCP connect immediately, so each failed attempt returns within
// milliseconds.
const unreachableRelay = "ws://127.0.0.1:1"

// Level 1: unit tests for the backoff loop's interaction with onConnectFail.
// These exercise only relayConnectWithBackoff — no DB, no real relay.

func TestRelayConnectWithBackoff_AbortsWhenOnFailReturnsTrue(t *testing.T) {
	svc := &Service{Logger: logrus.New()}

	var calls int
	onFail := func(err error) bool {
		calls++
		return calls >= 3
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	relay, err := svc.relayConnectWithBackoff(ctx, unreachableRelay, onFail)

	assert.True(t, errors.Is(err, ErrRelayUnreachable), "expected ErrRelayUnreachable, got %v", err)
	assert.Nil(t, relay)
	assert.Equal(t, 3, calls)
}

func TestRelayConnectWithBackoff_OnFailFalseKeepsRetryingUntilCtx(t *testing.T) {
	svc := &Service{Logger: logrus.New()}

	var calls int
	onFail := func(err error) bool {
		calls++
		return false
	}

	// Window long enough for at least attempts 1 (sleep 0) and 2 (sleep 2s),
	// short enough to fail fast if the loop ignores ctx cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := svc.relayConnectWithBackoff(ctx, unreachableRelay, onFail)

	assert.True(t, errors.Is(err, context.DeadlineExceeded), "expected context.DeadlineExceeded, got %v", err)
	assert.True(t, calls >= 2, "expected at least 2 attempts, got %d", calls)
}

// Level 2: integration test for the full auto-disable flow. Requires the
// shared testSvc (which itself needs a Postgres test DB).

func TestSubscription_AutoDisablesWhenRelayUnreachable(t *testing.T) {
	if testSvc == nil {
		t.Fatal("testService is not initialized")
	}

	// Force a low threshold so the test finishes in seconds rather than days.
	// Restored via t.Cleanup so other tests are unaffected.
	origMax := testSvc.Cfg.MaxRelayConnectionErrors
	testSvc.Cfg.MaxRelayConnectionErrors = 3
	t.Cleanup(func() { testSvc.Cfg.MaxRelayConnectionErrors = origMax })

	// Ensure subCancelFnMap is initialized — stopSubscription touches it.
	testSvc.subscriptionsMutex.Lock()
	if testSvc.subCancelFnMap == nil {
		testSvc.subCancelFnMap = make(map[string]context.CancelFunc)
	}
	testSvc.subscriptionsMutex.Unlock()

	kinds := []int{1}
	authors := []string{publicKey}
	sub := &Subscription{
		RelayUrl:   unreachableRelay,
		WebhookUrl: "http://127.0.0.1:1/webhook",
		Open:       true,
		Kinds:      &kinds,
		Authors:    &authors,
	}
	if err := testSvc.db.Create(sub).Error; err != nil {
		t.Fatalf("failed to create test subscription: %v", err)
	}
	t.Cleanup(func() { testSvc.db.Delete(sub) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		testSvc.startSubscription(ctx, sub, nil, testSvc.handleSubscribedEvent)
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("startSubscription did not exit within timeout — expected disable after 3 failures")
	}

	var got Subscription
	if err := testSvc.db.First(&got, sub.ID).Error; err != nil {
		t.Fatalf("failed to reload subscription: %v", err)
	}

	assert.False(t, got.Open, "subscription should be closed")
	assert.Equal(t, SUBSCRIPTION_CLOSED_RELAY_UNREACHABLE, got.ClosedReason)
	assert.True(t, got.ConnectionErrorCount >= 3, "expected at least 3 errors recorded, got %d", got.ConnectionErrorCount)
	assert.NotNil(t, got.LastConnectionError)
}
