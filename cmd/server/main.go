package main

import (
	"fmt"

	"http-nostr/internal/nostr"
	"time"

	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	ddEcho "gopkg.in/DataDog/dd-trace-go.v1/contrib/labstack/echo.v4"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"

	"github.com/getsentry/sentry-go"
)

func main() {
	logrus.SetFormatter(&logrus.JSONFormatter{})
	// Load env file as env variables
	err := godotenv.Load(".env")
	if err != nil {
		logrus.Errorf("Error loading environment variables: %v", err)
	}
	//load global config
	type config struct {
		SentryDSN       string `envconfig:"SENTRY_DSN"`
		DatadogAgentUrl string `envconfig:"DATADOG_AGENT_URL"`
		DefaultRelayURL string `envconfig:"DEFAULT_RELAY_URL"`
		Port            int    `default:"8080"`
	}
	globalConf := &config{}
	err = envconfig.Process("", globalConf)
	if err != nil {
		logrus.Fatal(err)
	}

	// Setup exception tracking with Sentry if configured
	if globalConf.SentryDSN != "" {
		if err = sentry.Init(sentry.ClientOptions{
			Dsn:          globalConf.SentryDSN,
			IgnoreErrors: []string{"401"},
		}); err != nil {
			logrus.Errorf("sentry init error: %v", err)
		}
		defer sentry.Flush(2 * time.Second)
	}

	e := echo.New()
	if globalConf.DatadogAgentUrl != "" {
		tracer.Start(tracer.WithAgentAddr(globalConf.DatadogAgentUrl))
		defer tracer.Stop()
		e.Use(ddEcho.Middleware(ddEcho.WithServiceName("http-nostr")))
	}

	service, err := nostr.NewNostrService(&nostr.Config{
		DefaultRelayURL: globalConf.DefaultRelayURL,
	})
	if err != nil {
		logrus.Fatalf("Failed to initialize NostrService: %v", err)
	}

	e.GET("/info", service.InfoHandler)
	e.POST("/nip47", service.NIP47Handler)
	// r.Use(loggingMiddleware)

	logrus.Infof("Server starting on port %d", globalConf.Port)
	if err := e.Start(fmt.Sprintf(":%d", globalConf.Port)); err != nil {
		logrus.Fatalf("Server failed to start: %v", err)
	}
}
