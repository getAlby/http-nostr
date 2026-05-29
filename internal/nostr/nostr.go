package nostr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/getAlby/go-nostr"
	"github.com/sirupsen/logrus"
)

// executeSyncRequest connects to a relay, subscribes to a filter, and returns the first matching event.
// If eventToPublish is provided, it safely waits for the EndOfStoredEvents (EOSE) signal, drops stale
// historical events, publishes the request, and then waits for the response and returns it.
func (svc *Service) executeSyncRequest(ctx context.Context, relayUrl string, filter nostr.Filter, eventToPublish *nostr.Event) (*nostr.Event, error) {
	relay, err := svc.getRelayConnection(ctx, relayUrl)
	if err != nil {
		return nil, fmt.Errorf("error connecting to relay: %w", err)
	}

	sub, err := relay.Subscribe(ctx, []nostr.Filter{filter})
	if err != nil {
		return nil, fmt.Errorf("error subscribing to relay: %w", err)
	}
	defer sub.Unsub()

	waitingForEOSE := eventToPublish != nil

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-sub.EndOfStoredEvents:
			if waitingForEOSE {
				waitingForEOSE = false
				err := relay.Publish(ctx, *eventToPublish)

				state := REQUEST_EVENT_PUBLISH_CONFIRMED
				if err != nil {
					state = REQUEST_EVENT_PUBLISH_FAILED
				}

				if err := svc.db.Model(&RequestEvent{}).
					Where("nostr_id = ?", eventToPublish.ID).
					Update("state", state).Error; err != nil {
					return nil, err
				}

				if err != nil {
					return nil, err
				}
			}
		case event, ok := <-sub.Events:
			if !ok {
				return nil, fmt.Errorf("subscription events channel closed")
			}
			if waitingForEOSE {
				continue
			}
			return event, nil
		}
	}
}

func (svc *Service) getRelayConnection(ctx context.Context, relayURL string) (*nostr.Relay, error) {
	if relayURL == "" {
		// fall back to the first default relay
		relayURL = svc.Cfg.DefaultRelayURLs[0]
	}

	relayURL = nostr.NormalizeURL(relayURL)

	svc.relayMutex.RLock()
	relay, exists := svc.Relays[relayURL]
	svc.relayMutex.RUnlock()

	if exists && relay.IsConnected() {
		return relay, nil
	}

	// This blocks duplicate requests for the same relayURL to wait
	ch := svc.relayGroup.DoChan(relayURL, func() (interface{}, error) {
		svc.Logger.WithFields(logrus.Fields{
			"relay_url": relayURL,
		}).Info("Connecting to relay...")

		newRelay, err := svc.relayConnectWithBackoff(relayURL)
		if err != nil {
			return nil, err
		}

		svc.relayMutex.Lock()
		defer svc.relayMutex.Unlock()

		existingRelay, exists := svc.Relays[relayURL]
		if exists && existingRelay.IsConnected() {
			newRelay.Close()
			return existingRelay, nil
		}

		svc.Relays[relayURL] = newRelay
		return newRelay, nil
	})

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.(*nostr.Relay), nil
	}
}

func (svc *Service) relayConnectWithBackoff(relayURL string) (relay *nostr.Relay, err error) {
	attempt := 0
	wait := time.Duration(0)

	for {
		timer := time.NewTimer(wait)

		select {
		case <-svc.Ctx.Done():
			timer.Stop()

			svc.Logger.WithError(err).WithFields(logrus.Fields{
				"relay_url": relayURL,
			}).Errorf("Context canceled, exiting attempt to connect to relay")
			return nil, svc.Ctx.Err()
		case <-timer.C:
			relay, err = svc.connectToRelay(relayURL)
			if err == nil {
				svc.Logger.WithFields(logrus.Fields{
					"relay_url": relayURL,
				}).Info("Relay connection successful.")
				return relay, nil
			}

			attempt++
			if !svc.isDefaultRelay(relayURL) && attempt >= svc.Cfg.MaxRelayConnectionErrors {
				return nil, ErrRelayUnreachable
			}

			waitToReconnectSeconds := min(1<<(attempt-1), 900)
			wait = time.Duration(waitToReconnectSeconds) * time.Second

			svc.Logger.WithError(err).WithFields(logrus.Fields{
				"relay_url": relayURL,
			}).Errorf("Failed to connect to relay, retrying in %vs...", waitToReconnectSeconds)
		}
	}
}

func (svc *Service) connectToRelay(relayURL string) (*nostr.Relay, error) {
	headers := http.Header{}
	headers.Set("User-Agent", fmt.Sprintf("http-nostr/%s", Version))

	relay := nostr.NewRelay(svc.Ctx, relayURL, nostr.WithRequestHeader(headers))
	err := relay.Connect(svc.Ctx)
	return relay, err
}

func (svc *Service) isDefaultRelay(relayURL string) bool {
	normalized := nostr.NormalizeURL(relayURL)
	for _, defaultURL := range svc.Cfg.DefaultRelayURLs {
		if nostr.NormalizeURL(defaultURL) == normalized {
			return true
		}
	}
	return false
}

func (svc *Service) postEventToWebhook(event *nostr.Event, webhookUrl string) {
	eventData, err := json.Marshal(event)
	if err != nil {
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"response_event_id":   event.ID,
			"response_event_kind": event.Kind,
			"webhook_url":         webhookUrl,
		}).Error("Failed to marshal event for webhook")
		return
	}

	requestEventId := ""
	if eTag := event.Tags.Find("e"); len(eTag) >= 2 {
		requestEventId = eTag[1]
	}

	logFields := logrus.Fields{
		"request_event_id":    requestEventId,
		"response_event_id":   event.ID,
		"response_event_kind": event.Kind,
		"webhook_url":         webhookUrl,
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	resp, err := client.Post(webhookUrl, "application/json", bytes.NewBuffer(eventData))
	if err != nil {
		svc.Logger.WithError(err).WithFields(logFields).Error("Failed to post event to webhook")
		return
	}
	defer resp.Body.Close()

	svc.Logger.WithFields(logFields).Debug("Posted event to webhook")
}

func (svc *Service) subscriptionToFilter(subscription *Subscription) *nostr.Filter {
	filter := nostr.Filter{
		Limit:  subscription.Limit,
		Search: subscription.Search,
	}
	if subscription.Ids != nil {
		filter.IDs = *subscription.Ids
	}
	if subscription.Kinds != nil {
		filter.Kinds = *subscription.Kinds
	}
	if subscription.Authors != nil {
		filter.Authors = *subscription.Authors
	}
	if subscription.Tags != nil {
		filter.Tags = *subscription.Tags
	}
	if !subscription.Since.IsZero() {
		since := nostr.Timestamp(subscription.Since.Unix())
		filter.Since = &since
	}
	if !subscription.Until.IsZero() {
		until := nostr.Timestamp(subscription.Until.Unix())
		filter.Until = &until
	}
	return &filter
}
