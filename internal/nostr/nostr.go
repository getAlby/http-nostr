package nostr

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"http-nostr/migrations"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
	"github.com/labstack/echo/v4"
	"github.com/nbd-wtf/go-nostr"
	"github.com/sirupsen/logrus"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	expo "github.com/getAlby/exponent-server-sdk-golang/sdk"
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
	LogLevel                int    `envconfig:"LOG_LEVEL" default:"4"`
	Port                    int    `envconfig:"PORT" default:"8081"`
}

type Service struct {
	db                 *gorm.DB
	Ctx                context.Context
	Wg                 *sync.WaitGroup
	Relay              *nostr.Relay
	Cfg                *Config
	Logger             *logrus.Logger
	subscriptionsMutex sync.Mutex
	relayMutex         sync.Mutex
	client             *expo.PushClient
	subCancelFnMap     map[string]context.CancelFunc
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
	logger.SetLevel(logrus.Level(cfg.LogLevel))

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

	client := expo.NewPushClient(&expo.ClientConfig{
		Host:  "https://api.expo.dev",
		APIURL: "/v2",
	})

	var wg sync.WaitGroup
	svc := &Service{
		Cfg:           cfg,
		db:            db,
		Ctx:           ctx,
		Wg:            &wg,
		Logger:        logger,
		Relay:         relay,
		client:        client,
	}

	logger.Info("Starting all open subscriptions...")

	var openSubscriptions []Subscription
	if err := svc.db.Where("open = ?", true).Find(&openSubscriptions).Error; err != nil {
		logger.WithError(err).Error("Failed to query open subscriptions")
		return nil, err
	}
	cancelFnMap := make(map[string]context.CancelFunc)
	for _, sub := range openSubscriptions {
		// Create a copy of the loop variable to
		// avoid passing address of the same variable
    subscription := sub
		handleEvent := svc.handleSubscribedEvent
		if sub.PushToken != "" {
			handleEvent = svc.handleSubscribedEventForPushNotification
		}
		subCtx, subCancelFn := context.WithCancel(svc.Ctx)
		cancelFnMap[subscription.Uuid] = subCancelFn
		go svc.startSubscription(subCtx, &subscription, nil, handleEvent)
	}
	svc.subCancelFnMap = cancelFnMap

	return svc, nil
}

func (svc *Service) InfoHandler(c echo.Context) error {
	var requestData InfoRequest
	if err := c.Bind(&requestData); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Error decoding info request",
			Error:   err.Error(),
		})
	}

	if (requestData.WalletPubkey == "") {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Wallet pubkey is empty",
			Error:   "no wallet pubkey in request data",
		})
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), 90*time.Second)
	defer cancel()

	relay, isCustomRelay, err := svc.getRelayConnection(ctx, requestData.RelayUrl)
	if err != nil {
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"relay_url":     requestData.RelayUrl,
			"wallet_pubkey": requestData.WalletPubkey,
		}).Error("Error connecting to relay")
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "Error connecting to relay",
			Error:   err.Error(),
		})
	}
	if isCustomRelay {
		defer relay.Close()
	}

	svc.Logger.WithFields(logrus.Fields{
		"relay_url":     requestData.RelayUrl,
		"wallet_pubkey": requestData.WalletPubkey,
	}).Debug("Subscribing to info event")

	filter := nostr.Filter{
		Authors: []string{requestData.WalletPubkey},
		Kinds:   []int{NIP_47_INFO_EVENT_KIND},
		Limit:   1,
	}
	sub, err := relay.Subscribe(ctx, []nostr.Filter{filter})
	if err != nil {
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"relay_url":     requestData.RelayUrl,
			"wallet_pubkey": requestData.WalletPubkey,
		}).Error("Error subscribing to relay")
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "Error subscribing to relay",
			Error:   err.Error(),
		})
	}

	select {
	case <-ctx.Done():
		svc.Logger.WithError(ctx.Err()).WithFields(logrus.Fields{
			"relay_url":     requestData.RelayUrl,
			"wallet_pubkey": requestData.WalletPubkey,
		}).Error("Exiting info subscription without receiving event")
		return c.JSON(http.StatusRequestTimeout, ErrorResponse{
			Message: "Request canceled or timed out",
			Error:   ctx.Err().Error(),
		})
	case event := <-sub.Events:
		svc.Logger.WithFields(logrus.Fields{
			"relay_url":     requestData.RelayUrl,
			"wallet_pubkey": requestData.WalletPubkey,
			"response_event_id": event.ID,
		}).Info("Received info event")
		sub.Unsub()
		return c.JSON(http.StatusOK, InfoResponse{
			Event: event,
		})
	}
}

