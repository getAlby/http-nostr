package nostr

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/test-go/testify/assert"
)

const unreachableRelay = "ws://127.0.0.1:1"

func TestRelayConnectWithBackoff_CustomRelayStopsAfterMaxFailures(t *testing.T) {
	svc := &Service{
		Cfg:    &Config{DefaultRelayURL: "wss://relay.getalby.com/v1", MaxRelayConnectionErrors: 3},
		Logger: logrus.New(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	relay, err := svc.relayConnectWithBackoff(ctx, unreachableRelay)

	assert.True(t, errors.Is(err, ErrRelayUnreachable), "expected ErrRelayUnreachable, got %v", err)
	assert.Nil(t, relay)
}

func TestRelayConnectWithBackoff_DefaultRelayIgnoresMaxFailures(t *testing.T) {
	svc := &Service{
		Cfg:    &Config{DefaultRelayURL: unreachableRelay, MaxRelayConnectionErrors: 1},
		Logger: logrus.New(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2000*time.Millisecond)
	defer cancel()

	_, err := svc.relayConnectWithBackoff(ctx, "")

	assert.True(t, errors.Is(err, context.DeadlineExceeded), "expected context deadline exceeded, got %v", err)
	assert.False(t, errors.Is(err, ErrRelayUnreachable), "default relay should not stop after max failures")
}
