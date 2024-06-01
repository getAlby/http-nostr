package nostr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"http-nostr/migrations"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
	"github.com/labstack/echo/v4"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip04"
	"github.com/sirupsen/logrus"
	"github.com/test-go/testify/assert"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

const testDB = "postgresql://username@localhost:5432/httpnostr-test"
const ALBY_NWC_PUBKEY = "69effe7b49a6dd5cf525bd0905917a5005ffe480b58eeb8e861418cf3ae760d9"

var testSvc *Service
var privateKey, publicKey string
var webhookResult nostr.Event

func setupTestService() *Service {
	ctx := context.Background()

	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{})
	logger.SetOutput(os.Stdout)
	logger.SetLevel(logrus.InfoLevel)

	// Load env file as env variables
	godotenv.Load(".env")

	cfg := &Config{}
	err := envconfig.Process("", cfg)
	if err != nil {
		logger.Fatalf("Failed to process config: %v", err)
		return nil
	}

	db, err := gorm.Open(postgres.Open(testDB), &gorm.Config{})
	if err != nil {
		logger.Fatalf("Failed to open DB: %v", err)
		return nil
	}

	err = migrations.Migrate(db)
	if err != nil {
		logger.Fatalf("Failed to migrate: %v", err)
		return nil
	}

	relay, err := nostr.RelayConnect(ctx, cfg.DefaultRelayURL)
	if err != nil {
		logger.Fatalf("Failed to connect to default relay: %v", err)
		return nil
	}

	var wg sync.WaitGroup
	svc := &Service{
		Cfg:           cfg,
		db:            db,
		Ctx:           ctx,
		Wg:            &wg,
		Logger:        logger,
		Relay:         relay,
		subscriptions: make(map[string]*nostr.Subscription),
	}

	privateKey = nostr.GeneratePrivateKey()
	publicKey, err = nostr.GetPublicKey(privateKey)
	if err != nil {
		logger.Fatalf("Error converting nostr privkey to pubkey: %v", err)
	}

	return svc
}

func TestMain(m *testing.M) {
	// initialize Service before any tests run
	testSvc = setupTestService()
	exitCode := m.Run()
	os.Exit(exitCode)
}

func TestInfoHandler(t *testing.T) {
	if testSvc == nil {
		t.Fatal("testService is not initialized")
	}

	e := echo.New()

	tests := []struct {
		name         string
		body         map[string]interface{}
		expectedCode int
		expectedResp interface{}
	}{
		{
			name:         "missing_pubkey",
			body:         map[string]interface{}{},
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "valid_request",
			body:         map[string]interface{}{"walletPubkey": ALBY_NWC_PUBKEY},
			expectedCode: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.body)
			runTest(t, e, http.MethodPost, "/info", bytes.NewBuffer(body), tt.expectedCode, testSvc.InfoHandler)
		})
	}
}

func TestPublishHandler(t *testing.T) {
	if testSvc == nil {
		t.Fatal("testService is not initialized")
	}

	e := echo.New()

	tests := []struct {
		name         string
		body         map[string]interface{}
		expectedCode int
		expectedResp interface{}
	}{
		{
			name:         "missing_event",
			body:         map[string]interface{}{},
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "valid_request",
			body:         map[string]interface{}{"event": generateRequestEvent()},
			expectedCode: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.body)
			runTest(t, e, http.MethodPost, "/publish", bytes.NewBuffer(body), tt.expectedCode, testSvc.PublishHandler)
		})
	}
}

func TestNIP47Handler(t *testing.T) {
	if testSvc == nil {
		t.Fatal("testService is not initialized")
	}

	e := echo.New()

	tests := []struct {
		name         string
		body         map[string]interface{}
		expectedCode int
		expectedResp interface{}
	}{
		{
			name:         "missing_pubkey",
			body:         map[string]interface{}{},
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "missing_event",
			body:         map[string]interface{}{"walletPubkey": ALBY_NWC_PUBKEY},
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "valid_request",
			body:         map[string]interface{}{
				"walletPubkey": ALBY_NWC_PUBKEY,
				"event":        generateRequestEvent(),
			},
			expectedCode: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.body)
			runTest(t, e, http.MethodPost, "/nip47", bytes.NewBuffer(body), tt.expectedCode, testSvc.NIP47Handler)
		})
	}
}

