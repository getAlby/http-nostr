package nostr

import (
	"net/http"
	"time"

	expo "github.com/getAlby/exponent-server-sdk-golang/sdk"
	"github.com/labstack/echo/v4"
	"github.com/nbd-wtf/go-nostr"
	"github.com/sirupsen/logrus"
)

func (svc *Service) NIP47PushNotificationHandler(c echo.Context) error {
	var requestData NIP47PushNotificationRequest
	if err := c.Bind(&requestData); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Error decoding notification request",
			Error:   err.Error(),
		})
	}

	if (requestData.PushToken == "") {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "push token is empty",
			Error:   "no push token in request data",
		})
	}

	_, err := expo.NewExponentPushToken(requestData.PushToken)
	if err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "invalid push token",
			Error:   "invalid push token in request data",
		})
	}

	if (requestData.WalletPubkey == "") {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "wallet pubkey is empty",
			Error:   "no wallet pubkey in request data",
		})
	}

	if (requestData.ConnPubkey == "") {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "connection pubkey is empty",
			Error:   "no connection pubkey in request data",
		})
	}

	var existingSubscriptions []Subscription
	if err := svc.db.Where("push_token = ? AND open = ? AND authors_json->>0 = ? AND tags_json->'p'->>0 = ?", requestData.PushToken, true, requestData.WalletPubkey, requestData.ConnPubkey).Find(&existingSubscriptions).Error; err != nil {
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"push_token": requestData.PushToken,
		}).Error("Failed to check existing subscriptions")
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "internal server error",
			Error:   err.Error(),
		})
	}

	if len(existingSubscriptions) > 0 {
		existingSubscription := existingSubscriptions[0]
		svc.Logger.WithFields(logrus.Fields{
			"wallet_pubkey": requestData.WalletPubkey,
			"relay_url":     requestData.RelayUrl,
			"push_token":    requestData.PushToken,
		}).Debug("Subscription already started")
		return c.JSON(http.StatusOK, PushSubscriptionResponse{
			SubscriptionId: existingSubscription.Uuid,
			PushToken:      requestData.PushToken,
			WalletPubkey:   requestData.WalletPubkey,
			AppPubkey:      requestData.ConnPubkey,
		})
	}

	svc.Logger.WithFields(logrus.Fields{
		"wallet_pubkey": requestData.WalletPubkey,
		"relay_url":     requestData.RelayUrl,
		"push_token":    requestData.PushToken,
	}).Debug("Subscribing to send push notifications")

	subscription := Subscription{
		RelayUrl:   requestData.RelayUrl,
		PushToken:  requestData.PushToken,
		IsIOS:      requestData.IsIOS,
		Open:       true,
		Since:      time.Now(),
		Authors:    &[]string{requestData.WalletPubkey},
		Kinds:      &[]int{NIP_47_NOTIFICATION_KIND},
	}

	tags := make(nostr.TagMap)
	(tags)["p"] = []string{requestData.ConnPubkey}
	subscription.Tags = &tags

	err = svc.db.Create(&subscription).Error
	if err != nil {
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"wallet_pubkey": requestData.WalletPubkey,
			"relay_url":     requestData.RelayUrl,
			"push_token":    requestData.PushToken,
		}).Error("Failed to store subscription")
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Failed to store subscription",
			Error:   err.Error(),
		})
	}

	go svc.startSubscription(svc.Ctx, &subscription, nil, svc.handleSubscribedEventForPushNotification)

	return c.JSON(http.StatusOK, PushSubscriptionResponse{
		SubscriptionId: subscription.Uuid,
		PushToken:      requestData.PushToken,
		WalletPubkey:   requestData.WalletPubkey,
		AppPubkey:      requestData.ConnPubkey,
	})
}

func (svc *Service) handleSubscribedEventForPushNotification(event *nostr.Event, subscription *Subscription) {
	svc.Logger.WithFields(logrus.Fields{
		"event_id":        event.ID,
		"event_kind":      event.Kind,
		"subscription_id": subscription.ID,
		"relay_url":       subscription.RelayUrl,
	}).Debug("Received subscribed push notification")

	pushToken, _ := expo.NewExponentPushToken(subscription.PushToken)

	pushMessage := &expo.PushMessage{
		To: []expo.ExponentPushToken{pushToken},
		Data: map[string]string{
			"content": event.Content,
			"appPubkey": event.Tags.GetFirst([]string{"p", ""}).Value(),
		},
	}

	if subscription.IsIOS {
		pushMessage.Title = "Received notification"
		pushMessage.MutableContent = true
	}

	response, err := svc.client.Publish(pushMessage)
	if err != nil {
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"push_token": subscription.PushToken,
		}).Error("Failed to send push notification")
		return
	}

	err = response.ValidateResponse()
	if err != nil {
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"push_token": subscription.PushToken,
		}).Error("Failed to validate expo publish response")
		return
	}

	svc.Logger.WithFields(logrus.Fields{
		"event_id":   event.ID,
		"push_token": subscription.PushToken,
	}).Debug("Push notification sent successfully")
}
