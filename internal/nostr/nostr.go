package nostr

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

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

func InfoHandler(w http.ResponseWriter, r *http.Request) {
	req := &InfoRequest{}
	err := json.NewDecoder(r.Body).Decode(req)
	if err != nil {
		handleError(w, err, "error decoding client info request", http.StatusBadRequest)
		return
	}

	logrus.Info("Connecting to the relay...")
	relay, err := nostr.RelayConnect(r.Context(), req.RelayURL)
	if err != nil {
		handleError(w, err, "error connecting to relay", http.StatusBadRequest)
		return
	}

	logrus.Info("Subscribing to info event...")
	filter := nostr.Filter{
		Authors: []string{req.WalletPubkey},
		Kinds:   []int{NIP_47_INFO_EVENT_KIND},
		Limit:   1,
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	sub, err := relay.Subscribe(ctx, []nostr.Filter{filter})
	if err != nil {
		handleError(w, err, "error connecting to relay", http.StatusBadRequest)
		return
	}

	select {
	case <-ctx.Done():
		logrus.Info("Exiting subscription.")
		http.Error(w, "request canceled or timed out", http.StatusRequestTimeout)
		return
	case event := <-sub.Events:
		w.Header().Add("Content-type", "application/json")
		err = json.NewEncoder(w).Encode(event)
		if err != nil {
			handleError(w, err, "error encoding response", http.StatusInternalServerError)
		}
		return
	}
}

func NIP47Handler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	req := &NIP47Request{}
	err := json.NewDecoder(r.Body).Decode(req)
	if err != nil {
		handleError(w, err, "error decoding nip47 request", http.StatusBadRequest)
		return
	}

	relay, err := nostr.RelayConnect(ctx, req.RelayURL)
	if err != nil {
		handleError(w, err, "error connecting to relay", http.StatusBadRequest)
		return
	}

	// Publish the request event
	logrus.Info("Publishing request event...")
	status, err := relay.Publish(ctx, req.SignedEvent)
	if err != nil {
		handleError(w, err, "error publishing request event", http.StatusBadRequest)
		return
	}

	if status == nostr.PublishStatusSucceeded {
		logrus.WithFields(logrus.Fields{
			"status":  status,
			"eventId": req.SignedEvent.ID,
		}).Info("Published request")
	} else if status == nostr.PublishStatusFailed {
		logrus.WithFields(logrus.Fields{
			"status":  status,
			"eventId": req.SignedEvent.ID,
		}).Info("Failed to publish request")
		handleError(w, err, "error publishing request event", http.StatusBadRequest)
		return
	} else {
		logrus.WithFields(logrus.Fields{
			"status":  status,
			"eventId": req.SignedEvent.ID,
		}).Info("Request sent but no response from relay (timeout)")
	}

	// Start subscribing to the event for response
	logrus.Info("Subscribing to events for response...")
	filter := nostr.Filter{
		Authors: []string{req.WalletPubkey},
		Kinds:   []int{NIP_47_RESPONSE_KIND},
		Tags:    nostr.TagMap{"e": []string{req.SignedEvent.ID}},
	}
	sub, err := relay.Subscribe(ctx, []nostr.Filter{filter})
	if err != nil {
		handleError(w, err, "error subscribing to events", http.StatusBadRequest)
		return
	}

	select {
	case <-ctx.Done():
		logrus.Info("Exiting subscription.")
		http.Error(w, "request canceled or timed out", http.StatusRequestTimeout)
		return
	case event := <-sub.Events:
		// TODO: Store the req.SignedEvent.IDs which didn't get
		// a response in a DB and use a global subscription to
		// respond to them
		w.Header().Add("Content-type", "application/json")
		err = json.NewEncoder(w).Encode(event)
		if err != nil {
			handleError(w, err, "error encoding response", http.StatusInternalServerError)
		}
		logrus.Infof("Successfully received event: %s", event.ID)
	}
}
