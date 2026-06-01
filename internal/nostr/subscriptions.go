package nostr

import (
	"context"
	"errors"
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

func (svc *Service) startSubscription(subscription Subscription, subscriptionType PersistentSubscriptionType) {
	subCtx, subCancelFn := context.WithCancel(svc.Ctx)

	svc.subscriptionsMutex.Lock()
	defer svc.subscriptionsMutex.Unlock()

	if svc.subCancelFnMap == nil {
		svc.subCancelFnMap = make(map[string]context.CancelFunc)
	}
	svc.subCancelFnMap[subscription.Uuid] = subCancelFn

	go svc.startPersistentSubscription(subCtx, subscription, subscriptionType)
}

func (svc *Service) startPersistentSubscription(
	ctx context.Context,
	subscription Subscription,
	subscriptionType PersistentSubscriptionType,
) {
	filter := svc.subscriptionToFilter(&subscription)

	for {
		if ctx.Err() != nil {
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
			continue
		}

		relaySub, err := relay.Subscribe(ctx, nostr.Filters{*filter})
		if err != nil {
			continue
		}

		err = svc.processEvents(ctx, subscription, subscriptionType, relaySub)
		relaySub.Unsub()
		if err == nil {
			return
		}
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
