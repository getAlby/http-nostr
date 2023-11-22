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

func InfoHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	req := &NIP47HTTPInfo{}
	err := json.NewDecoder(r.Body).Decode(req)
	if err != nil {
		logrus.Errorf("Error parsing NWC uri %s", err.Error())
		w.WriteHeader(http.StatusBadRequest)
		_, err = w.Write([]byte("Could not parse NWC uri"))
		if err != nil {
			logrus.Error(err)
		}
		cancel()
		return
	}

	config, err := parseConfigUri(req.NwcUri)
	if err != nil {
		logrus.Errorf("Error parsing NWC uri %s", err.Error())
		w.WriteHeader(http.StatusBadRequest)
		_, err = w.Write([]byte("Could not parse NWC uri"))
		if err != nil {
			logrus.Error(err)
		}
		cancel()
		return
	}

	logrus.Info("connecting to the relay...")
	relay, err := nostr.RelayConnect(ctx, config.RelayURL)
	if err != nil {
		logrus.Errorf("Error connecting to relay %s", err.Error())
		w.WriteHeader(http.StatusBadRequest)
		_, err = w.Write([]byte("Could not connect to the relay"))
		if err != nil {
			logrus.Error(err)
		}
		cancel()
		return
	}

	logrus.Info("Subscribing to events...")
	filter := nostr.Filter{
		Authors: []string{config.WalletPubkey},
		Kinds:   []int{NIP_47_INFO_EVENT_KIND},
		Limit:   1,
	}
	sub, err := relay.Subscribe(ctx, []nostr.Filter{filter})
	if err != nil {
		logrus.Errorf("Error subscribing to events %s", err.Error())
		w.WriteHeader(http.StatusBadRequest)
		_, err = w.Write([]byte("Could not subscribe to event"))
		if err != nil {
			logrus.Error(err)
		}
		cancel()
		return
	}

	evs := make([]nostr.Event, 0)

	go func() {
		<-sub.EndOfStoredEvents
		cancel()
	}()

	for ev := range sub.Events {
		evs = append(evs, *ev)
	}

	if len(evs) == 0 {
		logrus.Errorf("Didn't find any info event")
		w.WriteHeader(http.StatusNotFound)
		_, err = w.Write([]byte("Could not find info event with the specified wallet pubkey"))
		if err != nil {
			logrus.Error(err)
		}
		return
	}

	w.Header().Add("Content-type", "application/json")
	err = json.NewEncoder(w).Encode(evs[0])
	if err != nil {
		logrus.Error(err)
	}
	return
}

func NIP47Handler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	req := &NIP47HTTPReq{}
	err := json.NewDecoder(r.Body).Decode(req)

	if err != nil {
		logrus.Errorf("Error decoding nostr info request %s", err.Error())
		w.WriteHeader(http.StatusBadRequest)
		_, err = w.Write([]byte("Could not parse nostr info request"))
		if err != nil {
			logrus.Error(err)
		}
		cancel()
		return
	}

	// logrus.Info(req.Params["invoice"])

	config, err := parseConfigUri(req.NwcUri)

	relay, err := nostr.RelayConnect(ctx, config.RelayURL)
	if err != nil {
		panic(err)
	}

	payloadJSON, err := json.Marshal(map[string]interface{}{
		"method": req.Method,
		"params": req.Params,
	})
	if err != nil {
		logrus.Errorf("Error marshaling JSON %s", err)
		cancel()
		return
	}

	ss, err := nip04.ComputeSharedSecret(config.WalletPubkey, config.Secret)

	payload, err := nip04.Encrypt(string(payloadJSON), ss)

	resp := &nostr.Event{
		PubKey:    config.WalletPubkey,
		CreatedAt: nostr.Now(),
		Kind:      NIP_47_REQUEST_KIND,
		Tags:      nostr.Tags{[]string{"p", config.WalletPubkey}},
		Content:   payload,
	}
	err = resp.Sign(config.Secret)

	// Publish the request event
	logrus.Info("Publishing request event...")
	err = publisher(ctx, relay, resp)

	// Start subscribing to the event for response
	logrus.Info("Subscribing to events for response...")
	filter := nostr.Filter{
		Authors: []string{config.WalletPubkey},
		Kinds:   []int{NIP_47_RESPONSE_KIND},
		Tags:    nostr.TagMap{"e": []string{resp.ID}},
	}
	sub, err := relay.Subscribe(ctx, []nostr.Filter{filter})
	if err != nil {
		logrus.Errorf("Error subscribing to events %s", err.Error())
		w.WriteHeader(http.StatusBadRequest)
		_, err = w.Write([]byte("Could not subscribe to event"))
		if err != nil {
			logrus.Error(err)
		}
		cancel()
		return
	}

	for {
		select {
		case <-ctx.Done():
			logrus.Info("Exiting subscription.")
			cancel()
			return
		case event := <-sub.Events:
			go func() {
				w.Header().Add("Content-type", "application/json")
				err = json.NewEncoder(w).Encode(event)
				if err != nil {
					logrus.Error(err)
				}
				logrus.Info(event.ID)
				logrus.Info("Success")
				cancel()
			}()
		}
	}
}

func parseConfigUri(nwcUri string) (WalletConnectConfig, error) {
	// Replace "nostrwalletconnect://" or "nostr+walletconnect://" with "http://"
	nwcUri = strings.Replace(nwcUri, "nostrwalletconnect://", "http://", 1)
	nwcUri = strings.Replace(nwcUri, "nostr+walletconnect://", "http://", 1)

	uri, err := url.Parse(nwcUri)

	return WalletConnectConfig{
		RelayURL:     uri.Query().Get("relay"),
		WalletPubkey: uri.Host,
		Secret:       uri.Query().Get("secret"),
	}, err
}

func publisher(ctx context.Context, relay *nostr.Relay, resp *nostr.Event) error {
	status, err := relay.Publish(ctx, *resp)

	if err != nil {
		return err
	}

	if status == nostr.PublishStatusSucceeded {
		logrus.WithFields(logrus.Fields{
			"status":       status,
			"eventId":      resp.ID,
		}).Info("Published reply")
	} else if status == nostr.PublishStatusFailed {
		logrus.WithFields(logrus.Fields{
			"status":       status,
			"eventId":      resp.ID,
		}).Info("Failed to publish reply")
	} else {
		logrus.WithFields(logrus.Fields{
			"status":       status,
			"eventId":      resp.ID,
		}).Info("Reply sent but no response from relay (timeout)")
	}

	return nil
}

func createFilters(pubkey string, kind int) nostr.Filters {
	filter := nostr.Filter{
		Tags:  nostr.TagMap{"p": []string{pubkey}},
		Kinds: []int{NIP_47_REQUEST_KIND},
	}
	return []nostr.Filter{filter}
}