func TestSubscriptions(t *testing.T) {
	if testSvc == nil {
		t.Fatal("testService is not initialized")
	}

	// register the webhook route
	e := echo.New()
	e.POST("/webhook", webhookHandler)
	ts := httptest.NewServer(e)
	defer ts.Close()

	// subscribe to response events to the pubkey
	tags := make(nostr.TagMap)
	(tags)["p"] = []string{publicKey}

	body, _ := json.Marshal(map[string]interface{}{
		"webhookUrl": fmt.Sprintf("%s/webhook", ts.URL),
    "filter": nostr.Filter{
			Kinds:   []int{23195},
			Authors: []string{ALBY_NWC_PUBKEY},
			Tags:    tags,
    },
	})
	req := httptest.NewRequest("POST", "/subscriptions", bytes.NewBuffer(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	assert.NoError(t, testSvc.SubscriptionHandler(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	var subscriptionResp SubscriptionResponse
	err := json.Unmarshal(rec.Body.Bytes(), &subscriptionResp)
	assert.NoError(t, err)

	// make an nip47 request from our pubkey
	body, _ = json.Marshal(map[string]interface{}{
    "event":        generateRequestEvent(),
		"walletPubkey": ALBY_NWC_PUBKEY,
	})
	req = httptest.NewRequest("POST", "/nip47", bytes.NewBuffer(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec = httptest.NewRecorder()
	c = e.NewContext(req, rec)

	assert.NoError(t, testSvc.NIP47Handler(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp NIP47Response
	err = json.Unmarshal(rec.Body.Bytes(), &resp)
	assert.NoError(t, err)

	// wait for event to be posted on the webhook
	time.Sleep(1 * time.Second)

	assert.NotNil(t, webhookResult)
	// the nip47 response should be the same as webhook response
	assert.Equal(t, resp.Event.ID, webhookResult.ID)

	req = httptest.NewRequest("DELETE", fmt.Sprintf("/subscriptions/%s", subscriptionResp.SubscriptionId), nil)
	rec = httptest.NewRecorder()
	c = e.NewContext(req, rec)

	// manually set the id param
	c.SetParamNames("id")
	c.SetParamValues(subscriptionResp.SubscriptionId)

	assert.NoError(t, testSvc.StopSubscriptionHandler(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func runTest(t *testing.T, e *echo.Echo, method string, target string, body io.Reader, expectedCode int, handler echo.HandlerFunc) {
	req := httptest.NewRequest(method, target, body)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if assert.NoError(t, handler(c)) {
		assert.Equal(t, expectedCode, rec.Code)
	}
}

func generateRequestEvent() (*nostr.Event) {
	var params map[string]interface{}
	jsonStr := `{
    "method": "get_info"
	}`
	decoder := json.NewDecoder(strings.NewReader(jsonStr))
	err := decoder.Decode(&params)
	if err != nil {
		return &nostr.Event{}
	}

	payloadJSON, err := json.Marshal(params)
	if err != nil {
		return &nostr.Event{}
	}

	ss, err := nip04.ComputeSharedSecret(ALBY_NWC_PUBKEY, privateKey)
	if err != nil {
		return &nostr.Event{}
	}

	payload, err := nip04.Encrypt(string(payloadJSON), ss)
	if err != nil {
		return &nostr.Event{}
	}

	req := &nostr.Event{
		PubKey:    ALBY_NWC_PUBKEY,
		CreatedAt: nostr.Now(),
		Kind:      NIP_47_REQUEST_KIND,
		Tags:      nostr.Tags{[]string{"p", ALBY_NWC_PUBKEY}},
		Content:   payload,
	}

	err = req.Sign(privateKey)
	if err != nil {
		return &nostr.Event{}
	}

	return req
}

func webhookHandler(c echo.Context) error {
	var data nostr.Event
	if err := c.Bind(&data); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid payload"})
	}

	// use mutex here if we use it more than once
	webhookResult = data

	return c.JSON(http.StatusOK, nil)
}
