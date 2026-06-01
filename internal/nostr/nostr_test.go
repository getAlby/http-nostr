package nostr

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	gostr "github.com/getAlby/go-nostr"
	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	"github.com/test-go/testify/assert"
)

const testDefaultRelay = "wss://relay.getalby.com"
const testUnreachableRelay = "ws://127.0.0.1:1"
const testWalletPubkey = "69effe7b49a6dd5cf525bd0905917a5005ffe480b58eeb8e861418cf3ae760d9"

func newTestService(ctx context.Context) *Service {
	return &Service{
		Cfg: &Config{
			DefaultRelayURLs:         []string{testDefaultRelay},
			MaxRelayConnectionErrors: 3,
			EncryptionKey:            "0123456789abcdef0123456789abcdef",
		},
		Ctx:            ctx,
		Wg:             &sync.WaitGroup{},
		Logger:         logrus.New(),
		Relays:         make(map[string]*gostr.Relay),
		subCancelFnMap: make(map[string]context.CancelFunc),
	}
}

func TestInfoHandler(t *testing.T) {
	svc := newTestService(context.Background())
	runHandlerTest(t, http.MethodPost, "/info", map[string]interface{}{}, http.StatusBadRequest, svc.InfoHandler)
}

func TestPublishHandler(t *testing.T) {
	svc := newTestService(context.Background())
	runHandlerTest(t, http.MethodPost, "/publish", map[string]interface{}{}, http.StatusBadRequest, svc.PublishHandler)
}

func TestNIP47Handler(t *testing.T) {
	svc := newTestService(context.Background())

	t.Run("missing_pubkey", func(t *testing.T) {
		runHandlerTest(t, http.MethodPost, "/nip47", map[string]interface{}{}, http.StatusBadRequest, svc.NIP47Handler)
	})

	t.Run("missing_event", func(t *testing.T) {
		runHandlerTest(t, http.MethodPost, "/nip47", map[string]interface{}{"walletPubkey": testWalletPubkey}, http.StatusBadRequest, svc.NIP47Handler)
	})
}

func TestNIP47WebhookHandler(t *testing.T) {
	svc := newTestService(context.Background())
	runHandlerTest(t, http.MethodPost, "/nip47/webhook", map[string]interface{}{"walletPubkey": testWalletPubkey}, http.StatusBadRequest, svc.NIP47WebhookHandler)
}

func TestNIP47NotificationHandler(t *testing.T) {
	svc := newTestService(context.Background())
	runHandlerTest(t, http.MethodPost, "/nip47/notifications", map[string]interface{}{"walletPubkey": testWalletPubkey, "webhookUrl": "https://example.com"}, http.StatusBadRequest, svc.NIP47NotificationHandler)
}

func TestNIP47PushNotificationHandler(t *testing.T) {
	svc := newTestService(context.Background())
	runHandlerTest(t, http.MethodPost, "/nip47/push", map[string]interface{}{"pushToken": "bad-token"}, http.StatusBadRequest, svc.NIP47PushNotificationHandler)
}

func TestRelayConnectWithBackoffCustomRelayStopsAfterMaxFailures(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	svc := newTestService(ctx)
	relay, err := svc.relayConnectWithBackoff(testUnreachableRelay)

	assert.True(t, err == ErrRelayUnreachable, "expected ErrRelayUnreachable, got %v", err)
	assert.Nil(t, relay)
}

func TestRelayConnectWithBackoffDefaultRelayIgnoresMaxFailures(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	svc := newTestService(ctx)
	svc.Cfg.DefaultRelayURLs = []string{testUnreachableRelay}
	svc.Cfg.MaxRelayConnectionErrors = 1

	_, err := svc.relayConnectWithBackoff(testUnreachableRelay)

	assert.True(t, err == context.DeadlineExceeded, "expected context deadline exceeded, got %v", err)
	assert.False(t, err == ErrRelayUnreachable, "default relay should not stop after max failures")
}

func TestThunderingHerdPrevention(t *testing.T) {
	svc := newTestService(context.Background())
	deadRelayURL := testUnreachableRelay

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			reqCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			_, err := svc.getRelayConnection(reqCtx, deadRelayURL)
			assert.Equal(t, context.DeadlineExceeded, err)
		}()
	}

	wg.Wait()
}

func TestContextCancellationDuringBackoff(t *testing.T) {
	svc := newTestService(context.Background())

	reqCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	startTime := time.Now()
	_, err := svc.getRelayConnection(reqCtx, testUnreachableRelay)
	duration := time.Since(startTime)

	assert.Equal(t, context.DeadlineExceeded, err)
	if duration >= 150*time.Millisecond {
		t.Fatalf("expected request to stop quickly, took %v", duration)
	}
}

func runHandlerTest(t *testing.T, method string, target string, body map[string]interface{}, expectedCode int, handler echo.HandlerFunc) {
	t.Helper()

	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}

	e := echo.New()
	req := httptest.NewRequest(method, target, bytes.NewBuffer(payload))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if assert.NoError(t, handler(c)) {
		assert.Equal(t, expectedCode, rec.Code)
	}
}
