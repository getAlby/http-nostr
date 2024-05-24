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
	"strings"
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
	DefaultRelayURL         string `envconfig:"DEFAULT_RELAY_URL" default:"wss://relay.getalby.com/v1"`
	DatabaseUri             string `envconfig:"DATABASE_URI" default:"http-nostr.db"`
	DatabaseMaxConns        int    `envconfig:"DATABASE_MAX_CONNS" default:"10"`
	DatabaseMaxIdleConns    int    `envconfig:"DATABASE_MAX_IDLE_CONNS" default:"5"`
	DatabaseConnMaxLifetime int    `envconfig:"DATABASE_CONN_MAX_LIFETIME" default:"1800"` // 30 minutes
	Port                    int    `default:"8081"`
}

type Service struct {
	db            *gorm.DB
	Ctx           context.Context
	Wg            *sync.WaitGroup
	Relay         *nostr.Relay
	Cfg           *Config
	Logger        *logrus.Logger
	subscriptions map[uint]context.CancelFunc
	mu            sync.Mutex
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

	if cfg.DatadogAgentUrl != "" {
		sqltrace.Register("pgx", &stdlib.Driver{}, sqltrace.WithServiceName("http-nostr"))
		sqlDb, err = sqltrace.Open("pgx", cfg.DatabaseUri)
		if err != nil {
			logger.Fatalf("Failed to open DB %v", err)
			return nil, err
		}
		db, err = gormtrace.Open(postgres.New(postgres.Config{Conn: sqlDb}), &gorm.Config{}, gormtrace.WithServiceName("http-nostr"))
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

	subscriptions := make(map[uint]context.CancelFunc)

	var wg sync.WaitGroup
	svc := &Service{
		Cfg:           cfg,
		db:            db,
		Ctx:           ctx,
		Wg:            &wg,
		Logger:        logger,
		subscriptions: subscriptions,
		Relay:         relay,
	}

	logger.Info("starting all open subscriptions...")

	var openSubscriptions []Subscription
	if err := svc.db.Where("open = ?", true).Find(&openSubscriptions).Error; err != nil {
		logger.Errorf("Failed to query open subscriptions: %v", err)
		return nil, err
	}

	for _, sub := range openSubscriptions {
		go func(sub Subscription) {
			ctx, cancel := context.WithCancel(svc.Ctx)
			svc.mu.Lock()
			svc.subscriptions[sub.ID] = cancel
			svc.mu.Unlock()
			errorChan := make(chan error)
			go svc.handleSubscription(ctx, &sub, errorChan)

			err := <-errorChan
			if err != nil {
				svc.stopSubscription(&sub)
				svc.Logger.Errorf("error opening subscription %d: %v", sub.ID, err)
			}
			svc.Logger.Infof("opened subscription %d", sub.ID)
		}(sub)
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

	if (requestData.WalletPubkey == "") {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "wallet pubkey is empty",
		})
	}

	relay, isCustomRelay, err := svc.getRelayConnection(c.Request().Context(), requestData.RelayUrl)
	if err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: fmt.Sprintf("error connecting to relay: %s", err.Error()),
		})
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

func (svc *Service) PublishHandler(c echo.Context) error {
	var requestData PublishRequest
	if err := c.Bind(&requestData); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: fmt.Sprintf("error decoding publish request: %s", err.Error()),
		})
	}

	if len(requestData.Relays) == 0 {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "relay(s) not specified",
		})
	}

	if (requestData.SignedEvent == nil) {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "signed event is empty",
		})
	}

	var wg sync.WaitGroup
	errorCh := make(chan error, len(requestData.Relays))

	for _, relayUrl := range requestData.Relays {
		wg.Add(1)
		go func(relayUrl string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(svc.Ctx, 30*time.Second)
			defer cancel()

			relay, isCustomRelay, err := svc.getRelayConnection(ctx, relayUrl)
			if err != nil {
				errorCh <- fmt.Errorf("error connecting to relay %s: %v", relayUrl, err)
				return
			}
			if isCustomRelay {
				defer relay.Close()
			}
			err = relay.Publish(ctx, *requestData.SignedEvent)
			if err != nil {
				errorCh <- fmt.Errorf("error publishing to relay %s: %v", relayUrl, err)
			}
		}(relayUrl)
	}

	wg.Wait()
	close(errorCh)
	var errors []string
	for err := range errorCh {
		errors = append(errors, err.Error())
	}

	if len(errors) > 0 {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "errors occurred while publishing to relays: " + strings.Join(errors, "; "),
		})
	}

	return c.JSON(http.StatusOK, "published")
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
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "request event is already processed",
		})
	}

	subscription = Subscription{
		RelayUrl:   requestData.RelayUrl,
		WebhookUrl: requestData.WebhookUrl,
		Open:       true,
		Authors:    &[]string{requestData.WalletPubkey},
		Kinds:      &[]int{NIP_47_RESPONSE_KIND},
		Tags:       &nostr.TagMap{"e": []string{requestData.SignedEvent.ID}},
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

	if subscription.WebhookUrl != "" {
		go func() {
			ctx, cancel := context.WithTimeout(svc.Ctx, 120*time.Second)
			defer cancel()
			event, _, err := svc.processRequest(ctx, &subscription, &requestEvent, &requestData)
			if err != nil {
				svc.Logger.WithError(err).Error("failed to process request for webhook")
				// what to pass to the webhook?
				return
			}
			svc.postEventToWebhook(event, requestData.WebhookUrl)
		}()
		return c.JSON(http.StatusOK, "webhook received")
	}

	ctx, cancel := context.WithTimeout(svc.Ctx, 120*time.Second)
	defer cancel()
	event, code, err := svc.processRequest(ctx, &subscription, &requestEvent, &requestData)
	if err != nil {
		return c.JSON(code, ErrorResponse{
				Message: err.Error(),
		})
	}
	return c.JSON(http.StatusOK, event)
}

