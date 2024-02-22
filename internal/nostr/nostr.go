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

	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
	"github.com/labstack/echo/v4"
	"github.com/nbd-wtf/go-nostr"
	"github.com/sirupsen/logrus"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/jackc/pgx/v5/stdlib"
	sqltrace "gopkg.in/DataDog/dd-trace-go.v1/contrib/database/sql"
	gormtrace "gopkg.in/DataDog/dd-trace-go.v1/contrib/gorm.io/gorm.v1"
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
	Relay  *nostr.Relay
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

	if os.Getenv("DATADOG_AGENT_URL") != "" {
		sqltrace.Register("pgx", &stdlib.Driver{}, sqltrace.WithServiceName("nostr-wallet-connect"))
		sqlDb, err = sqltrace.Open("pgx", cfg.DatabaseUri)
		if err != nil {
			logger.Fatalf("Failed to open DB %v", err)
			return nil, err
		}
		db, err = gormtrace.Open(postgres.New(postgres.Config{Conn: sqlDb}), &gorm.Config{})
		if err != nil {
			logger.Fatalf("Failed to open DB %v", err)
			return nil, err
		}
	} else {
		db, err = gorm.Open(postgres.Open(cfg.DatabaseUri), &gorm.Config{})
		if err != nil {
			logger.Fatalf("Failed to open DB %v", err)
			return nil, err
		}
		sqlDb, err = db.DB()
		if err != nil {
			logger.Fatalf("Failed to set DB config: %v", err)
			return nil, err
		}
	}

	sqlDb.SetMaxOpenConns(cfg.DatabaseMaxConns)
	sqlDb.SetMaxIdleConns(cfg.DatabaseMaxIdleConns)
	sqlDb.SetConnMaxLifetime(time.Duration(cfg.DatabaseConnMaxLifetime) * time.Second)

	err = migrations.Migrate(db)
	if err != nil {
		logger.Fatalf("Failed to migrate: %v", err)
		return nil, err
	}
	logger.Info("Any pending migrations ran successfully")

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
		Relay:  relay,
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

	if (requestData.SignedEvent == nil) {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "signed event is empty",
		})
	}

	subscription := Subscription{}
	requestEvent := RequestEvent{}
	findRequestResult := svc.db.Where("nostr_id = ?", requestData.SignedEvent.ID).Find(&requestEvent)
	if findRequestResult.RowsAffected != 0 {
		svc.Logger.Info("request event is already processed")
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "request event is already processed",
		})
	} else {
		subscription = Subscription{
			RelayUrl:   requestData.RelayUrl,
			WebhookUrl: requestData.WebhookUrl,
			Open:       true,
			Ids:        &[]string{},
			Authors:    &[]string{requestData.WalletPubkey},
			Kinds:      &[]int{NIP_47_RESPONSE_KIND},
			Tags:       &nostr.TagMap{"e": []string{requestEvent.NostrId}},
			Since:      time.Now(),
			Limit:      1,
		}
		svc.db.Create(&subscription)

		requestEvent = RequestEvent{
			NostrId: requestData.SignedEvent.ID,
			Content: requestData.SignedEvent.Content,
			SubscriptionId: subscription.ID,
		}
		svc.db.Create(&requestEvent)
	}

	if subscription.WebhookUrl != "" {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			event, publishState, _, err := svc.processRequest(ctx, &subscription, &requestEvent, requestData.SignedEvent)
			subscription.Open = false
			requestEvent.State = publishState
			svc.db.Save(&subscription)
			svc.db.Save(&requestEvent)
			if err != nil {
				svc.Logger.WithError(err).Error("failed to process request for webhook")
				// what to pass to the webhook?
				return
			}
			responseEvent := ResponseEvent{
				SubscriptionId: subscription.ID,
				RequestId: requestEvent.ID,
				NostrId: event.ID,
				Content: event.Content,
				RepliedAt: event.CreatedAt.Time(),
			}
			svc.db.Save(&responseEvent)
			svc.postEventToWebhook(event, requestData.WebhookUrl)
		}()
		return c.JSON(http.StatusOK, "webhook received")
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		event, publishState, code, err := svc.processRequest(ctx, &subscription, &requestEvent, requestData.SignedEvent)
		subscription.Open = false
		requestEvent.State = publishState
		svc.db.Save(&subscription)
		svc.db.Save(&requestEvent)
		if err != nil {
			return c.JSON(code, ErrorResponse{
					Message: err.Error(),
			})
		}
		responseEvent := ResponseEvent{
			SubscriptionId: subscription.ID,
			RequestId: requestEvent.ID,
			NostrId: event.ID,
			Content: event.Content,
			RepliedAt: event.CreatedAt.Time(),
		}
		svc.db.Save(&responseEvent)
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
	if svc.Relay.IsConnected() {
		return svc.Relay, false, nil
	} else {
		svc.Logger.Info("lost connection to default relay, reconnecting...")
		relay, err := nostr.RelayConnect(context.Background(), svc.Cfg.DefaultRelayURL)
		return relay, false, err
	}
}

