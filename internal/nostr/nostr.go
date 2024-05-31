package nostr

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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
	Port                    int    `envconfig:"PORT" default:"8081"`
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
		go svc.startSubscription(svc.Ctx, &subscription, nil, svc.persistSubscribedEvent)
	}

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
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "Error connecting to relay",
			Error:   err.Error(),
		})
	}
	if isCustomRelay {
		defer relay.Close()
	}

	svc.Logger.WithFields(logrus.Fields{
		"relayUrl":     requestData.RelayUrl,
		"walletPubkey": requestData.WalletPubkey,
	}).Info("Subscribing to info event")

	filter := nostr.Filter{
		Authors: []string{requestData.WalletPubkey},
		Kinds:   []int{NIP_47_INFO_EVENT_KIND},
		Limit:   1,
	}
	sub, err := relay.Subscribe(ctx, []nostr.Filter{filter})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "Error subscribing to relay",
			Error:   err.Error(),
		})
	}

	select {
	case <-ctx.Done():
		svc.Logger.WithFields(logrus.Fields{
			"relayUrl":     requestData.RelayUrl,
			"walletPubkey": requestData.WalletPubkey,
		}).Info("Exiting info subscription without receiving")
		return c.JSON(http.StatusRequestTimeout, ErrorResponse{
			Message: "Request canceled or timed out",
			Error:   ctx.Err().Error(),
		})
	case event := <-sub.Events:
		svc.Logger.WithFields(logrus.Fields{
			"relayUrl":     requestData.RelayUrl,
			"walletPubkey": requestData.WalletPubkey,
			"eventId":      event.ID,
		}).Info("Received info event")
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
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "Error connecting to relay",
			Error:   err.Error(),
		})
	}

	if isCustomRelay {
		defer relay.Close()
	}

	svc.Logger.WithFields(logrus.Fields{
		"eventId":   requestData.SignedEvent.ID,
		"relayUrl":  requestData.RelayUrl,
	}).Info("Publishing event")

	err = relay.Publish(ctx, *requestData.SignedEvent)
	if err != nil {
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"eventId":   requestData.SignedEvent.ID,
			"relayUrl":  requestData.RelayUrl,
		}).Error("Failed to publish event")
	
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "Error publishing the event",
			Error:   err.Error(),
		})
	}

	svc.Logger.WithFields(logrus.Fields{
		"eventId":   requestData.SignedEvent.ID,
		"relayUrl":  requestData.RelayUrl,
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
		"requestEventId": requestData.SignedEvent.ID,
		"walletPubkey":   requestData.WalletPubkey,
		"relayUrl":       requestData.RelayUrl,
	}).Info("Processing request event")

	requestEvent := RequestEvent{}
	findRequestResult := svc.db.Where("nostr_id = ?", requestData.SignedEvent.ID).Find(&requestEvent)

	if findRequestResult.RowsAffected != 0 {
		responseEvent := ResponseEvent{}
		findResponseResult := svc.db.Where("request_id = ?", requestEvent.ID).Find(&responseEvent)

		if findResponseResult.RowsAffected != 0 {
			return c.JSON(http.StatusBadRequest, NIP47Response{
				Event: &nostr.Event{
					ID:        responseEvent.NostrId,
					CreatedAt: nostr.Timestamp(responseEvent.RepliedAt.Unix()),
					Content:   responseEvent.Content,
				},
				State: EVENT_ALREADY_PUBLISHED,
			})
		}
	} else {
		requestEvent = RequestEvent{
			NostrId: requestData.SignedEvent.ID,
			Content: requestData.SignedEvent.Content,
		}

		if err := svc.db.Create(&requestEvent).Error; err != nil {
			return c.JSON(http.StatusInternalServerError, ErrorResponse{
				Message: "Failed to store request event",
				Error:   err.Error(),
			})
		}
	}

	subscription := Subscription{
		RelayUrl:       requestData.RelayUrl,
		Open:           true,
		Authors:        &[]string{requestData.WalletPubkey},
		Kinds:          &[]int{NIP_47_RESPONSE_KIND},
		Tags:           &nostr.TagMap{"e": []string{requestData.SignedEvent.ID}},
		Since:          time.Now(),
		Limit:          1,
		RequestEvent:   requestData.SignedEvent,
		RequestEventDB: requestEvent,
		EventChan:      make(chan *nostr.Event, 1),
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), 90*time.Second)
	defer cancel()
	go svc.startSubscription(ctx, &subscription, svc.publishEvent, svc.persistResponseEvent)

	select {
	case <-ctx.Done():
		svc.Logger.WithFields(logrus.Fields{
			"requestEventId": requestData.SignedEvent.ID,
			"walletPubkey":   requestData.WalletPubkey,
			"relayUrl":       requestData.RelayUrl,
		}).Info("Stopped subscription without receiving event")
		return c.JSON(http.StatusRequestTimeout, ErrorResponse{
			Message: "Request canceled or timed out",
			Error:   ctx.Err().Error(),
		})
	case event := <-subscription.EventChan:
		svc.Logger.WithFields(logrus.Fields{
			"requestEventId":  requestData.SignedEvent.ID,
			"responseEventId": event.ID,
			"walletPubkey":    requestData.WalletPubkey,
			"relayUrl":        requestData.RelayUrl,
		}).Info("Received response event")
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
		"requestEventId": requestData.SignedEvent.ID,
		"walletPubkey":   requestData.WalletPubkey,
		"relayUrl":       requestData.RelayUrl,
		"webhookUrl":     requestData.WebhookUrl,
	}).Info("Processing request event")

	requestEvent := RequestEvent{}
	findRequestResult := svc.db.Where("nostr_id = ?", requestData.SignedEvent.ID).Find(&requestEvent)

	if findRequestResult.RowsAffected != 0 {
		responseEvent := ResponseEvent{}
		findResponseResult := svc.db.Where("request_id = ?", requestEvent.ID).Find(&responseEvent)

		if findResponseResult.RowsAffected != 0 {
			return c.JSON(http.StatusBadRequest, NIP47Response{
				Event: &nostr.Event{
					ID:        responseEvent.NostrId,
					CreatedAt: nostr.Timestamp(responseEvent.RepliedAt.Unix()),
					Content:   responseEvent.Content,
				},
				State: EVENT_ALREADY_PUBLISHED,
			})
		}
	} else {
		requestEvent = RequestEvent{
			NostrId: requestData.SignedEvent.ID,
			Content: requestData.SignedEvent.Content,
		}

		if err := svc.db.Create(&requestEvent).Error; err != nil {
			return c.JSON(http.StatusInternalServerError, ErrorResponse{
				Message: "Failed to store request event",
				Error:   err.Error(),
			})
		}
	}

	subscription := Subscription{
		RelayUrl:       requestData.RelayUrl,
		WebhookUrl:     requestData.WebhookUrl,
		Open:           true,
		Authors:        &[]string{requestData.WalletPubkey},
		Kinds:          &[]int{NIP_47_RESPONSE_KIND},
		Tags:           &nostr.TagMap{"e": []string{requestData.SignedEvent.ID}},
		Since:          time.Now(),
		Limit:          1,
		RequestEvent:   requestData.SignedEvent,
		RequestEventDB: requestEvent,
		EventChan:      make(chan *nostr.Event, 1),
	}

	ctx, cancel := context.WithTimeout(svc.Ctx, 90*time.Second)
	defer cancel()

	go svc.startSubscription(ctx, &subscription, svc.publishEvent, svc.persistResponseEvent)
	return c.JSON(http.StatusOK, NIP47Response{
		State: WEBHOOK_RECEIVED,
	})
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

	subscription := Subscription{
		RelayUrl:   requestData.RelayUrl,
		WebhookUrl: requestData.WebhookUrl,
		Open:       true,
		Authors:    &[]string{requestData.WalletPubkey},
		Kinds:      &[]int{NIP_47_NOTIFICATION_KIND},
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

	go svc.startSubscription(svc.Ctx, &subscription, nil, svc.persistSubscribedEvent)

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

	go svc.startSubscription(svc.Ctx, &subscription, nil, svc.persistSubscribedEvent)

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
		"subscriptionId": subscription.ID,
	}).Info("Stopping subscription")

	svc.subscriptionsMutex.Lock()
	sub, exists := svc.subscriptions[subscription.ID]
	if exists {
		sub.Unsub()
		delete(svc.subscriptions, subscription.ID)
	}
	svc.subscriptionsMutex.Unlock()

	if (!exists && !subscription.Open) {
		svc.Logger.WithFields(logrus.Fields{
			"subscriptionId": subscription.ID,
		}).Info("Subscription is stopped already")

		return c.JSON(http.StatusAlreadyReported, StopSubscriptionResponse{
			Message: "Subscription is already closed",
			State:   SUBSCRIPTION_ALREADY_CLOSED,
		})
	}

	subscription.Open = false
	svc.db.Save(&subscription)
	// delete svix app

	svc.Logger.WithFields(logrus.Fields{
		"subscriptionId": subscription.ID,
	}).Info("Stopped subscription")

	return c.JSON(http.StatusOK, StopSubscriptionResponse{
		Message: "Subscription stopped successfully",
		State:   SUBSCRIPTION_CLOSED,
	})
}

