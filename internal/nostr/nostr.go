package nostr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip04"
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

func handleError(w http.ResponseWriter, err error, message string, httpStatusCode int) {
	logrus.WithError(err).Error(message)
	http.Error(w, message, httpStatusCode)
}

func InfoHandler(w http.ResponseWriter, r *http.Request) {
	authHeader := r.Header.Get("Authorization")
	nwcUri := strings.TrimPrefix(authHeader, "Bearer ")

	config, err := parseConfigUri(nwcUri)

	if err != nil {
		handleError(w, err, "error parsing NWC uri", http.StatusBadRequest)
		return
	}

	logrus.Info("Connecting to the relay...")
	relay, err := nostr.RelayConnect(r.Context(), config.RelayURL)
	if err != nil {
		handleError(w, err, "error connecting to relay", http.StatusBadRequest)
		return
	}

	logrus.Info("Subscribing to info event...")
	filter := nostr.Filter{
		Authors: []string{config.WalletPubkey},
		Kinds:   []int{NIP_47_INFO_EVENT_KIND},
		Limit:   1,
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var params map[string]interface{}
	err := json.NewDecoder(r.Body).Decode(&params)

	if err != nil {
		handleError(w, err, "error decoding nostr info request", http.StatusBadRequest)
		return
	}

	authHeader := r.Header.Get("Authorization")
	nwcUri := strings.TrimPrefix(authHeader, "Bearer ")

	config, err := parseConfigUri(nwcUri)

	relay, err := nostr.RelayConnect(ctx, config.RelayURL)
	if err != nil {
		handleError(w, err, "error connecting to relay", http.StatusBadRequest)
		return
	}

	payloadJSON, err := json.Marshal(params)
	if err != nil {
		handleError(w, err, "error marshaling JSON", http.StatusBadRequest)
		return
	}

	ss, err := nip04.ComputeSharedSecret(config.WalletPubkey, config.Secret)
	if err != nil {
		handleError(w, err, "error computing shared secret", http.StatusBadRequest)
		return
	}

	payload, err := nip04.Encrypt(string(payloadJSON), ss)
	if err != nil {
		handleError(w, err, "error encrypting payload", http.StatusBadRequest)
		return
	}

	req := &nostr.Event{
		PubKey:    config.WalletPubkey,
		CreatedAt: nostr.Now(),
		Kind:      NIP_47_REQUEST_KIND,
		Tags:      nostr.Tags{[]string{"p", config.WalletPubkey}},
		Content:   payload,
	}
	err = req.Sign(config.Secret)
	if err != nil {
		handleError(w, err, "error signing event", http.StatusBadRequest)
		return
	}

	// Publish the request event
	logrus.Info("Publishing request event...")

	status, err := relay.Publish(ctx, *req)
	if err != nil {
		handleError(w, err, "error publishing request event", http.StatusBadRequest)
		return
	}

	if status == nostr.PublishStatusSucceeded {
		logrus.WithFields(logrus.Fields{
			"status":  status,
			"eventId": req.ID,
		}).Info("Published reply")
	} else if status == nostr.PublishStatusFailed {
		logrus.WithFields(logrus.Fields{
			"status":  status,
			"eventId": req.ID,
		}).Info("Failed to publish reply")
		handleError(w, err, "error publishing request event", http.StatusBadRequest)
		return
	} else {
		logrus.WithFields(logrus.Fields{
			"status":  status,
			"eventId": req.ID,
		}).Info("Reply sent but no response from relay (timeout)")
	}

	// Start subscribing to the event for response
	logrus.Info("Subscribing to events for response...")
	filter := nostr.Filter{
		Authors: []string{config.WalletPubkey},
		Kinds:   []int{NIP_47_RESPONSE_KIND},
		Tags:    nostr.TagMap{"e": []string{req.ID}},
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
		// TODO: Store the req.IDs which didn't get a response
		// in a DB and use a global subscription to respond
		// to them
		w.Header().Add("Content-type", "application/json")
		event.Content, err = nip04.Decrypt(event.Content, ss)
		if err != nil {
			handleError(w, err, "error decrypting response event content", http.StatusBadRequest)
		}
		err = json.NewEncoder(w).Encode(event)
		if err != nil {
			handleError(w, err, "error encoding response", http.StatusInternalServerError)
		}
		logrus.Info(event.ID)
		logrus.Info("Successful")
	}
}

func parseConfigUri(nwcUri string) (WalletConnectInfo, error) {
	uri, err := url.Parse(nwcUri)
	if err != nil {
		return WalletConnectInfo{}, err
	}

	return WalletConnectInfo{
		RelayURL:     uri.Query().Get("relay"),
		WalletPubkey: uri.Host,
		Secret:       uri.Query().Get("secret"),
	}, err
}