func (svc *Service) PublishHandler(c echo.Context) error {
	var requestData PublishRequest
	if err := c.Bind(&requestData); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Error decoding publish request",
			Error:   err.Error(),
		})
	}

	if (requestData.SignedEvent == nil) {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Signed event is empty",
			Error:   "no signed event in request data",
		})
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), 90*time.Second)
	defer cancel()

	relay, isCustomRelay, err := svc.getRelayConnection(ctx, requestData.RelayUrl)
	if err != nil {
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"event_id":  requestData.SignedEvent.ID,
			"relay_url": requestData.RelayUrl,
		}).Error("Error subscribing to relay")
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "Error connecting to relay",
			Error:   err.Error(),
		})
	}

	if isCustomRelay {
		defer relay.Close()
	}

	svc.Logger.WithFields(logrus.Fields{
		"event_id":  requestData.SignedEvent.ID,
		"relay_url": requestData.RelayUrl,
	}).Debug("Publishing event")

	err = relay.Publish(ctx, *requestData.SignedEvent)
	if err != nil {
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"event_id":  requestData.SignedEvent.ID,
			"relay_url": requestData.RelayUrl,
		}).Error("Failed to publish event")

		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "Error publishing the event",
			Error:   err.Error(),
		})
	}

	svc.Logger.WithFields(logrus.Fields{
		"event_id":  requestData.SignedEvent.ID,
		"relay_url": requestData.RelayUrl,
	}).Info("Published event")

	return c.JSON(http.StatusOK, PublishResponse{
		EventId:  requestData.SignedEvent.ID,
		RelayUrl: requestData.RelayUrl,
		State:    EVENT_PUBLISHED,
	})
}

func (svc *Service) NIP47Handler(c echo.Context) error {
	var requestData NIP47Request
	if err := c.Bind(&requestData); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Error decoding nip-47 request",
			Error:   err.Error(),
		})
	}

	if (requestData.WalletPubkey == "") {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Wallet pubkey is empty",
			Error:   "no wallet pubkey in request data",
		})
	}

	if (requestData.SignedEvent == nil) {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Signed event is empty",
			Error:   "no signed event in request data",
		})
	}

	svc.Logger.WithFields(logrus.Fields{
		"request_event_id": requestData.SignedEvent.ID,
		"client_pubkey":    requestData.SignedEvent.PubKey,
		"wallet_pubkey":    requestData.WalletPubkey,
		"relay_url":        requestData.RelayUrl,
	}).Debug("Processing request event")

	if svc.db.Where("nostr_id = ?", requestData.SignedEvent.ID).Find(&RequestEvent{}).RowsAffected != 0 {
		svc.Logger.WithFields(logrus.Fields{
			"request_event_id": requestData.SignedEvent.ID,
			"client_pubkey":    requestData.SignedEvent.PubKey,
			"wallet_pubkey":    requestData.WalletPubkey,
			"relay_url":        requestData.RelayUrl,
		}).Error("Event already processed")
		return c.JSON(http.StatusBadRequest, NIP47Response{
			State: EVENT_ALREADY_PROCESSED,
		})
	}

	requestEvent := RequestEvent{
		NostrId:     requestData.SignedEvent.ID,
		Content:     requestData.SignedEvent.Content,
		SignedEvent: requestData.SignedEvent,
	}

	if err := svc.db.Create(&requestEvent).Error; err != nil {
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"request_event_id": requestData.SignedEvent.ID,
			"client_pubkey":    requestData.SignedEvent.PubKey,
			"wallet_pubkey":    requestData.WalletPubkey,
			"relay_url":        requestData.RelayUrl,
		}).Error("Failed to store request event")
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "Failed to store request event",
			Error:   err.Error(),
		})
	}

	subscription := svc.prepareNIP47Subscription(requestData.RelayUrl, requestData.WalletPubkey, "", &requestEvent)

	ctx, cancel := context.WithTimeout(c.Request().Context(), 90*time.Second)
	defer cancel()
	go svc.startSubscription(ctx, &subscription, svc.publishRequestEvent, svc.handleResponseEvent)

	select {
	case <-ctx.Done():
		svc.Logger.WithError(ctx.Err()).WithFields(logrus.Fields{
			"subscription_id":  subscription.Uuid,
			"request_event_id": requestData.SignedEvent.ID,
			"client_pubkey":    requestData.SignedEvent.PubKey,
			"wallet_pubkey":    requestData.WalletPubkey,
			"relay_url":        requestData.RelayUrl,
		}).Error("Stopped subscription without receiving event")
		if ctx.Err() == context.DeadlineExceeded {
			return c.JSON(http.StatusGatewayTimeout, ErrorResponse{
				Message: "Request timed out",
				Error:   ctx.Err().Error(),
			})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "Context cancelled",
			Error:   ctx.Err().Error(),
		})
	case event := <-subscription.EventChan:
		return c.JSON(http.StatusOK, NIP47Response{
			Event: event,
			State: EVENT_PUBLISHED,
		})
	}
}