func (svc *Service) startSubscription(ctx context.Context, subscription *Subscription, onReceiveEOS func(ctx context.Context, subscription *Subscription, sub *nostr.Subscription), persistEvent func(event *nostr.Event, subscription *Subscription, sub *nostr.Subscription)) {
	svc.Logger.WithFields(logrus.Fields{
		"subscriptionId": subscription.ID,
	}).Info("Starting subscription")

	filter := svc.subscriptionToFilter(subscription)

	var relay *nostr.Relay
	var isCustomRelay bool
	var err error

	for {
		// close relays with connection errors before connecting again
		// because context expiration has no effect on relays
		// TODO: Call relay.Connect on already initialized relays
		if relay != nil && isCustomRelay {
			relay.Close()
		}
		relay, isCustomRelay, err = svc.getRelayConnection(ctx, subscription.RelayUrl)
		if err != nil {
			// TODO: notify user about relay failure
			svc.Logger.WithError(err).WithFields(logrus.Fields{
				"subscriptionId": subscription.ID,
				"relayUrl":       subscription.RelayUrl,
			}).Error("Failed get relay connection, retrying in 5s...")
			time.Sleep(5 * time.Second) // sleep for 5 seconds
			continue
		}

		sub, err := relay.Subscribe(ctx, []nostr.Filter{*filter})
		if err != nil {
			// TODO: notify user about subscription failure
			svc.Logger.WithError(err).WithFields(logrus.Fields{
				"subscriptionId": subscription.ID,
				"relayUrl":       subscription.RelayUrl,
			}).Error("Failed to subscribe to relay, retrying in 5s...")
			time.Sleep(5 * time.Second) // sleep for 5 seconds
			continue
		}

		svc.subscriptionsMutex.Lock()
		svc.subscriptions[subscription.ID] = sub
		svc.subscriptionsMutex.Unlock()

		svc.Logger.WithFields(logrus.Fields{
			"subscriptionId": subscription.ID,
		}).Info("Started subscription")

		err = svc.processEvents(ctx, subscription, sub, onReceiveEOS, persistEvent)

		if err != nil {
			// TODO: notify user about subscription failure
			svc.Logger.WithError(err).WithFields(logrus.Fields{
				"subscriptionId": subscription.ID,
				"relayUrl":       subscription.RelayUrl,
			}).Error("Subscription stopped due to relay error, reconnecting in 5s...")
			time.Sleep(5 * time.Second) // sleep for 5 seconds
			continue
		} else {
			if isCustomRelay {
				relay.Close()
			}
			if (subscription.RequestEvent != nil && subscription.RequestEventDB.State == "") {
				subscription.RequestEventDB.State = REQUEST_EVENT_PUBLISH_FAILED
				svc.db.Save(&subscription.RequestEventDB)
			}
			svc.Logger.WithFields(logrus.Fields{
				"subscriptionId": subscription.ID,
				"relayUrl":       subscription.RelayUrl,
			}).Info("Stopping subscription")
			break
		}
	}
}

