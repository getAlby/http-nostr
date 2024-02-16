package nostr

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"http-nostr/migrations"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
	"github.com/labstack/echo/v4"
	"github.com/nbd-wtf/go-nostr"
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

type Config struct {
	SentryDSN               string `envconfig:"SENTRY_DSN"`
	DatadogAgentUrl         string `envconfig:"DATADOG_AGENT_URL"`
	DefaultRelayURL         string `envconfig:"DEFAULT_RELAY_URL"`
	DatabaseUri             string `envconfig:"DATABASE_URI" default:"http-nostr.db"`
	DatabaseMaxConns        int    `envconfig:"DATABASE_MAX_CONNS" default:"10"`
	DatabaseMaxIdleConns    int    `envconfig:"DATABASE_MAX_IDLE_CONNS" default:"5"`
	DatabaseConnMaxLifetime int    `envconfig:"DATABASE_CONN_MAX_LIFETIME" default:"1800"` // 30 minutes
	Port                    int    `default:"8080"`
}

type Service struct {
	db     *gorm.DB
	Ctx    context.Context
	Wg     *sync.WaitGroup
	relay  *nostr.Relay
	Cfg    *Config
	Logger *logrus.Logger
}

func NewService(ctx context.Context) (*Service, error) {
	// Load env file as env variables
	godotenv.Load(".env")

	cfg := &Config{}
	err := envconfig.Process("", cfg)
	if err != nil {
		return nil, err
	}

	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{})
	logger.SetOutput(os.Stdout)
	logger.SetLevel(logrus.InfoLevel)

	var db *gorm.DB
	var sqlDb *sql.DB
	db, err = gorm.Open(sqlite.Open(cfg.DatabaseUri), &gorm.Config{})
	if err != nil {
		return nil, err
	}

	// db.Exec("PRAGMA foreign_keys=ON;") // if we wish to enable foreign keys
	sqlDb, err = db.DB()
	if err != nil {
		return nil, err
	}

	sqlDb.SetMaxOpenConns(cfg.DatabaseMaxConns)
	sqlDb.SetMaxIdleConns(cfg.DatabaseMaxIdleConns)
	sqlDb.SetConnMaxLifetime(time.Duration(cfg.DatabaseConnMaxLifetime) * time.Second)

	err = migrations.Migrate(db)
	if err != nil {
		logger.Fatalf("Failed to migrate: %v", err)
		return nil, err
	}
	logger.Println("Any pending migrations ran successfully")

	ctx, _ = signal.NotifyContext(ctx, os.Interrupt)

	logger.Info("Connecting to the relay...")
	relay, err := nostr.RelayConnect(ctx, cfg.DefaultRelayURL)
	if err != nil {
		logger.Fatalf("Failed to connect to default relay: %v", err)
		return nil, err
	}

	var wg sync.WaitGroup
	svc := &Service{
		Cfg:    cfg,
		db:     db,
		Ctx:    ctx,
		Wg:     &wg,
		Logger: logger,
		relay:  relay,
	}

	return svc, nil
}

func (svc *Service) InfoHandler(c echo.Context) error {
	var requestData InfoRequest
	if err := c.Bind(&requestData); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: fmt.Sprintf("error decoding info request: %s", err.Error()),
		})
	}

	relay, isCustomRelay, err := svc.getRelayConnection(c.Request().Context(), requestData.RelayURL)
	if err != nil {
		if isCustomRelay {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("error connecting to relay: %s", err))
		}
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("error connecting to default relay: %s", err))
	}
	if isCustomRelay {
		defer relay.Close()
	}

	svc.Logger.Info("subscribing to info event...")
	filter := nostr.Filter{
		Authors: []string{requestData.WalletPubkey},
		Kinds:   []int{NIP_47_INFO_EVENT_KIND},
		Limit:   1,
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 60*time.Second)
	defer cancel()
	sub, err := relay.Subscribe(ctx, []nostr.Filter{filter})
	if err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: fmt.Sprintf("error subscribing to relay: %s", err.Error()),
		})
	}

	select {
	case <-ctx.Done():
		svc.Logger.Info("exiting subscription.")
		return c.JSON(http.StatusRequestTimeout, ErrorResponse{
			Message: "request canceled or timed out",
		})
	case event := <-sub.Events:
		return c.JSON(http.StatusOK, event)
	}
}

func (svc *Service) NIP47Handler(c echo.Context) error {
	var requestData NIP47Request
	if err := c.Bind(&requestData); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: fmt.Sprintf("error decoding nip47 request: %s", err.Error()),
		})
	}

	if (requestData.WalletPubkey == "") {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "wallet pubkey is empty",
		})
	}

	
	subscription := Subscription{
		WalletPubkey: requestData.WalletPubkey,
		NostrId:      requestData.SignedEvent.ID,
		State:        SUBSCRIPTION_STATE_RECEIVED,
		RelayUrl:     requestData.RelayUrl,
		WebhookUrl:   requestData.WebhookUrl,
		Kind:         NIP_47_RESPONSE_KIND,
	}
	// mu.Lock()
	svc.db.Create(&subscription)
	// mu.Unlock()

	if requestData.WebhookUrl != "" {
		go func() {
			event, _, err := svc.processRequest(context.Background(), &subscription, requestData.SignedEvent)
			if err != nil {
				svc.Logger.WithError(err).Error("failed to process request for webhook")
				// what to pass to the webhook?
				return
			}
			svc.postEventToWebhook(event, requestData.WebhookUrl)
		}()
		return c.JSON(http.StatusOK, "webhook received")
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		event, code, err := svc.processRequest(ctx, &subscription, requestData.SignedEvent)
		if err != nil {
			return c.JSON(code, ErrorResponse{
					Message: err.Error(),
			})
		}
		return c.JSON(http.StatusOK, event)
	}
}