func (svc *Service) NIP47WebhookHandler(c echo.Context) error {
	var requestData NIP47WebhookRequest
	if err := c.Bind(&requestData); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Error decoding nip-47 request",
			Error:   err.Error(),
		})
	}

	if (requestData.WalletPubkey == "") {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Wallet pubkey is empty",
			Error:   "no wallet pubkey in request data",
		})
	}

	if (requestData.SignedEvent == nil) {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Signed event is empty",
			Error:   "no signed event in request data",
		})
	}

	if (requestData.WebhookUrl == "") {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Webhook URL is empty",
			Error:   "no webhook url in request data",
		})
	}

	svc.Logger.WithFields(logrus.Fields{
		"request_event_id": requestData.SignedEvent.ID,
		"wallet_pubkey":    requestData.WalletPubkey,
		"relay_url":        requestData.RelayUrl,
		"webhook_url":      requestData.WebhookUrl,
	}).Debug("Processing request event")

	if svc.db.Where("nostr_id = ?", requestData.SignedEvent.ID).Find(&RequestEvent{}).RowsAffected != 0 {
		svc.Logger.WithFields(logrus.Fields{
			"request_event_id": requestData.SignedEvent.ID,
			"wallet_pubkey":    requestData.WalletPubkey,
			"relay_url":        requestData.RelayUrl,
			"webhook_url":      requestData.WebhookUrl,
		}).Error("Event already processed")
		return c.JSON(http.StatusBadRequest, NIP47Response{
			State: EVENT_ALREADY_PROCESSED,
		})
	}

	requestEvent := RequestEvent{
		NostrId:     requestData.SignedEvent.ID,
		Content:     requestData.SignedEvent.Content,
		SignedEvent: requestData.SignedEvent,
	}

	if err := svc.db.Create(&requestEvent).Error; err != nil {
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"request_event_id": requestData.SignedEvent.ID,
			"wallet_pubkey":    requestData.WalletPubkey,
			"relay_url":        requestData.RelayUrl,
			"webhook_url":      requestData.WebhookUrl,
		}).Error("Failed to store request event")
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "Failed to store request event",
			Error:   err.Error(),
		})
	}

	subscription := svc.prepareNIP47Subscription(requestData.RelayUrl, requestData.WalletPubkey, requestData.WebhookUrl, &requestEvent)

	ctx, cancel := context.WithTimeout(svc.Ctx, 90*time.Second)

	go svc.startSubscription(ctx, &subscription, svc.publishRequestEvent, svc.handleResponseEvent)

	go func(){
		defer cancel()
		select {
		case <-ctx.Done():
			svc.Logger.WithError(ctx.Err()).WithFields(logrus.Fields{
				"subscription_id":  subscription.Uuid,
				"request_event_id": requestData.SignedEvent.ID,
				"client_pubkey":    requestData.SignedEvent.PubKey,
				"wallet_pubkey":    requestData.WalletPubkey,
				"relay_url":        requestData.RelayUrl,
			}).Error("Stopped subscription without receiving event")
		case event := <-subscription.EventChan:
			svc.postEventToWebhook(event, &subscription)
		}
	}()

	return c.JSON(http.StatusOK, NIP47Response{
		State: WEBHOOK_RECEIVED,
	})
}

