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
	DefaultRelayURL         string `envconfig:"DEFAULT_RELAY_URL" default:"wss://relay.getalby.com/v1"`
	DatabaseUri             string `envconfig:"DATABASE_URI" default:"http-nostr.db"`
	DatabaseMaxConns        int    `envconfig:"DATABASE_MAX_CONNS" default:"10"`
	DatabaseMaxIdleConns    int    `envconfig:"DATABASE_MAX_IDLE_CONNS" default:"5"`
	DatabaseConnMaxLifetime int    `envconfig:"DATABASE_CONN_MAX_LIFETIME" default:"1800"` // 30 minutes
	Port                    int    `default:"8081"`
}

type Service struct {
	db                 *gorm.DB
	Ctx                context.Context
	Wg                 *sync.WaitGroup
	Relay              *nostr.Relay
	Cfg                *Config
	Logger             *logrus.Logger
	subscriptions      map[uint]*nostr.Subscription
	subscriptionsMutex sync.Mutex
	relayMutex         sync.Mutex
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
			logger.WithError(err).Error("Failed to open DB")
			return nil, err
		}
		db, err = gormtrace.Open(postgres.New(postgres.Config{Conn: sqlDb}), &gorm.Config{}, gormtrace.WithServiceName("http-nostr"))
		if err != nil {
			logger.WithError(err).Error("Failed to open DB")
			return nil, err
		}
	} else {
		db, err = gorm.Open(postgres.Open(cfg.DatabaseUri), &gorm.Config{})
		if err != nil {
			logger.WithError(err).Error("Failed to open DB")
			return nil, err
		}
		sqlDb, err = db.DB()
		if err != nil {
			logger.WithError(err).Error("Failed to set DB config")
			return nil, err
		}
	}

	sqlDb.SetMaxOpenConns(cfg.DatabaseMaxConns)
	sqlDb.SetMaxIdleConns(cfg.DatabaseMaxIdleConns)
	sqlDb.SetConnMaxLifetime(time.Duration(cfg.DatabaseConnMaxLifetime) * time.Second)

	err = migrations.Migrate(db)
	if err != nil {
		logger.WithError(err).Error("Failed to migrate")
		return nil, err
	}
	logger.Info("Any pending migrations ran successfully")

	ctx, _ = signal.NotifyContext(ctx, os.Interrupt)

	logger.Info("Connecting to the relay...")
	relay, err := nostr.RelayConnect(ctx, cfg.DefaultRelayURL)
	if err != nil {
		logger.WithError(err).Error("Failed to connect to default relay")
		return nil, err
	}

	subscriptions := make(map[uint]*nostr.Subscription)

	var wg sync.WaitGroup
	svc := &Service{
		Cfg:           cfg,
		db:            db,
		Ctx:           ctx,
		Wg:            &wg,
		Logger:        logger,
		Relay:         relay,
		subscriptions: subscriptions,
	}

	logger.Info("Starting all open subscriptions...")

	var openSubscriptions []Subscription
	if err := svc.db.Where("open = ?", true).Find(&openSubscriptions).Error; err != nil {
		logger.WithError(err).Error("Failed to query open subscriptions")
		return nil, err
	}

	for _, sub := range openSubscriptions {
		// Create a copy of the loop variable to
		// avoid passing address of the same variable
    subscription := sub
		svc.Logger.WithFields(logrus.Fields{
			"subscriptionId": subscription.ID,
		}).Info("Starting subscription")
		go svc.startSubscription(svc.Ctx, &subscription)
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

	svc.Logger.WithFields(logrus.Fields{
		"walletPubkey": requestData.WalletPubkey,
	}).Info("Subscribing to info event...")

	filter := nostr.Filter{
		Authors: []string{requestData.WalletPubkey},
		Kinds:   []int{NIP_47_INFO_EVENT_KIND},
		Limit:   1,
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 90*time.Second)
	defer cancel()
	sub, err := relay.Subscribe(ctx, []nostr.Filter{filter})
	if err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: fmt.Sprintf("error subscribing to relay: %s", err.Error()),
		})
	}

	select {
	case <-ctx.Done():
		svc.Logger.Info("Exiting info subscription.")
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
			ctx, cancel := context.WithTimeout(svc.Ctx, 90*time.Second)
			defer cancel()
			event, _, err := svc.processRequest(ctx, &subscription, &requestEvent, &requestData)
			if err != nil {
				svc.Logger.WithError(err).Error("Failed to process request for webhook")
				// what to pass to the webhook?
				return
			}
			svc.postEventToWebhook(event, requestData.WebhookUrl)
		}()
		return c.JSON(http.StatusOK, "webhook received")
	}

	ctx, cancel := context.WithTimeout(svc.Ctx, 90*time.Second)
	defer cancel()
	event, code, err := svc.processRequest(ctx, &subscription, &requestEvent, &requestData)
	if err != nil {
		return c.JSON(code, ErrorResponse{
				Message: err.Error(),
		})
	}
	return c.JSON(http.StatusOK, event)
}

