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

				svc.Logger.WithFields(logrus.Fields{
					"event_id":  eventToPublish.ID,
					"relay_url": relayUrl,
				}).Debug("Received EOSE, publishing request event")

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

func (svc *Service) processNIP47WebhookRequest(requestID uint, relayUrl, webhookUrl string, filter nostr.Filter, signedEvent *nostr.Event) error {
	bgCtx, cancel := context.WithTimeout(svc.Ctx, 90*time.Second)
	defer cancel()

	responseEvent, err := svc.executeSyncRequest(bgCtx, relayUrl, filter, signedEvent)
	if err != nil {
		return err
	}

	if err := svc.db.Model(&RequestEvent{}).Where("id = ?", requestID).Updates(RequestEvent{
		ResponseReceivedAt: time.Now(),
	}).Error; err != nil {
		return err
	}

	dbResponseEvent := ResponseEvent{
		NostrId:   responseEvent.ID,
		Content:   responseEvent.Content,
		RepliedAt: responseEvent.CreatedAt.Time(),
		RequestId: &requestID,
	}
	if err := svc.db.Save(&dbResponseEvent).Error; err != nil {
		return err
	}

	return svc.postEventToWebhook(responseEvent, webhookUrl)
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

			backoffExponent := min(attempt-1, 6)
			waitToReconnectSeconds := min(1<<backoffExponent, 60)
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

func (svc *Service) postEventToWebhook(event *nostr.Event, webhookUrl string) error {
	eventData, err := json.Marshal(event)
	if err != nil {
		return err
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	resp, err := client.Post(webhookUrl, "application/json", bytes.NewBuffer(eventData))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return nil
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