func (svc *Service) prepareNIP47Subscription(relayUrl, walletPubkey, webhookUrl string, requestEvent *RequestEvent) (Subscription) {
	return Subscription{
		RelayUrl:     relayUrl,
		WebhookUrl:   webhookUrl,
		Open:         true,
		Authors:      &[]string{walletPubkey},
		Kinds:        &[]int{NIP_47_RESPONSE_KIND},
		Tags:         &nostr.TagMap{"e": []string{requestEvent.NostrId}},
		Since:        time.Now(),
		Limit:        1,
		RequestEvent: requestEvent,
		EventChan:    make(chan *nostr.Event, 1),
		Uuid:         uuid.New().String(),
	}
}

func (svc *Service) NIP47NotificationHandler(c echo.Context) error {
	var requestData NIP47NotificationRequest
	// send in a pubkey and authenticate by signing
	if err := c.Bind(&requestData); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Error decoding notification request",
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

	svc.Logger.WithFields(logrus.Fields{
		"wallet_pubkey": requestData.WalletPubkey,
		"relay_url":     requestData.RelayUrl,
		"webhook_url":   requestData.WebhookUrl,
	}).Debug("Subscribing to notifications")

	subscription := Subscription{
		RelayUrl:   requestData.RelayUrl,
		WebhookUrl: requestData.WebhookUrl,
		Open:       true,
		Since:      time.Now(),
		Authors:    &[]string{requestData.WalletPubkey},
		Kinds:      &[]int{LEGACY_NIP_47_NOTIFICATION_KIND},
	}

	if (requestData.Version == "1.0") {
		subscription.Kinds = &[]int{NIP_47_NOTIFICATION_KIND}
	}

	tags := make(nostr.TagMap)
	(tags)["p"] = []string{requestData.ConnPubkey}

	subscription.Tags = &tags

	err := svc.db.Create(&subscription).Error

	if err != nil {
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"wallet_pubkey": requestData.WalletPubkey,
			"relay_url":     requestData.RelayUrl,
			"webhook_url":   requestData.WebhookUrl,
		}).Error("Failed to store subscription")
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Failed to store subscription",
			Error:   err.Error(),
		})
	}

	subCtx, subCancelFn := context.WithCancel(svc.Ctx)
	svc.subscriptionsMutex.Lock()
	svc.subCancelFnMap[subscription.Uuid] = subCancelFn
	svc.subscriptionsMutex.Unlock()
	go svc.startSubscription(subCtx, &subscription, nil, svc.handleSubscribedEvent)

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
			Message: "Error decoding subscription request",
			Error:   err.Error(),
		})
	}

	if (requestData.Filter == nil) {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Filters are empty",
			Error:   "no filters in request data",
		})
	}

	if (requestData.WebhookUrl == "") {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Webhook URL is empty",
			Error:   "no webhook url in request data",
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
			Message: "Failed to store subscription",
			Error:   err.Error(),
		})
	}

	subCtx, subCancelFn := context.WithCancel(svc.Ctx)
	svc.subscriptionsMutex.Lock()
	svc.subCancelFnMap[subscription.Uuid] = subCancelFn
	svc.subscriptionsMutex.Unlock()
	go svc.startSubscription(subCtx, &subscription, nil, svc.handleSubscribedEvent)

	return c.JSON(http.StatusOK, SubscriptionResponse{
		SubscriptionId: subscription.Uuid,
		WebhookUrl:     requestData.WebhookUrl,
	})
}

