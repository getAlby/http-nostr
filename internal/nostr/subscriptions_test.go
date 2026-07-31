package nostr

import (
	"context"
	"testing"
	"time"
)

func TestResubscribeDelayStaysWithinWindow(t *testing.T) {
	cases := []struct {
		attempt int
		window  time.Duration
	}{
		{attempt: 1, window: resubscribeBaseDelay},
		{attempt: 2, window: 2 * resubscribeBaseDelay},
		{attempt: 3, window: 4 * resubscribeBaseDelay},
		// Doubling is capped, so far-out attempts share the max window.
		{attempt: 9, window: resubscribeMaxDelay},
		{attempt: 50, window: resubscribeMaxDelay},
	}

	for _, tc := range cases {
		for i := 0; i < 500; i++ {
			got := resubscribeDelay(tc.attempt)
			if got < 0 || got >= tc.window {
				t.Fatalf("attempt %d: delay %v outside [0, %v)", tc.attempt, got, tc.window)
			}
		}
	}
}

func TestResubscribeDelayGrowsWithAttempts(t *testing.T) {
	// Compare means rather than single draws, which are random by design.
	mean := func(attempt int) time.Duration {
		const samples = 2000
		var total time.Duration
		for i := 0; i < samples; i++ {
			total += resubscribeDelay(attempt)
		}
		return total / samples
	}

	first, third := mean(1), mean(3)
	if third <= first {
		t.Fatalf("expected later attempts to back off further, got attempt1=%v attempt3=%v", first, third)
	}
}

// The regression this guards: every persistent subscription watches the same
// shared relay connection, so one connection failure wakes them all at once.
// Retrying after an identical delay would send them back as a synchronised
// burst rather than a spread.
func TestResubscribeDelayIsDecorrelatedAcrossSubscriptions(t *testing.T) {
	const subscriptions = 1000

	buckets := make(map[time.Duration]int)
	for i := 0; i < subscriptions; i++ {
		// Round to 100ms buckets: a fixed delay lands every subscription in
		// one bucket, jitter spreads them across many.
		buckets[resubscribeDelay(1)/(100*time.Millisecond)]++
	}

	// Full jitter should populate a good share of the available buckets.
	// The threshold is deliberately conservative so the test stays stable.
	if len(buckets) < 20 {
		t.Fatalf("retries are too clustered: %d distinct buckets across %d subscriptions", len(buckets), subscriptions)
	}

	for bucket, count := range buckets {
		if count > subscriptions/4 {
			t.Fatalf("bucket %v holds %d of %d retries, expected a spread", bucket, count, subscriptions)
		}
	}
}

// Subscriptions reloaded at startup are restored in bulk, so they carry an
// initial delay. It must be abandoned when the subscription is cancelled,
// rather than connecting later regardless.
func TestStartPersistentSubscriptionAbandonsInitialDelayWhenCancelled(t *testing.T) {
	svc := &Service{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.startPersistentSubscription(ctx, Subscription{}, WebhookSubscriptionType, time.Hour)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("startPersistentSubscription kept waiting after its context was cancelled")
	}
}

func TestWaitBeforeResubscribeReturnsFalseWhenContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan bool, 1)
	go func() {
		// A high attempt number means a long window, so this would block for
		// minutes if cancellation were not honoured.
		done <- waitBeforeResubscribe(ctx, 50)
	}()

	select {
	case got := <-done:
		if got {
			t.Fatal("expected false when the context is already cancelled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitBeforeResubscribe ignored context cancellation")
	}
}
