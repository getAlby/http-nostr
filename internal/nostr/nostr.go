package nostr

import (
	"bytes"
	"context"
	"encoding/json"
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
	RelayURL     string       `json:"relayUrl"`
	WalletPubkey string       `json:"walletPubkey"`
	SignedEvent  *nostr.Event `json:"event"`
	WebhookURL   string       `json:"webhookURL"`
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

	logrus.Info("connecting to the relay...")
	relay, err := nostr.RelayConnect(c.Request().Context(), requestData.RelayURL)
	if err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: fmt.Sprintf("error connecting to relay: %s", err.Error()),
		})
	}

	logrus.Info("subscribing to info event...")
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
		logrus.Info("exiting subscription.")
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

	if (requestData.RelayURL == "" || requestData.WalletPubkey == "") {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "relay url or wallet pubkey is/are empty",
		})
	}

	if requestData.WebhookURL != "" {
		go func() {
			event, _, err := processRequest(context.Background(), &requestData)
			if err != nil {
				logrus.WithError(err).Error("failed to process request for webhook")
				// what to pass to the webhook?
				return
			}
			postEventToWebhook(event, requestData.WebhookURL)
		}()
		return c.JSON(http.StatusOK, "webhook received")
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		event, code, err := processRequest(ctx, &requestData)
		if err != nil {
			return c.JSON(code, ErrorResponse{
					Message: err.Error(),
			})
		}
		return c.JSON(http.StatusOK, event)
	}
}

func processRequest(ctx context.Context, requestData *NIP47Request) (*nostr.Event, int, error) {
	relay, err := nostr.RelayConnect(ctx, requestData.RelayURL)
	if err != nil {
		return &nostr.Event{}, http.StatusBadRequest, fmt.Errorf("error connecting to relay: %w", err)
	}

	logrus.WithFields(logrus.Fields{
		"e": requestData.SignedEvent.ID,
		"author": requestData.WalletPubkey,
	}).Info("subscribing to events for response...")

	filter := nostr.Filter{
		Authors: []string{requestData.WalletPubkey},
		Kinds:   []int{NIP_47_RESPONSE_KIND},
		Tags:    nostr.TagMap{"e": []string{requestData.SignedEvent.ID}},
	}

	sub, err := relay.Subscribe(ctx, []nostr.Filter{filter})
	if err != nil {
		return &nostr.Event{}, http.StatusBadRequest, fmt.Errorf("error subscribing to relay: %w", err)
	}

	status, err := relay.Publish(ctx, *requestData.SignedEvent)
	if err != nil {
		return &nostr.Event{}, http.StatusBadRequest, fmt.Errorf("error publishing request event: %w", err)
	}

	if status == nostr.PublishStatusSucceeded {
		logrus.WithFields(logrus.Fields{
			"status":  status,
			"eventId": requestData.SignedEvent.ID,
		}).Info("published request")
	} else if status == nostr.PublishStatusFailed {
		logrus.WithFields(logrus.Fields{
			"status":  status,
			"eventId": requestData.SignedEvent.ID,
		}).Info("failed to publish request")
		return &nostr.Event{}, http.StatusBadRequest, fmt.Errorf("error publishing request event: %s", err.Error())
	} else {
		logrus.WithFields(logrus.Fields{
			"status":  status,
			"eventId": requestData.SignedEvent.ID,
		}).Info("request sent but no response from relay (timeout)")
	}

	select {
	case <-ctx.Done():
		return &nostr.Event{}, http.StatusRequestTimeout, fmt.Errorf("request canceled or timed out")
	case event := <-sub.Events:
		logrus.Infof("successfully received event: %s", event.ID)
		return event, http.StatusOK, nil
	}
}

func postEventToWebhook(event *nostr.Event, webhookURL string) {
	eventData, err := json.Marshal(event)
	if err != nil {
		logrus.WithError(err).Error("failed to marshal event for webhook")
		return
	}

	_, err = http.Post(webhookURL, "application/json", bytes.NewBuffer(eventData))
	if err != nil {
		logrus.WithError(err).Error("failed to post event to webhook")
	}

	logrus.WithFields(logrus.Fields{
		"eventId": event.ID,
		"eventKind": event.Kind,
	}).Infof("successfully posted event to webhook")
}