func (svc *Service) publishEvent(ctx context.Context, subscription *Subscription, sub *nostr.Subscription) {
	err := sub.Relay.Publish(ctx, *subscription.RequestEvent)
	if err != nil {
		// TODO: notify user about publish failure
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"subscriptionId": subscription.ID,
			"relayUrl":       subscription.RelayUrl,
		}).Error("Failed to publish to relay")
		sub.Unsub()
	} else {
		svc.Logger.WithFields(logrus.Fields{
			"status":  REQUEST_EVENT_PUBLISH_CONFIRMED,
			"eventId": subscription.RequestEvent.ID,
		}).Info("Published request event successfully")
		subscription.RequestEventDB.State = REQUEST_EVENT_PUBLISH_CONFIRMED
		svc.db.Save(&subscription.RequestEventDB)
	}
}

func (svc *Service) persistResponseEvent(event *nostr.Event, subscription *Subscription, sub *nostr.Subscription) {
	svc.Logger.WithFields(logrus.Fields{
		"eventId":        event.ID,
		"eventKind":      event.Kind,
		"requestEventId": subscription.RequestEvent.ID,
	}).Info("Received response event")
	responseEvent := ResponseEvent{
		NostrId:   event.ID,
		Content:   event.Content,
		RepliedAt: event.CreatedAt.Time(),
		RequestId: &subscription.RequestEventDB.ID,
	}
	svc.db.Save(&responseEvent)
	if subscription.WebhookUrl != "" {
		svc.postEventToWebhook(event, subscription.WebhookUrl)
	} else {
		subscription.EventChan <- event
	}
	sub.Unsub()
}

