package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"http-nostr/internal/nostr"

	echologrus "github.com/davrux/echo-logrus/v4"
	"github.com/getsentry/sentry-go"
	sentryecho "github.com/getsentry/sentry-go/echo"
	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	ddEcho "gopkg.in/DataDog/dd-trace-go.v1/contrib/labstack/echo.v4"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"
)


func main() {
	ctx := context.Background()
	svc, err := nostr.NewService(ctx)
	if err != nil {
		logrus.Fatalf("Failed to initialize service: %v", err)
	}

	e := echo.New()
	e.HideBanner = true
	echologrus.Logger = svc.Logger
	e.Use(echologrus.Middleware())

	if svc.Cfg.SentryDSN != "" {
		if err = sentry.Init(sentry.ClientOptions{
			Dsn: svc.Cfg.SentryDSN,
		}); err != nil {
			svc.Logger.WithError(err).Error("Failed to init Sentry")
		}
		defer sentry.Flush(2 * time.Second)
		e.Use(sentryecho.New(sentryecho.Options{}))
	}

	if svc.Cfg.DatadogAgentUrl != "" {
		tracer.Start(tracer.WithService("http-nostr"))
		defer tracer.Stop()
		e.Use(ddEcho.Middleware(ddEcho.WithServiceName("http-nostr")))
	}

	e.POST("/nip47/info", svc.InfoHandler)
	e.POST("/nip47", svc.NIP47Handler)
	e.POST("/nip47/webhook", svc.NIP47WebhookHandler)
	e.POST("/nip47/notifications", svc.NIP47NotificationHandler)
	e.POST("/publish", svc.PublishHandler)
	e.POST("/subscriptions", svc.SubscriptionHandler)
	e.DELETE("/subscriptions/:id", svc.StopSubscriptionHandler)

	//start Echo server
	go func() {
		if err := e.Start(fmt.Sprintf(":%v", svc.Cfg.Port)); err != nil && err != http.ErrServerClosed {
			svc.Logger.Fatalf("Shutting down the server: %v", err)
		}
	}()
	//handle graceful shutdown
	<-svc.Ctx.Done()
	svc.Logger.Info("Shutting down echo server...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e.Shutdown(ctx)
	svc.Logger.Info("Echo server exited")
	svc.Relay.Close()
	svc.Logger.Info("Relay connection closed")
	svc.Logger.Info("Waiting for service to exit...")
	svc.Wg.Wait()
	svc.Logger.Info("Service exited")
}