func (svc *Service) processRequest(ctx context.Context, subscription *Subscription, requestEvent *RequestEvent, signedEvent *nostr.Event) (*nostr.Event, string, int, error) {
	publishState := REQUEST_EVENT_PUBLISH_FAILED
	relay, isCustomRelay, err := svc.getRelayConnection(ctx, subscription.RelayUrl)
	if err != nil {
		return &nostr.Event{}, publishState, http.StatusBadRequest, fmt.Errorf("error connecting to relay: %w", err)
	}
	if isCustomRelay {
		defer relay.Close()
	}

	svc.Logger.WithFields(logrus.Fields{
		"e":       requestEvent.ID,
		"authors": subscription.Authors,
	}).Info("subscribing to events for response...")

	since := nostr.Timestamp(subscription.Since.Unix())
	filter := nostr.Filter{
		// IDs:     *subscription.Ids,
		Kinds:   *subscription.Kinds,
		Authors: *subscription.Authors,
		Tags:    *subscription.Tags,
		Since:   &since,
		Limit:   subscription.Limit,
	}

	sub, err := relay.Subscribe(ctx, []nostr.Filter{filter})
	if err != nil {
		return &nostr.Event{}, publishState, http.StatusBadRequest, fmt.Errorf("error subscribing to relay: %w", err)
	}

	status, err := relay.Publish(ctx, *signedEvent)
	if err != nil {
		return &nostr.Event{}, publishState, http.StatusBadRequest, fmt.Errorf("error publishing request event: %w", err)
	}

	if status == nostr.PublishStatusSucceeded {
		svc.Logger.WithFields(logrus.Fields{
			"status":  status,
			"eventId": requestEvent.ID,
		}).Info("published request")
		publishState = REQUEST_EVENT_PUBLISH_CONFIRMED
	} else if status == nostr.PublishStatusFailed {
		svc.Logger.WithFields(logrus.Fields{
			"status":  status,
			"eventId": requestEvent.ID,
		}).Info("failed to publish request")
		return &nostr.Event{}, publishState, http.StatusBadRequest, fmt.Errorf("error publishing request event: %s", err.Error())
	} else {
		svc.Logger.WithFields(logrus.Fields{
			"status":  status,
			"eventId": requestEvent.ID,
		}).Info("request sent but no response from relay (timeout)")
		// If we can somehow handle this case, then publishState can be removed
		publishState = REQUEST_EVENT_PUBLISH_UNCONFIRMED
	}

	select {
	case <-ctx.Done():
		return &nostr.Event{}, publishState, http.StatusRequestTimeout, fmt.Errorf("request canceled or timed out")
	case event := <-sub.Events:
		svc.Logger.WithFields(logrus.Fields{
			"eventId":   event.ID,
			"eventKind": event.Kind,
		}).Infof("successfully received event")
		return event, publishState, http.StatusOK, nil
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
