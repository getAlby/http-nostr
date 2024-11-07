package nostr

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/nbd-wtf/go-nostr"
	expo "github.com/oliveroneill/exponent-server-sdk-golang/sdk"
	"github.com/sirupsen/logrus"
)

func (svc *Service) NIP47ExpoNotificationHandler(c echo.Context) error {
	var requestData NIP47ExpoNotificationRequest
	// send in a pubkey and authenticate by signing
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

	svc.Logger.WithFields(logrus.Fields{
		"wallet_pubkey": requestData.WalletPubkey,
		"relay_url":     requestData.RelayUrl,
		"push_token":   requestData.PushToken,
	}).Debug("Subscribing to send push notifications")

	subscription := Subscription{
		RelayUrl:   requestData.RelayUrl,
		PushToken:  requestData.PushToken,
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

	go svc.startSubscription(svc.Ctx, &subscription, nil, svc.handleSubscribedExpoNotification)

	return c.NoContent(http.StatusOK)
}

func (svc *Service) handleSubscribedExpoNotification(event *nostr.Event, subscription *Subscription) {
	svc.Logger.WithFields(logrus.Fields{
		"event_id":        event.ID,
		"event_kind":      event.Kind,
		"subscription_id": subscription.ID,
		"relay_url":       subscription.RelayUrl,
	}).Debug("Received subscribed notification")

	pushToken, _ := expo.NewExponentPushToken(subscription.PushToken)

	response, err := svc.client.Publish(
		&expo.PushMessage{
			To: []expo.ExponentPushToken{pushToken},
			Title: "New event",
			Body: "",
			Data: map[string]string{
				"content": event.Content,
				"appPubkey": event.Tags.GetFirst([]string{"p", ""}).Value(),
			},
			Priority: expo.DefaultPriority,
		},
	)

	someerr := response.ValidateResponse()
	if err != nil || someerr != nil {
		svc.Logger.WithFields(logrus.Fields{
			"push_token":      subscription.PushToken,
		}).Error("Failed to send expo notification")
		return
	}

	svc.Logger.WithFields(logrus.Fields{
		"event_id":        event.ID,
		"push_token":      subscription.PushToken,
	}).Debug("Push notification sent successfully")
}
