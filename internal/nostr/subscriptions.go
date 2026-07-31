package nostr

import (
	"context"
	"errors"
	"math/rand"
	"time"

	expo "github.com/getAlby/exponent-server-sdk-golang/sdk"
	"github.com/getAlby/go-nostr"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

func (svc *Service) cancelSubscription(uuid string) bool {
	svc.subscriptionsMutex.Lock()
	defer svc.subscriptionsMutex.Unlock()

	cancelFn, exists := svc.subCancelFnMap[uuid]
	if exists {
		delete(svc.subCancelFnMap, uuid)
		cancelFn()
	}

	return exists
}

// startSubscription starts a newly created subscription, which connects
// straight away.
func (svc *Service) startSubscription(subscription Subscription, subscriptionType PersistentSubscriptionType) {
	svc.startSubscriptionAfter(subscription, subscriptionType, 0)
}

// restoreSubscription starts a subscription that already existed before this
// process did, as when open subscriptions are reloaded at startup. Those are
// restored in bulk, so each waits a randomised moment first; otherwise every
// subscription in the database would connect in the same instant, which is
// the burst this file's backoff exists to avoid.
func (svc *Service) restoreSubscription(subscription Subscription, subscriptionType PersistentSubscriptionType) {
	svc.startSubscriptionAfter(subscription, subscriptionType, resubscribeDelay(1))
}

func (svc *Service) startSubscriptionAfter(
	subscription Subscription,
	subscriptionType PersistentSubscriptionType,
	initialDelay time.Duration,
) {
	subCtx, subCancelFn := context.WithCancel(svc.Ctx)

	svc.subscriptionsMutex.Lock()
	defer svc.subscriptionsMutex.Unlock()

	if svc.subCancelFnMap == nil {
		svc.subCancelFnMap = make(map[string]context.CancelFunc)
	}
	svc.subCancelFnMap[subscription.Uuid] = subCancelFn

	go svc.startPersistentSubscription(subCtx, subscription, subscriptionType, initialDelay)
}

const (
	// Width of the randomisation window for the first resubscribe attempt.
	// Wide enough that a large number of subscriptions resubscribing at once
	// arrives at a rate a relay can serve, without delaying recovery unduly.
	resubscribeBaseDelay = 15 * time.Second
	// Upper bound on that window, however many attempts have failed.
	resubscribeMaxDelay = 2 * time.Minute
	// A subscription that stayed up at least this long counts as healthy, so
	// the next failure starts over from resubscribeBaseDelay.
	resubscribeStableAfter = 1 * time.Minute
)

// waitBeforeResubscribe pauses before the caller retries, reporting false if
// ctx was cancelled while waiting.
//
// The pause is drawn uniformly from [0, window) — "full jitter" — instead of
// being a fixed sleep. Every persistent subscription watches the same shared
// relay connection, so a single connection failure wakes all of them at the
// same moment. Retrying immediately, or after an identical delay, sends them
// back as one synchronised burst, which a relay may not be able to serve; the
// resulting failures return every subscription to this loop and rebuild the
// burst. Randomising per subscription decorrelates the retries instead.
func waitBeforeResubscribe(ctx context.Context, attempt int) bool {
	return sleepCtx(ctx, resubscribeDelay(attempt))
}

// sleepCtx waits for d, reporting false if ctx was cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// resubscribeDelay returns the randomised delay for the given attempt, where
// attempt is 1 for the first retry. The window doubles per attempt up to
// resubscribeMaxDelay; the returned value is uniform within it.
func resubscribeDelay(attempt int) time.Duration {
	window := resubscribeBaseDelay << min(attempt-1, 5)
	if window > resubscribeMaxDelay {
		window = resubscribeMaxDelay
	}

	return time.Duration(rand.Int63n(int64(window)))
}

func (svc *Service) startPersistentSubscription(
	ctx context.Context,
	subscription Subscription,
	subscriptionType PersistentSubscriptionType,
	initialDelay time.Duration,
) {
	if initialDelay > 0 && !sleepCtx(ctx, initialDelay) {
		return
	}

	filter := svc.subscriptionToFilter(&subscription)

	attempt := 0

	for {
		if ctx.Err() != nil {
			return
		}

		// Skipped on the first pass so a freshly created subscription still
		// connects immediately.
		if attempt > 0 && !waitBeforeResubscribe(ctx, attempt) {
			return
		}

		relay, err := svc.getRelayConnection(ctx, subscription.RelayUrl)
		if err != nil {
			if errors.Is(err, ErrRelayUnreachable) {
				subscription.Open = false
				if err := svc.db.Model(&Subscription{}).
					Where("id = ?", subscription.ID).
					Updates(Subscription{Open: false}).Error; err != nil {
					svc.Logger.WithError(err).WithFields(logrus.Fields{
						"subscription_id": subscription.Uuid,
						"relay_url":       subscription.RelayUrl,
					}).Error("Failed to mark subscription as closed")
				}
				svc.cancelSubscription(subscription.Uuid)
				return
			}
			attempt++
			continue
		}

		relaySub, err := relay.Subscribe(ctx, nostr.Filters{*filter})
		if err != nil {
			attempt++
			continue
		}

		subscribedAt := time.Now()

		err = svc.processEvents(ctx, subscription, subscriptionType, relaySub)
		relaySub.Unsub()
		if err == nil {
			return
		}

		// Only a subscription that survived a while indicates the relay is
		// healthy; without this a relay that closes subscriptions instantly
		// would keep resetting the backoff and never actually back off.
		if time.Since(subscribedAt) >= resubscribeStableAfter {
			attempt = 0
		}
		attempt++
	}
}

func (svc *Service) processEvents(
	ctx context.Context,
	subscription Subscription,
	subscriptionType PersistentSubscriptionType,
	relaySub *nostr.Subscription,
) error {
	for {
		select {
		case event, ok := <-relaySub.Events:
			if !ok {
				if relaySub.Relay.Context().Err() != nil {
					return relaySub.Relay.ConnectionError
				}
				if err := context.Cause(relaySub.Context); err != nil {
					return err
				}
				return nil
			}

			switch subscriptionType {
			case WebhookSubscriptionType:
				go svc.handleWebhookSubscriptionEvent(event, &subscription)
			case PushSubscriptionType:
				go svc.handlePushSubscriptionEvent(event, &subscription)
			}
		case <-relaySub.Relay.Context().Done():
			return relaySub.Relay.ConnectionError
		case <-relaySub.Context.Done():
			return context.Cause(relaySub.Context)
		case <-ctx.Done():
			return nil
		}
	}
}

func (svc *Service) handleWebhookSubscriptionEvent(event *nostr.Event, subscription *Subscription) {
	svc.Logger.WithFields(logrus.Fields{
		"subscription_id": subscription.Uuid,
		"event_id":        event.ID,
		"relay_url":       subscription.RelayUrl,
		"webhook_url":     subscription.WebhookUrl,
	}).Debug("Received subscribed webhook event")

	if err := svc.storeSubscribedEvent(subscription, event); err != nil {
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"subscription_id": subscription.Uuid,
			"event_id":        event.ID,
		}).Error("Failed to store subscription event receipt")
	}

	if err := svc.postEventToWebhook(event, subscription.WebhookUrl); err != nil {
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"subscription_id": subscription.Uuid,
			"event_id":        event.ID,
			"webhook_url":     subscription.WebhookUrl,
		}).Error("Failed to post event to webhook")
	}
}

