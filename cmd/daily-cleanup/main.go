package main

import (
	"database/sql"
	"http-nostr/internal/nostr"
	"os"
	"time"

	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
	"github.com/sirupsen/logrus"
	logger "github.com/sirupsen/logrus"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/jackc/pgx/v5/stdlib"
	sqltrace "gopkg.in/DataDog/dd-trace-go.v1/contrib/database/sql"
	gormtrace "gopkg.in/DataDog/dd-trace-go.v1/contrib/gorm.io/gorm.v1"
)

func main() {
	godotenv.Load(".env")

	cfg := &nostr.Config{}
	err := envconfig.Process("", cfg)
	if err != nil {
		logger.Fatalf("Error loading environment variables: %v", err)
	}

	logger.SetFormatter(&logrus.JSONFormatter{})
	logger.SetOutput(os.Stdout)
	logger.SetLevel(logrus.Level(cfg.LogLevel))

	db, err := initStorage(cfg)
	if err != nil {
		logger.Fatalf("Could not initialize storage: %v", err)
	}

	queries := []string{
		// delete closed subscriptions older than 2 weeks
		// note: this will cascade delete the corresponding response_events
		`DELETE FROM subscriptions
			 WHERE open = FALSE
				 AND created_at < NOW() - INTERVAL '2 weeks';`,

		// delete request_events older than 2 weeks
		// note: this will cascade delete the corresponding response_events
		`DELETE FROM request_events
			 WHERE created_at < NOW() - INTERVAL '2 weeks';`,

		// delete response_events older than 2 weeks
		// note: this deletes response_events from active subscriptions
		`DELETE FROM response_events
			 WHERE created_at < NOW() - INTERVAL '2 weeks';`,
	}

	err = db.Transaction(func(tx *gorm.DB) error {
		for _, q := range queries {
			if err := tx.Exec(q).Error; err != nil {
				logger.WithError(err).Errorf("Query failed: %s", q)
				return err
			}
		}
		return nil
	})
	if err != nil {
		logger.Fatalf("Failed to run cron job: %v", err)
	}
}

func initStorage(cfg *nostr.Config) (*gorm.DB, error) {
	var db *gorm.DB
	var sqlDb *sql.DB
	var err error
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

	return db, nil
}