func (svc *Service) NIP47NotificationsHandler(c echo.Context) error {
	var requestData NIP47SubscriptionRequest
	// send in a pubkey and authenticate by signing
	if err := c.Bind(&requestData); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Error decoding subscription request",
			Error:   err.Error(),
		})
	}

	if (requestData.WebhookUrl == "") {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "webhook url is empty",
			Error:   "no webhook url in request data",
		})
	}

	if (requestData.WalletPubkey == "") {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Wallet pubkey is empty",
			Error:   "no wallet pubkey in request data",
		})
	}

	if (requestData.ConnPubkey == "") {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Connection pubkey is empty",
			Error:   "no connection pubkey in request data",
		})
	}

	subscription := Subscription{
		RelayUrl:   requestData.RelayUrl,
		WebhookUrl: requestData.WebhookUrl,
		Open:       true,
		Authors:    &[]string{requestData.WalletPubkey},
		Kinds:      &[]int{23196},
	}

	tags := new(nostr.TagMap)
	*tags = make(nostr.TagMap)
	(*tags)["p"] = []string{requestData.ConnPubkey}

	subscription.Tags = tags


	err := svc.db.Create(&subscription).Error

	if err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Failed to store subscription",
			Error:   err.Error(),
		})
	}

	go svc.startSubscription(svc.Ctx, &subscription)

	return c.JSON(http.StatusOK, SubscriptionResponse{
		SubscriptionId: subscription.Uuid,
		WebhookUrl: requestData.WebhookUrl,
	})
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

	err := svc.db.Create(&subscription).Error

	if err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: fmt.Sprintf("error setting up subscription %s",err.Error()),
		})
	}

	go svc.startSubscription(svc.Ctx, &subscription)

	return c.JSON(http.StatusOK, SubscriptionResponse{
		SubscriptionId: subscription.Uuid,
		WebhookUrl: requestData.WebhookUrl,
	})
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

	svc.subscriptionsMutex.Lock()
	sub, exists := svc.subscriptions[subscription.ID]
	if exists {
		sub.Unsub()
		delete(svc.subscriptions, subscription.ID)
	}
	svc.subscriptionsMutex.Unlock()

	if (!exists && !subscription.Open) {
		return c.JSON(http.StatusAlreadyReported, ErrorResponse{
			Message: "subscription stopped already",
		})
	}

	subscription.Open = false
	svc.db.Save(&subscription)
	// delete svix app

	return c.NoContent(http.StatusNoContent)
}

func (svc *Service) startSubscription(ctx context.Context, subscription *Subscription) {
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

	for {
		relay, isCustomRelay, err := svc.getRelayConnection(ctx, subscription.RelayUrl)
		if err != nil {
			// TODO: notify user about relay failure
			svc.Logger.WithError(err).WithFields(logrus.Fields{
				"subscriptionId": subscription.ID,
				"relayUrl":       subscription.RelayUrl,
			}).Error("Failed get relay connection, retrying in 5s...")
			time.Sleep(5 * time.Second) // sleep for 5 seconds
			continue
		}

		for {
			sub, err := relay.Subscribe(ctx, []nostr.Filter{filter})
			if err != nil {
				// TODO: notify user about subscription failure
				svc.Logger.WithError(err).WithFields(logrus.Fields{
					"subscriptionId": subscription.ID,
					"relayUrl":       subscription.RelayUrl,
				}).Error("Failed to subscribe to relay, retrying in 5s...")
				time.Sleep(5 * time.Second) // sleep for 5 seconds
				break
			}

			svc.subscriptionsMutex.Lock()
			svc.subscriptions[subscription.ID] = sub
			svc.subscriptionsMutex.Unlock()

			svc.Logger.WithFields(logrus.Fields{
				"subscriptionId": subscription.ID,
			}).Info("Started subscription")

			err = svc.processEvents(ctx, subscription, sub)
			// closing relay as we reach here due to either
			// halting subscription or relay error
			if isCustomRelay {
				relay.Close()
			}

			if err != nil {
				// TODO: notify user about subscription failure
				svc.Logger.WithError(err).WithFields(logrus.Fields{
					"subscriptionId": subscription.ID,
					"relayUrl":       subscription.RelayUrl,
				}).Error("Subscription stopped due to relay error, reconnecting in 5s...")
				time.Sleep(5 * time.Second) // sleep for 5 seconds
				break
			} else {
				svc.Logger.WithFields(logrus.Fields{
					"subscriptionId": subscription.ID,
					"relayUrl":       subscription.RelayUrl,
				}).Info("Stopping subscription")
				return
			}
		}
	}
}

