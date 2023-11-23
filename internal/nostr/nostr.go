package nostr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip04"
	"github.com/sirupsen/logrus"
)

func InfoHandler(w http.ResponseWriter, r *http.Request) {
	//typically, you would use r.Context() for all contexts inside the
	//handling of the request, instead of creating a new context
	//I don't think it's necessary to explicitly set a timeout
	//but if you need to, you can use the context from the request as a parent context
	//so this line can be deleted
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	//some documentation about this header requirement in the readme would be helpful
	authHeader := r.Header.Get("Authorization")
	nwcUri := strings.TrimPrefix(authHeader, "Bearer ")

	config, err := parseConfigUri(nwcUri)

	//this error handling code can be extracted into a function
	//actually, the entire logic should return a struct an an error
	//and the error response handling / struct serialization should be done in a function one level above
	if err != nil {
		//error messages typically start with lowercase
		//I also typically use logrus.WithError().Error()
		//instead of errorf
		logrus.Errorf("Error parsing NWC uri %s", err.Error())
		w.WriteHeader(http.StatusBadRequest)
		_, err = w.Write([]byte("Could not parse NWC uri"))
		//this is one of the few cases where it's not really useful to check the error
		//so this can be removed
		if err != nil {
			logrus.Error(err)
		}
		//typically you would do a defer cancel() as soon as you create the cancel func
		//but you don't even need to create it like I said
		cancel()
		return
	}

	logrus.Info("connecting to the relay...")
	relay, err := nostr.RelayConnect(ctx, config.RelayURL)
	//duplicate error handling code
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
	//here it might make sense to add a timeout to the context
	sub, err := relay.Subscribe(ctx, []nostr.Filter{filter})
	//duplicate error handling code
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

	//I would replace this entire block by a simple
	//event <- sub.events
	//close the subscription
	//and return the event
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

	//would do this on one line
	//as a matter of fact, why not put the method in the body?
	//so it's just the same payload as in nip 47?
	vars := mux.Vars(r)
	method := vars["method"]
	logrus.Info(method)
	if method == "" {
		logrus.Errorf("No method passed")
		w.WriteHeader(http.StatusBadRequest)
		_, err := w.Write([]byte("No method passed"))
		if err != nil {
			logrus.Error(err)
		}
		cancel()
		return
	}

	var params map[string]interface{}
	err := json.NewDecoder(r.Body).Decode(&params)
	logrus.Info(params)

	if err != nil {
		logrus.Errorf("Error decoding nostr info request %s", err.Error())
		w.WriteHeader(http.StatusBadRequest)
		_, err = w.Write([]byte("Could not parse nip47 request"))
		if err != nil {
			logrus.Error(err)
		}
		cancel()
		return
	}

	authHeader := r.Header.Get("Authorization")
	nwcUri := strings.TrimPrefix(authHeader, "Bearer ")

	config, err := parseConfigUri(nwcUri)

	relay, err := nostr.RelayConnect(ctx, config.RelayURL)
	if err != nil {
		//DON'T PANIC
		//see the Go Proverbs: https://go-proverbs.github.io/
		//(last one)
		panic(err)
	}

	//first we decode, now we encode again
	//if the method would be in the body, the bytes could just be
	//encrypted and passed along
	payloadJSON, err := json.Marshal(map[string]interface{}{
		"method": method,
		"params": params,
	})
	if err != nil {
		logrus.Errorf("Error marshaling JSON %s", err)
		cancel()
		return
	}

	ss, err := nip04.ComputeSharedSecret(config.WalletPubkey, config.Secret)
	//no error handling

	payload, err := nip04.Encrypt(string(payloadJSON), ss)
	//no error handling

	//this is not a response, so it should be called req, not resp
	resp := &nostr.Event{
		PubKey:    config.WalletPubkey,
		CreatedAt: nostr.Now(),
		Kind:      NIP_47_REQUEST_KIND,
		Tags:      nostr.Tags{[]string{"p", config.WalletPubkey}},
		Content:   payload,
	}
	err = resp.Sign(config.Secret)
	//no error handling

	// Publish the request event
	logrus.Info("Publishing request event...")
	err = publisher(ctx, relay, resp)
	//no error handling

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

	//I would replace this entire block by a simple
	//event <- sub.events
	//decrypt the event
	//close the subscription
	//and return the event
	for {
		select {
		case <-ctx.Done():
			logrus.Info("Exiting subscription.")
			cancel()
			return
		case event := <-sub.Events:
			go func() {
				w.Header().Add("Content-type", "application/json")
				event.Content, _ = nip04.Decrypt(event.Content, ss)
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
	//this is not needed, these lines can be deleted
	nwcUri = strings.Replace(nwcUri, "nostrwalletconnect://", "http://", 1)
	nwcUri = strings.Replace(nwcUri, "nostr+walletconnect://", "http://", 1)

	uri, err := url.Parse(nwcUri)
	//if you would get an error here, you typically "return nil, err"
	//so the return type would need to be a pointer
	//even tho you can just return a *url.URL
	//actually, I think this entire function is not necessary and can be deleted
	//and the models.go file can then also be deleted

	return WalletConnectConfig{
		RelayURL:     uri.Query().Get("relay"),
		WalletPubkey: uri.Host,
		Secret:       uri.Query().Get("secret"),
	}, err
}

// I would name this differently, like publish
func publisher(ctx context.Context, relay *nostr.Relay, resp *nostr.Event) error {
	status, err := relay.Publish(ctx, *resp)

	if err != nil {
		return err
	}

	//I would remove all of this
	//or move it one level up
	//in fact, I would remove this entire function
	if status == nostr.PublishStatusSucceeded {
		logrus.WithFields(logrus.Fields{
			"status":  status,
			"eventId": resp.ID,
		}).Info("Published reply")
	} else if status == nostr.PublishStatusFailed {
		logrus.WithFields(logrus.Fields{
			"status":  status,
			"eventId": resp.ID,
		}).Info("Failed to publish reply")
	} else {
		logrus.WithFields(logrus.Fields{
			"status":  status,
			"eventId": resp.ID,
		}).Info("Reply sent but no response from relay (timeout)")
	}

	return nil
}

// unused code
func createFilters(pubkey string, kind int) nostr.Filters {
	filter := nostr.Filter{
		Tags:  nostr.TagMap{"p": []string{pubkey}},
		Kinds: []int{NIP_47_REQUEST_KIND},
	}
	return []nostr.Filter{filter}
}
