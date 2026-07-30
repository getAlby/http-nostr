package nostr

import (
	"context"
	"database/sql"
	"http-nostr/migrations"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/getAlby/go-nostr"
	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	sqltrace "github.com/DataDog/dd-trace-go/contrib/database/sql/v2"
	gormtrace "github.com/DataDog/dd-trace-go/contrib/gorm.io/gorm.v1/v2"
	expo "github.com/getAlby/exponent-server-sdk-golang/sdk"
	"github.com/jackc/pgx/v5/stdlib"
)

type Config struct {
	SentryDSN                string   `envconfig:"SENTRY_DSN"`
	DatadogAgentUrl          string   `envconfig:"DATADOG_AGENT_URL"`
	DefaultRelayURLs         []string `envconfig:"DEFAULT_RELAY_URLS" default:"wss://relay.getalby.com,wss://relay2.getalby.com,wss://relay.getalby.com/v1,wss://relay2.getalby.com/v1"`
	MaxRelayConnectionErrors int      `envconfig:"MAX_RELAY_CONNECTION_ERRORS" default:"200"`
	DatabaseUri              string   `envconfig:"DATABASE_URI" default:"http-nostr.db"`
	DatabaseMaxConns         int      `envconfig:"DATABASE_MAX_CONNS" default:"10"`
	DatabaseMaxIdleConns     int      `envconfig:"DATABASE_MAX_IDLE_CONNS" default:"5"`
	DatabaseConnMaxLifetime  int      `envconfig:"DATABASE_CONN_MAX_LIFETIME" default:"1800"` // 30 minutes
	EncryptionKey            string   `envconfig:"ENCRYPTION_KEY"`
	LogLevel                 int      `envconfig:"LOG_LEVEL" default:"4"`
	Port                     int      `envconfig:"PORT" default:"8081"`
}

type Service struct {
	db                 *gorm.DB
	Ctx                context.Context
	Wg                 *sync.WaitGroup
	Relays             map[string]*nostr.Relay
	relayMutex         sync.RWMutex
	relayGroup         singleflight.Group
	Cfg                *Config
	Logger             *logrus.Logger
	subscriptionsMutex sync.Mutex
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
		sqltrace.Register("pgx", &stdlib.Driver{}, sqltrace.WithService("http-nostr"))
		sqlDb, err = sqltrace.Open("pgx", cfg.DatabaseUri)
		if err != nil {
			logger.WithError(err).Error("Failed to open DB")
			return nil, err
		}
		db, err = gormtrace.Open(postgres.New(postgres.Config{Conn: sqlDb}), &gorm.Config{}, gormtrace.WithService("http-nostr"))
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

	client := expo.NewPushClient(&expo.ClientConfig{
		Host:   "https://api.expo.dev",
		APIURL: "/v2",
	})

	var wg sync.WaitGroup
	svc := &Service{
		Cfg:    cfg,
		db:     db,
		Ctx:    ctx,
		Wg:     &wg,
		Logger: logger,
		// TODO: Better to have a garbage collector which removes unused relays periodically
		Relays: make(map[string]*nostr.Relay),
		client: client,
	}

	logger.Info("Connecting to the default relays...")
	for _, url := range cfg.DefaultRelayURLs {
		normalizedRelayURL := nostr.NormalizeURL(url)
		go svc.getRelayConnection(svc.Ctx, normalizedRelayURL)
	}

	logger.Info("Starting all open subscriptions...")
	var openSubscriptions []Subscription
	if err := svc.db.Where("open = ?", true).Find(&openSubscriptions).Error; err != nil {
		logger.WithError(err).Error("Failed to query open subscriptions")
		return nil, err
	}
	svc.subCancelFnMap = make(map[string]context.CancelFunc)
	for _, sub := range openSubscriptions {
		subscription := sub
		if subscription.PushToken != "" {
			svc.restoreSubscription(subscription, PushSubscriptionType)
		} else {
			svc.restoreSubscription(subscription, WebhookSubscriptionType)
		}
	}

	return svc, nil
}