func (svc *Service) handlePushSubscriptionEvent(event *nostr.Event, subscription *Subscription) {
	svc.Logger.WithFields(logrus.Fields{
		"subscription_id": subscription.Uuid,
		"event_id":        event.ID,
		"relay_url":       subscription.RelayUrl,
	}).Debug("Received subscribed push event")

	decryptedPushToken, err := svc.decryptToken(subscription.PushToken)
	if err != nil {
		svc.Logger.WithError(err).Error("Failed to decrypt push token")
		return
	}

	pushToken, err := expo.NewExponentPushToken(decryptedPushToken)
	if err != nil {
		svc.Logger.WithError(err).Error("Invalid stored push token")
		return
	}

	lastEventReceivedAt := time.Now()
	if err := svc.db.Model(&Subscription{}).
		Where("id = ?", subscription.ID).
		Updates(Subscription{LastEventReceivedAt: lastEventReceivedAt}).Error; err != nil {
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"subscription_id": subscription.Uuid,
			"event_id":        event.ID,
		}).Error("Failed to update subscription last event timestamp")
	}

	appPubkey := ""
	if pTag := event.Tags.Find("p"); pTag != nil {
		appPubkey = pTag[1]
	}

	pushMessage := &expo.PushMessage{
		To: []expo.ExponentPushToken{pushToken},
		Data: map[string]string{
			"content":   event.Content,
			"appPubkey": appPubkey,
		},
	}

	if subscription.IsIOS {
		pushMessage.Title = "Received notification"
		pushMessage.MutableContent = true
	}

	response, err := svc.client.Publish(pushMessage)
	if err != nil {
		svc.Logger.WithError(err).Error("Failed to send push notification")
		return
	}

	if err := response.ValidateResponse(); err != nil {
		svc.Logger.WithError(err).Error("Failed to validate expo publish response")
		return
	}
}

func (svc *Service) storeSubscribedEvent(subscription *Subscription, event *nostr.Event) error {
	lastEventReceivedAt := time.Now()

	return svc.db.Transaction(func(tx *gorm.DB) error {
		responseEvent := ResponseEvent{
			NostrId:        event.ID,
			Content:        event.Content,
			RepliedAt:      event.CreatedAt.Time(),
			SubscriptionId: &subscription.ID,
		}
		if err := tx.Save(&responseEvent).Error; err != nil {
			return err
		}

		return tx.Model(&Subscription{}).
			Where("id = ?", subscription.ID).
			Updates(Subscription{LastEventReceivedAt: lastEventReceivedAt}).Error
	})
}

func notificationFilter(walletPubkey, connPubkey, version string) nostr.Filter {
	kinds := []int{LEGACY_NIP_47_NOTIFICATION_KIND}
	if version == "1.0" {
		kinds = []int{NIP_47_NOTIFICATION_KIND}
	}

	since := nostr.Now()

	return nostr.Filter{
		Authors: []string{walletPubkey},
		Kinds:   kinds,
		Tags: nostr.TagMap{
			"p": []string{connPubkey},
		},
		Since: &since,
	}
}

func subscriptionFromFilter(relayURL string, filter nostr.Filter) Subscription {
	subscription := Subscription{
		RelayUrl: relayURL,
		Open:     true,
		Ids:      &filter.IDs,
		Authors:  &filter.Authors,
		Kinds:    &filter.Kinds,
		Tags:     &filter.Tags,
		Limit:    filter.Limit,
		Search:   filter.Search,
		Uuid:     uuid.NewString(),
	}

	if filter.Since != nil {
		subscription.Since = filter.Since.Time()
	}
	if filter.Until != nil {
		subscription.Until = filter.Until.Time()
	}

	return subscription
}