func (svc *Service) StopSubscriptionHandler(c echo.Context) error {
	uuid := c.Param("id")

	subscription := Subscription{}
	if err := svc.db.First(&subscription, "uuid = ?", uuid).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return c.JSON(http.StatusNotFound, ErrorResponse{
				Message: "Subscription does not exist",
				Error:   err.Error(),
			})
		} else {
			return c.JSON(http.StatusInternalServerError, ErrorResponse{
				Message: "Error occurred while fetching subscription",
				Error:   err.Error(),
			})
		}
	}

	svc.Logger.WithFields(logrus.Fields{
		"subscription_id": subscription.Uuid,
	}).Debug("Stopping subscription")

	err := svc.stopSubscription(&subscription)
	if err != nil {
		svc.Logger.WithFields(logrus.Fields{
			"subscription_id": subscription.Uuid,
		}).Error("Subscription is stopped already")

		return c.JSON(http.StatusAlreadyReported, StopSubscriptionResponse{
			Message: "Subscription is already closed",
			State:   SUBSCRIPTION_ALREADY_CLOSED,
		})
	}

	subscription.Open = false
	svc.db.Save(&subscription)
	// delete svix app

	svc.Logger.WithFields(logrus.Fields{
		"subscription_id": subscription.Uuid,
	}).Info("Stopped subscription")

	return c.JSON(http.StatusOK, StopSubscriptionResponse{
		Message: "Subscription stopped successfully",
		State:   SUBSCRIPTION_CLOSED,
	})
}

func (svc *Service) stopSubscription(subscription *Subscription) error {
	svc.subscriptionsMutex.Lock()
	cancelFn, exists := svc.subCancelFnMap[subscription.Uuid]
	svc.subscriptionsMutex.Unlock()
	if exists {
		cancelFn()
	}
	
	if subscription.RelaySubscription != nil {
		subscription.RelaySubscription.Unsub()
	}

	if (!subscription.Open) {
		return errors.New(SUBSCRIPTION_ALREADY_CLOSED)
	}

	return nil
}

func (svc *Service) startSubscription(ctx context.Context, subscription *Subscription, onReceiveEOS OnReceiveEOSFunc, handleEvent HandleEventFunc) {
	requestEventId := ""
	if subscription.RequestEvent != nil {
		requestEventId = subscription.RequestEvent.NostrId
	}
	svc.Logger.WithFields(logrus.Fields{
		"request_event_id": requestEventId,
		"subscription_id":  subscription.Uuid,
		"relay_url":        subscription.RelayUrl,
	}).Debug("Starting subscription")

	filter := svc.subscriptionToFilter(subscription)

	var relay *nostr.Relay
	var isCustomRelay bool
	var err error

	for {
		// context expiration has no effect on relays
		if relay != nil && relay.Connection != nil && isCustomRelay {
			relay.Close()
		}
		if ctx.Err() != nil {
			svc.Logger.WithError(ctx.Err()).WithFields(logrus.Fields{
				"request_event_id": requestEventId,
				"subscription_id":  subscription.Uuid,
				"relay_url":        subscription.RelayUrl,
			}).Error("Stopping subscription")
			svc.stopSubscription(subscription)
			return
		}
		relay, isCustomRelay, err = svc.getRelayConnection(ctx, subscription.RelayUrl)
		if err != nil {
			continue
		}

		relaySubscription, err := relay.Subscribe(ctx, []nostr.Filter{*filter})
		if err != nil {
			continue
		}

		subscription.RelaySubscription = relaySubscription

		svc.Logger.WithFields(logrus.Fields{
			"request_event_id": requestEventId,
			"subscription_id":  subscription.Uuid,
			"relay_url":        subscription.RelayUrl,
		}).Debug("Started subscription")

		err = svc.processEvents(ctx, subscription, onReceiveEOS, handleEvent)

		if err != nil {
			// TODO: notify user about subscription failure
			svc.Logger.WithError(err).WithFields(logrus.Fields{
				"request_event_id": requestEventId,
				"subscription_id":  subscription.Uuid,
				"relay_url":        subscription.RelayUrl,
			}).Error("Subscription stopped due to relay error, reconnecting...")
			continue
		} else {
			if isCustomRelay {
				relay.Close()
			}
			// stop the subscription if it's an NIP47 request
			if (subscription.RequestEvent != nil) {
				svc.Logger.WithFields(logrus.Fields{
					"request_event_id": requestEventId,
					"subscription_id":  subscription.Uuid,
					"relay_url":        subscription.RelayUrl,
				}).Debug("Stopping subscription")
				svc.stopSubscription(subscription)
			}
			break
		}
	}
}

