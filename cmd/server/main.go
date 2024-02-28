package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"http-nostr/internal/nostr"

	echologrus "github.com/davrux/echo-logrus/v4"
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

	echologrus.Logger = svc.Logger
	e := echo.New()
	if svc.Cfg.DatadogAgentUrl != "" {
		tracer.Start(tracer.WithAgentAddr(svc.Cfg.DatadogAgentUrl))
		defer tracer.Stop()
		e.Use(ddEcho.Middleware(ddEcho.WithServiceName("http-nostr")))
	}

	e.POST("/nip47/info", svc.InfoHandler)
	e.POST("/nip47", svc.NIP47Handler)
	e.POST("/subscribe", svc.SubscriptionHandler)
	e.DELETE("/subscribe/:id", svc.StopSubscriptionHandler)
	e.Use(echologrus.Middleware())

	//start Echo server
	go func() {
		if err := e.Start(fmt.Sprintf(":%v", svc.Cfg.Port)); err != nil && err != http.ErrServerClosed {
			svc.Logger.Fatalf("Shutting down the server: %v", err)
		}
	}()
	//handle graceful shutdown
	<-svc.Ctx.Done()
	svc.Logger.Infof("Shutting down echo server...")
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