func (svc *Service) getRelayConnection(ctx context.Context, customRelayURL string) (*nostr.Relay, bool, error) {
	if customRelayURL != "" && customRelayURL != svc.Cfg.DefaultRelayURL {
		svc.Logger.WithFields(logrus.Fields{
			"customRelayURL": customRelayURL,
		}).Infof("connecting to custom relay")
		relay, err := nostr.RelayConnect(ctx, customRelayURL)
		return relay, true, err // true means custom and the relay should be closed
	}
	// check if the default relay is active
	if svc.relay.IsConnected() {
		return svc.relay, false, nil
	} else {
		svc.Logger.Info("lost connection to default relay, reconnecting...")
		relay, err := nostr.RelayConnect(context.Background(), svc.Cfg.DefaultRelayURL)
		return relay, false, err
	}
}

func (svc *Service) processRequest(ctx context.Context, subscription *Subscription, requestEvent *nostr.Event) (*nostr.Event, int, error) {
	relay, isCustomRelay, err := svc.getRelayConnection(ctx, subscription.RelayUrl)
	if err != nil {
		return &nostr.Event{}, http.StatusBadRequest, fmt.Errorf("error connecting to relay: %w", err)
	}
	if isCustomRelay {
		defer relay.Close()
	}

	svc.Logger.WithFields(logrus.Fields{
		"e":      requestEvent.ID,
		"author": subscription.WalletPubkey,
	}).Info("subscribing to events for response...")

	filter := nostr.Filter{
		Authors: []string{subscription.WalletPubkey},
		Kinds:   []int{subscription.Kind},
		Tags:    nostr.TagMap{"e": []string{requestEvent.ID}},
	}

	sub, err := relay.Subscribe(ctx, []nostr.Filter{filter})
	if err != nil {
		return &nostr.Event{}, http.StatusBadRequest, fmt.Errorf("error subscribing to relay: %w", err)
	}

	status, err := relay.Publish(ctx, *requestEvent)
	if err != nil {
		return &nostr.Event{}, http.StatusBadRequest, fmt.Errorf("error publishing request event: %w", err)
	}

	if status == nostr.PublishStatusSucceeded {
		svc.Logger.WithFields(logrus.Fields{
			"status":  status,
			"eventId": requestEvent.ID,
		}).Info("published request")
		subscription.PublishState = SUBSCRIPTION_STATE_PUBLISH_CONFIRMED
	} else if status == nostr.PublishStatusFailed {
		svc.Logger.WithFields(logrus.Fields{
			"status":  status,
			"eventId": requestEvent.ID,
		}).Info("failed to publish request")
		subscription.PublishState = SUBSCRIPTION_STATE_PUBLISH_FAILED
		subscription.State = SUBSCRIPTION_STATE_ERROR
		svc.db.Save(subscription)
		return &nostr.Event{}, http.StatusBadRequest, fmt.Errorf("error publishing request event: %s", err.Error())
	} else {
		svc.Logger.WithFields(logrus.Fields{
			"status":  status,
			"eventId": requestEvent.ID,
		}).Info("request sent but no response from relay (timeout)")
		subscription.PublishState = SUBSCRIPTION_STATE_PUBLISH_UNCONFIRMED
	}

	svc.db.Save(subscription)

	select {
	case <-ctx.Done():
		subscription.State = SUBSCRIPTION_STATE_ERROR
		svc.db.Save(subscription)
		return &nostr.Event{}, http.StatusRequestTimeout, fmt.Errorf("request canceled or timed out")
	case event := <-sub.Events:
		svc.Logger.WithFields(logrus.Fields{
			"eventId":   event.ID,
			"eventKind": event.Kind,
		}).Infof("successfully received event")
		subscription.State = SUBSCRIPTION_STATE_EXECUTED
		subscription.RepliedAt = time.Now()
		svc.db.Save(subscription)
		return event, http.StatusOK, nil
	}
}

func (svc *Service) postEventToWebhook(event *nostr.Event, webhookURL string) {
	eventData, err := json.Marshal(event)
	if err != nil {
		svc.Logger.WithError(err).Error("failed to marshal event for webhook")
		return
	}

	_, err = http.Post(webhookURL, "application/json", bytes.NewBuffer(eventData))
	if err != nil {
		svc.Logger.WithError(err).Error("failed to post event to webhook")
	}

	svc.Logger.WithFields(logrus.Fields{
		"eventId": event.ID,
		"eventKind": event.Kind,
	}).Infof("successfully posted event to webhook")
}
