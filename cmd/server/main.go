package main

import (
	"fmt"
	"net/http"

	"http-nostr/internal/nostr"
	"time"

	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
	"github.com/sirupsen/logrus"
	muxtrace "gopkg.in/DataDog/dd-trace-go.v1/contrib/gorilla/mux"
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

	r := muxtrace.NewRouter(muxtrace.WithServiceName("http-nostr"))
	if globalConf.DatadogAgentUrl != "" {
		tracer.Start(tracer.WithAgentAddr(globalConf.DatadogAgentUrl))
		defer tracer.Stop()
	}

	r.HandleFunc("/info", nostr.InfoHandler).Methods(http.MethodPost)
	r.HandleFunc("/nip47", nostr.NIP47Handler).Methods(http.MethodPost)
	// r.Use(loggingMiddleware)

	logrus.Infof("Server starting on port %d", globalConf.Port)
	logrus.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", globalConf.Port), r))
}