func (svc *Service) publishRequestEvent(ctx context.Context, subscription *Subscription) {
	walletPubkey, clientPubkey := getPubkeys(subscription)

	relaySubscription := subscription.RelaySubscription
	err := relaySubscription.Relay.Publish(ctx, *subscription.RequestEvent.SignedEvent)
	if err != nil {
		// TODO: notify user about publish failure
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"subscription_id":  subscription.Uuid,
			"request_event_id": subscription.RequestEvent.NostrId,
			"relay_url":        subscription.RelayUrl,
			"wallet_pubkey":    walletPubkey,
			"client_pubkey":    clientPubkey,
		}).Error("Failed to publish to relay")
		subscription.RequestEvent.State = REQUEST_EVENT_PUBLISH_FAILED
		relaySubscription.Unsub()
	} else {
		svc.Logger.WithFields(logrus.Fields{
			"subscription_id":  subscription.Uuid,
			"request_event_id": subscription.RequestEvent.NostrId,
			"relay_url":        subscription.RelayUrl,
			"wallet_pubkey":    walletPubkey,
			"client_pubkey":    clientPubkey,
		}).Debug("Published request event")
		subscription.RequestEvent.State = REQUEST_EVENT_PUBLISH_CONFIRMED
	}
	svc.db.Save(&subscription.RequestEvent)
}

func (svc *Service) handleResponseEvent(event *nostr.Event, subscription *Subscription) {
	walletPubkey, clientPubkey := getPubkeys(subscription)

	svc.Logger.WithFields(logrus.Fields{
		"response_event_id": event.ID,
		"request_event_id":  subscription.RequestEvent.NostrId,
		"wallet_pubkey":     walletPubkey,
		"client_pubkey":     clientPubkey,
		"relay_url":         subscription.RelayUrl,
	}).Debug("Received response event")

	subscription.RequestEvent.ResponseReceivedAt = time.Now()
	svc.db.Save(&subscription.RequestEvent)

	responseEvent := ResponseEvent{
		NostrId:   event.ID,
		Content:   event.Content,
		RepliedAt: event.CreatedAt.Time(),
		RequestId: &subscription.RequestEvent.ID,
	}
	svc.db.Save(&responseEvent)

	subscription.EventChan <- event
}

func (svc *Service) handleSubscribedEvent(event *nostr.Event, subscription *Subscription) {
	svc.Logger.WithFields(logrus.Fields{
		"response_event_id":     event.ID,
		"response_event_kind":   event.Kind,
		"subscription_id":       subscription.Uuid,
		"relay_url":             subscription.RelayUrl,
	}).Info("Received subscribed event")
	responseEvent := ResponseEvent{
		NostrId:        event.ID,
		Content:        event.Content,
		RepliedAt:      event.CreatedAt.Time(),
		SubscriptionId: &subscription.ID,
	}
	svc.db.Save(&responseEvent)
	svc.postEventToWebhook(event, subscription)
}

func (svc *Service) processEvents(ctx context.Context, subscription *Subscription, onReceiveEOS OnReceiveEOSFunc, handleEvent HandleEventFunc) error {
	relaySubscription := subscription.RelaySubscription

	go func(){
		// block till EOS is received for nip 47 handlers
		// only if request event is not yet published
		if (onReceiveEOS != nil && subscription.RequestEvent.State != REQUEST_EVENT_PUBLISH_CONFIRMED) {
			<-relaySubscription.EndOfStoredEvents
			svc.Logger.WithFields(logrus.Fields{
				"subscription_id": subscription.Uuid,
				"relay_url":       subscription.RelayUrl,
			}).Debug("Received EOS")

			onReceiveEOS(ctx, subscription)
		}

		// loop through incoming events
		for event := range relaySubscription.Events {
			go handleEvent(event, subscription)
		}

		svc.Logger.WithFields(logrus.Fields{
			"subscription_id": subscription.Uuid,
			"relay_url":       subscription.RelayUrl,
		}).Debug("Relay subscription events channel ended")
	}()

	select {
	case <-relaySubscription.Relay.Context().Done():
		return relaySubscription.Relay.ConnectionError
	case <-ctx.Done():
		return nil
	case <-relaySubscription.Context.Done():
		return nil
	}
}