func (svc *Service) SubscriptionHandler(c echo.Context) error {
	var requestData SubscriptionRequest
	// send in a pubkey and authenticate by signing
	if err := c.Bind(&requestData); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: fmt.Sprintf("error decoding subscription request: %s", err.Error()),
		})
	}

	if (requestData.Filter == nil) {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "filters are empty",
		})
	}

	if (requestData.WebhookUrl == "") {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "webhook url is empty",
		})
	}

	subscription := Subscription{
		RelayUrl:   requestData.RelayUrl,
		WebhookUrl: requestData.WebhookUrl,
		Open:       true,
		Ids:        &requestData.Filter.IDs,
		Authors:    &requestData.Filter.Authors,
		Kinds:      &requestData.Filter.Kinds,
		Tags:       &requestData.Filter.Tags,
		Limit:      requestData.Filter.Limit,
		Search:     requestData.Filter.Search,
	}
	if requestData.Filter.Since != nil {
		subscription.Since = requestData.Filter.Since.Time()
	}
	if requestData.Filter.Until != nil {
			subscription.Until = requestData.Filter.Until.Time()
	}
	svc.db.Create(&subscription)

	errorChan := make(chan error, 1)
	ctx, cancel := context.WithCancel(svc.Ctx)
	svc.mu.Lock()
	svc.subscriptions[subscription.ID] = cancel
	svc.mu.Unlock()
	go svc.handleSubscription(ctx, &subscription, errorChan)

	err := <-errorChan
	if err != nil {
		svc.stopSubscription(&subscription)
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: fmt.Sprintf("error setting up subscription %s",err.Error()),
		})
	}

	return c.JSON(http.StatusOK, SubscriptionResponse{
		SubscriptionId: subscription.Uuid,
		WebhookUrl: requestData.WebhookUrl,
	})
}

func (svc *Service) handleSubscription(ctx context.Context, subscription *Subscription, errorChan chan error) {
	relay, isCustomRelay, err := svc.getRelayConnection(ctx, subscription.RelayUrl)
	if err != nil {
		errorChan <- err
		return
	}
	if isCustomRelay {
		defer relay.Close()
	}

	filter := nostr.Filter{
		Limit:  subscription.Limit,
		Search: subscription.Search,
	}
	if subscription.Ids != nil {
    filter.IDs = *subscription.Ids
	}
	if subscription.Kinds != nil {
		filter.Kinds = *subscription.Kinds
	}
	if subscription.Authors != nil {
		filter.Authors = *subscription.Authors
	}
	if subscription.Tags != nil {
    filter.Tags = *subscription.Tags
	}
	if !subscription.Since.IsZero() {
		since := nostr.Timestamp(subscription.Since.Unix())
		filter.Since = &since
	}
	if !subscription.Until.IsZero() {
		until := nostr.Timestamp(subscription.Until.Unix())
		filter.Until = &until
	}

	sub, err := relay.Subscribe(ctx, []nostr.Filter{filter})
	if err != nil {
		errorChan <- err
		return
	}
	errorChan <- nil
	go func(){
		for event := range sub.Events {
			svc.Logger.WithFields(logrus.Fields{
				"eventId":   event.ID,
				"eventKind": event.Kind,
			}).Infof("received event on subscription %d", subscription.ID)
			responseEvent := ResponseEvent{
				SubscriptionId: subscription.ID,
				NostrId: event.ID,
				Content: event.Content,
				RepliedAt: event.CreatedAt.Time(),
			}
			svc.db.Save(&responseEvent)
			svc.postEventToWebhook(event, subscription.WebhookUrl)
		}
	}()
	svc.Logger.Infof("subscription %d started", subscription.ID)
	<-ctx.Done()
	svc.Logger.Infof("subscription %d closed", subscription.ID)
	// delete svix app
}

