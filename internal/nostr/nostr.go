package nostr

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/nbd-wtf/go-nostr"
	"github.com/sirupsen/logrus"
)

const (
	NIP_47_INFO_EVENT_KIND = 13194
	NIP_47_REQUEST_KIND    = 23194
	NIP_47_RESPONSE_KIND   = 23195
)

type WalletConnectInfo struct {
	RelayURL     string
	WalletPubkey string
	Secret       string
}

type ErrorResponse struct {
	Message string `json:"message"`
}

type InfoRequest struct {
	RelayURL     string `json:"relayUrl"`
	WalletPubkey string `json:"walletPubkey"`
}

type NIP47Request struct {
	RelayURL     string      `json:"relayUrl"`
	WalletPubkey string      `json:"walletPubkey"`
	SignedEvent  nostr.Event `json:"event"`
}

func handleError(w http.ResponseWriter, err error, message string, httpStatusCode int) {
	logrus.WithError(err).Error(message)
	http.Error(w, message, httpStatusCode)
}

func InfoHandler(c echo.Context) error {
	var requestData InfoRequest
	if err := c.Bind(&requestData); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: fmt.Sprintf("error decoding info request: %s", err.Error()),
		})
	}

	logrus.Info("Connecting to the relay...")
	relay, err := nostr.RelayConnect(c.Request().Context(), requestData.RelayURL)
	if err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: fmt.Sprintf("error connecting to relay: %s", err.Error()),
		})
	}

	logrus.Info("Subscribing to info event...")
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
		logrus.Info("Exiting subscription.")
		return c.JSON(http.StatusRequestTimeout, ErrorResponse{
			Message: "request canceled or timed out",
		})
	case event := <-sub.Events:
		return c.JSON(http.StatusOK, event)
	}
}

func NIP47Handler(c echo.Context) error {
	var requestData NIP47Request
	if err := c.Bind(&requestData); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: fmt.Sprintf("error decoding nip47 request: %s", err.Error()),
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	relay, err := nostr.RelayConnect(ctx, requestData.RelayURL)
	if err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: fmt.Sprintf("error connecting to relay: %s", err.Error()),
		})
	}

	// Start subscribing to the event for response
	logrus.WithFields(logrus.Fields{"e": requestData.SignedEvent.ID, "author": requestData.WalletPubkey}).Info("Subscribing to events for response...")
	filter := nostr.Filter{
		Authors: []string{requestData.WalletPubkey},
		Kinds:   []int{NIP_47_RESPONSE_KIND},
		Tags:    nostr.TagMap{"e": []string{requestData.SignedEvent.ID}},
	}
	sub, err := relay.Subscribe(ctx, []nostr.Filter{filter})
	if err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: fmt.Sprintf("error subscribing to relay: %s", err.Error()),
		})
	}

	// Publish the request event
	logrus.Info("Publishing request event...")
	status, err := relay.Publish(ctx, requestData.SignedEvent)
	if err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: fmt.Sprintf("error publishing request event: %s", err.Error()),
		})
	}

	if status == nostr.PublishStatusSucceeded {
		logrus.WithFields(logrus.Fields{
			"status":  status,
			"eventId": requestData.SignedEvent.ID,
		}).Info("Published request")
	} else if status == nostr.PublishStatusFailed {
		logrus.WithFields(logrus.Fields{
			"status":  status,
			"eventId": requestData.SignedEvent.ID,
		}).Info("Failed to publish request")
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: fmt.Sprintf("error publishing request event: %s", err.Error()),
		})
	} else {
		logrus.WithFields(logrus.Fields{
			"status":  status,
			"eventId": requestData.SignedEvent.ID,
		}).Info("Request sent but no response from relay (timeout)")
	}

	select {
	case <-ctx.Done():
		logrus.Info("Exiting subscription.")
		return c.JSON(http.StatusRequestTimeout, ErrorResponse{
			Message: "request canceled or timed out",
		})
	case event := <-sub.Events:
		// TODO: Store the req.SignedEvent.IDs which didn't get
		// a response in a DB and use a global subscription to
		// respond to them
		logrus.Infof("Successfully received event: %s", event.ID)
		return c.JSON(http.StatusOK, event)
	}
}