func (svc *Service) persistSubscribedEvent(event *nostr.Event, subscription *Subscription, sub *nostr.Subscription) {
	svc.Logger.WithFields(logrus.Fields{
		"eventId":        event.ID,
		"eventKind":      event.Kind,
		"subscriptionId": subscription.ID,
	}).Info("Received event")
	responseEvent := ResponseEvent{
		NostrId:        event.ID,
		Content:        event.Content,
		RepliedAt:      event.CreatedAt.Time(),
		SubscriptionId: &subscription.ID,
	}
	svc.db.Save(&responseEvent)
	svc.postEventToWebhook(event, subscription.WebhookUrl)
}

func (svc *Service) processEvents(ctx context.Context, subscription *Subscription, sub *nostr.Subscription, onReceiveEOS func(ctx context.Context, subscription *Subscription, sub *nostr.Subscription), persistEvent func(event *nostr.Event, subscription *Subscription, sub *nostr.Subscription)) error {
	receivedEOS := false
	// Do not process historic events
	go func() {
		<-sub.EndOfStoredEvents
		svc.Logger.WithFields(logrus.Fields{
			"subscriptionId": subscription.ID,
		}).Info("Received EOS")
		receivedEOS = true

		if (onReceiveEOS != nil) {
			onReceiveEOS(ctx, subscription, sub)
		}
	}()

	go func(){
		for event := range sub.Events {
			if receivedEOS {
				persistEvent(event, subscription, sub)
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
	svc.Logger.Info("Fetching default relay")
	// use mutex otherwise the svc.Relay will be reconnected more than once
	svc.relayMutex.Lock()
	defer svc.relayMutex.Unlock()
	// check if the default relay is active, else reconnect and return the relay
	if svc.Relay.IsConnected() {
		return svc.Relay, false, nil
	} else {
		svc.Logger.Info("Lost connection to default relay, reconnecting...")
		relay, err := nostr.RelayConnect(svc.Ctx, svc.Cfg.DefaultRelayURL)
		if err == nil {
			svc.Relay = relay
		}
		return svc.Relay, false, err
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