func (svc *Service) getRelayConnection(ctx context.Context, customRelayURL string) (*nostr.Relay, bool, error) {
	if customRelayURL != "" && customRelayURL != svc.Cfg.DefaultRelayURL {
		svc.Logger.WithFields(logrus.Fields{
			"custom_relay_url": customRelayURL,
		}).Info("Connecting to custom relay")
		relay, err := svc.relayConnectWithBackoff(ctx, customRelayURL)
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
		relay, err := svc.relayConnectWithBackoff(svc.Ctx, svc.Cfg.DefaultRelayURL)
		if err == nil {
			svc.Relay = relay
		}
		return svc.Relay, false, err
	}
}

func (svc *Service) relayConnectWithBackoff(ctx context.Context, relayURL string) (relay *nostr.Relay, err error) {
	waitToReconnectSeconds := 0
	for {
		select {
		case <-ctx.Done():
			svc.Logger.WithError(err).WithFields(logrus.Fields{
				"relay_url": relayURL,
			}).Errorf("Context canceled, exiting attempt to connect to relay")
			return nil, ctx.Err()
		default:
			time.Sleep(time.Duration(waitToReconnectSeconds) * time.Second)
			relay, err = nostr.RelayConnect(ctx, relayURL)
			if err != nil {
				// TODO: notify user about relay failure
				waitToReconnectSeconds = max(waitToReconnectSeconds, 1)
				waitToReconnectSeconds = min(waitToReconnectSeconds * 2, 900)
				svc.Logger.WithError(err).WithFields(logrus.Fields{
					"relay_url": relayURL,
				}).Errorf("Failed to connect to relay, retrying in %vs...", waitToReconnectSeconds)
				continue
			}
			svc.Logger.WithFields(logrus.Fields{
				"relay_url": relayURL,
			}).Info("Relay connection successful.")
			return relay, nil
		}
	}
}

func (svc *Service) postEventToWebhook(event *nostr.Event, subscription *Subscription) {
	eventData, err := json.Marshal(event)
	requestEventId := ""
	if subscription.RequestEvent != nil {
		requestEventId = subscription.RequestEvent.NostrId
	}

	if err != nil {
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"subscription_id":     subscription.Uuid,
			"request_event_id":    requestEventId,
			"response_event_id":   event.ID,
			"response_event_kind": event.Kind,
			"webhook_url":         subscription.WebhookUrl,
		}).Error("Failed to marshal event for webhook")
		return
	}

	// TODO: add svix functionality
	_, err = http.Post(subscription.WebhookUrl, "application/json", bytes.NewBuffer(eventData))
	if err != nil {
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"subscription_id":     subscription.Uuid,
			"request_event_id":    requestEventId,
			"response_event_id":   event.ID,
			"response_event_kind": event.Kind,
			"webhook_url":         subscription.WebhookUrl,
		}).Error("Failed to post event to webhook")
		return
	}

	svc.Logger.WithFields(logrus.Fields{
		"subscription_id":     subscription.Uuid,
		"request_event_id":    requestEventId,
		"response_event_id":   event.ID,
		"response_event_kind": event.Kind,
		"webhook_url":         subscription.WebhookUrl,
	}).Info("Posted event to webhook")
}

func (svc *Service) subscriptionToFilter(subscription *Subscription) (*nostr.Filter){
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
	return &filter
}

func getPubkeys(subscription *Subscription) (string, string) {
	walletPubkey := ""
	clientPubkey := ""

	if (subscription.RequestEvent != nil) {
		walletPubkey = getWalletPubkey(&subscription.RequestEvent.SignedEvent.Tags)
		clientPubkey = subscription.RequestEvent.SignedEvent.PubKey
	}

	return walletPubkey, clientPubkey
}

func getWalletPubkey(tags *nostr.Tags) string {
	pTag := tags.GetFirst([]string{"p", ""})
	if pTag != nil {
		return pTag.Value()
	}
	return ""
}