func (svc *Service) StopSubscriptionHandler(c echo.Context) error {
	uuid := c.Param("id")

	subscription := Subscription{}
	if err := svc.db.First(&subscription, "uuid = ?", uuid).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return c.JSON(http.StatusNotFound, ErrorResponse{
				Message: "subscription does not exist",
			})
		} else {
			return c.JSON(http.StatusInternalServerError, ErrorResponse{
				Message: fmt.Sprintf("error occurred while fetching user: %s", err.Error()),
			})
		}
	}

	err := svc.stopSubscription(&subscription)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: fmt.Sprintf("subscription exists but: %s", err.Error()),
		})
	}

	return c.NoContent(http.StatusNoContent)
}

func (svc *Service) stopSubscription(sub *Subscription) error {
	svc.mu.Lock()
	cancel, exists := svc.subscriptions[sub.ID]
	if exists {
		cancel()
		delete(svc.subscriptions, sub.ID)
		sub.Open = false
		svc.db.Save(sub)
	}
	svc.mu.Unlock()
	if (!exists) {
		return fmt.Errorf("cancel function doesn't exist")
	}
	return nil
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
		relay, err := nostr.RelayConnect(svc.Ctx, svc.Cfg.DefaultRelayURL)
		return relay, false, err
	}
}

func (svc *Service) processRequest(ctx context.Context, subscription *Subscription, requestEvent *RequestEvent, requestData *NIP47Request) (*nostr.Event, int, error) {
	publishState := REQUEST_EVENT_PUBLISH_FAILED
	defer func() {
		subscription.Open = false
		requestEvent.State = publishState
		svc.db.Save(subscription)
		svc.db.Save(requestEvent)
	}()
	relay, isCustomRelay, err := svc.getRelayConnection(ctx, subscription.RelayUrl)
	if err != nil {
		return &nostr.Event{}, http.StatusBadRequest, fmt.Errorf("error connecting to relay: %w", err)
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
		Kinds:   *subscription.Kinds,
		Authors: *subscription.Authors,
		Tags:    *subscription.Tags,
		Since:   &since,
		Limit:   subscription.Limit,
		Search:  subscription.Search,
	}

	if subscription.Ids != nil {
		filter.IDs = *subscription.Ids
	}
	if !subscription.Until.IsZero() {
		until := nostr.Timestamp(subscription.Until.Unix())
		filter.Until = &until
	}

	sub, err := relay.Subscribe(ctx, []nostr.Filter{filter})
	if err != nil {
		return &nostr.Event{}, http.StatusBadRequest, fmt.Errorf("error subscribing to relay: %w", err)
	}

	err = relay.Publish(ctx, *requestData.SignedEvent)
	if err != nil {
		return &nostr.Event{}, http.StatusBadRequest, fmt.Errorf("error publishing request event: %w", err)
	}

	if err == nil {
		publishState = REQUEST_EVENT_PUBLISH_CONFIRMED
		svc.Logger.WithFields(logrus.Fields{
			"status":  publishState,
			"eventId": requestEvent.ID,
		}).Info("published request")
	} else {
		svc.Logger.WithFields(logrus.Fields{
			"status":  publishState,
			"eventId": requestEvent.ID,
		}).Info("failed to publish request")
		return &nostr.Event{}, http.StatusBadRequest, fmt.Errorf("error publishing request event: %s", err.Error())
	}

	select {
	case <-ctx.Done():
		return &nostr.Event{}, http.StatusRequestTimeout, fmt.Errorf("request canceled or timed out")
	case event := <-sub.Events:
		svc.Logger.WithFields(logrus.Fields{
			"eventId":   event.ID,
			"eventKind": event.Kind,
		}).Infof("successfully received event")
		responseEvent := ResponseEvent{
			SubscriptionId: subscription.ID,
			RequestId: &requestEvent.ID,
			NostrId: event.ID,
			Content: event.Content,
			RepliedAt: event.CreatedAt.Time(),
		}
		svc.db.Save(&responseEvent)
		return event, http.StatusOK, nil
	}
}

func (svc *Service) postEventToWebhook(event *nostr.Event, webhookURL string) {
	eventData, err := json.Marshal(event)
	if err != nil {
		svc.Logger.WithError(err).Error("failed to marshal event for webhook")
		return
	}

	// TODO: add svix functionality
	_, err = http.Post(webhookURL, "application/json", bytes.NewBuffer(eventData))
	if err != nil {
		svc.Logger.WithError(err).Error("failed to post event to webhook")
	}

	svc.Logger.WithFields(logrus.Fields{
		"eventId": event.ID,
		"eventKind": event.Kind,
	}).Infof("successfully posted event to webhook")
}