func (svc *Service) processEvents(ctx context.Context, subscription *Subscription, sub *nostr.Subscription) error {
	receivedEOS := false
	// Do not process historic events
	go func() {
		<-sub.EndOfStoredEvents
		svc.Logger.WithFields(logrus.Fields{
			"subscriptionId": subscription.ID,
		}).Info("Received EOS")
		receivedEOS = true
	}()

	go func(){
		for event := range sub.Events {
			if receivedEOS {
				svc.Logger.WithFields(logrus.Fields{
					"eventId":        event.ID,
					"eventKind":      event.Kind,
					"subscriptionId": subscription.ID,
				}).Info("Received event")
				responseEvent := ResponseEvent{
					SubscriptionId: subscription.ID,
					NostrId: event.ID,
					Content: event.Content,
					RepliedAt: event.CreatedAt.Time(),
				}
				svc.db.Save(&responseEvent)
				svc.postEventToWebhook(event, subscription.WebhookUrl)
			}
		}
	}()

	select {
	case <-sub.Relay.Context().Done():
		return sub.Relay.ConnectionError
	case <-ctx.Done():
		return nil
	case <-sub.Context.Done():
		return nil
	}
}

func (svc *Service) getRelayConnection(ctx context.Context, customRelayURL string) (*nostr.Relay, bool, error) {
	if customRelayURL != "" && customRelayURL != svc.Cfg.DefaultRelayURL {
		svc.Logger.WithFields(logrus.Fields{
			"customRelayURL": customRelayURL,
		}).Infof("Connecting to custom relay")
		relay, err := nostr.RelayConnect(ctx, customRelayURL)
		return relay, true, err // true means custom and the relay should be closed
	}
	// use mutex otherwise the svc.Relay will be reconnected more than once
	svc.relayMutex.Lock()
	defer svc.relayMutex.Unlock()
	// check if the default relay is active, else reconnect and return the relay
	if svc.Relay.IsConnected() {
		return svc.Relay, false, nil
	} else {
		svc.Logger.Info("Lost connection to default relay, reconnecting...")
		relay, err := nostr.RelayConnect(svc.Ctx, svc.Cfg.DefaultRelayURL)
		svc.Relay = relay
		return svc.Relay, false, err
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
		"eventId": requestEvent.ID,
		"authors": subscription.Authors,
		"tags":    subscription.Tags,
		"kinds":   subscription.Kinds,
	}).Info("Subscribing to events for response...")

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
		}).Info("Published request")
	} else {
		svc.Logger.WithFields(logrus.Fields{
			"status":  publishState,
			"eventId": requestEvent.ID,
		}).Info("Failed to publish request")
		return &nostr.Event{}, http.StatusBadRequest, fmt.Errorf("error publishing request event: %s", err.Error())
	}

	select {
	case <-ctx.Done():
		return &nostr.Event{}, http.StatusRequestTimeout, fmt.Errorf("request canceled or timed out")
	case event := <-sub.Events:
		svc.Logger.WithFields(logrus.Fields{
			"eventId":   event.ID,
			"eventKind": event.Kind,
		}).Infof("Successfully received event")
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
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"eventId":    event.ID,
			"eventKind":  event.Kind,
			"webhookUrl": webhookURL,
		}).Error("Failed to marshal event for webhook")
		return
	}

	// TODO: add svix functionality
	_, err = http.Post(webhookURL, "application/json", bytes.NewBuffer(eventData))
	if err != nil {
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"eventId":    event.ID,
			"eventKind":  event.Kind,
			"webhookUrl": webhookURL,
		}).Error("Failed to post event to webhook")
	}

	svc.Logger.WithFields(logrus.Fields{
		"eventId":    event.ID,
		"eventKind":  event.Kind,
		"webhookUrl": webhookURL,
	}).Infof("Successfully posted event to webhook")
}
