package nostr

import (
	"context"
	"errors"
	"net/http"
	"time"

	expo "github.com/getAlby/exponent-server-sdk-golang/sdk"
	"github.com/getAlby/go-nostr"
	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

func (svc *Service) InfoHandler(c echo.Context) error {
	var requestData InfoRequest
	if err := c.Bind(&requestData); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Error decoding info request",
			Error:   err.Error(),
		})
	}

	if requestData.WalletPubkey == "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Wallet pubkey is empty",
			Error:   "no wallet pubkey in request data",
		})
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), 90*time.Second)
	defer cancel()

	svc.Logger.WithFields(logrus.Fields{
		"relay_url":     requestData.RelayUrl,
		"wallet_pubkey": requestData.WalletPubkey,
	}).Debug("Fetching info event")

	filter := nostr.Filter{
		Authors: []string{requestData.WalletPubkey},
		Kinds:   []int{NIP_47_INFO_EVENT_KIND},
		Limit:   1,
	}

	event, err := svc.executeSyncRequest(ctx, requestData.RelayUrl, filter, nil)
	if err != nil {
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"relay_url":     requestData.RelayUrl,
			"wallet_pubkey": requestData.WalletPubkey,
		}).Error("Error fetching info event")

		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return c.JSON(http.StatusRequestTimeout, ErrorResponse{
				Message: "Request canceled or timed out",
				Error:   err.Error(),
			})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "Error connecting or subscribing to relay",
			Error:   err.Error(),
		})
	}

	return c.JSON(http.StatusOK, InfoResponse{
		Event: event,
	})
}

func (svc *Service) PublishHandler(c echo.Context) error {
	var requestData PublishRequest
	if err := c.Bind(&requestData); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Error decoding publish request",
			Error:   err.Error(),
		})
	}

	if requestData.SignedEvent == nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Signed event is empty",
			Error:   "no signed event in request data",
		})
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), 90*time.Second)
	defer cancel()

	relay, err := svc.getRelayConnection(ctx, requestData.RelayUrl)
	if err != nil {
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"event_id":  requestData.SignedEvent.ID,
			"relay_url": requestData.RelayUrl,
		}).Error("Error subscribing to relay")
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "Error connecting to relay",
			Error:   err.Error(),
		})
	}

	svc.Logger.WithFields(logrus.Fields{
		"event_id":  requestData.SignedEvent.ID,
		"relay_url": requestData.RelayUrl,
	}).Debug("Publishing event")

	err = relay.Publish(ctx, *requestData.SignedEvent)
	if err != nil {
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"event_id":  requestData.SignedEvent.ID,
			"relay_url": requestData.RelayUrl,
		}).Error("Failed to publish event")

		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "Error publishing the event",
			Error:   err.Error(),
		})
	}

	return c.JSON(http.StatusOK, PublishResponse{
		EventId:  requestData.SignedEvent.ID,
		RelayUrl: requestData.RelayUrl,
		State:    EVENT_PUBLISHED,
	})
}

func (svc *Service) NIP47Handler(c echo.Context) error {
	var requestData NIP47Request
	if err := c.Bind(&requestData); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Error decoding nip-47 request",
			Error:   err.Error(),
		})
	}

	if requestData.WalletPubkey == "" || requestData.SignedEvent == nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Missing required fields",
			Error:   "wallet pubkey or signed event is empty",
		})
	}

	if svc.db.Where("nostr_id = ?", requestData.SignedEvent.ID).Find(&RequestEvent{}).RowsAffected != 0 {
		return c.JSON(http.StatusBadRequest, NIP47Response{
			State: EVENT_ALREADY_PROCESSED,
		})
	}

	dbRequestEvent := RequestEvent{
		NostrId: requestData.SignedEvent.ID,
		Content: requestData.SignedEvent.Content,
		State:   EVENT_PUBLISH_PENDING,
	}

	if err := svc.db.Create(&dbRequestEvent).Error; err != nil {
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"request_event_id": requestData.SignedEvent.ID,
			"relay_url":        requestData.RelayUrl,
		}).Error("Failed to store request event")
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "Failed to store request event",
			Error:   err.Error(),
		})
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), 90*time.Second)
	defer cancel()

	filter := nostr.Filter{
		Authors: []string{requestData.WalletPubkey},
		Kinds:   []int{NIP_47_RESPONSE_KIND},
		Tags:    nostr.TagMap{"e": []string{requestData.SignedEvent.ID}},
		Limit:   1,
	}

	responseEvent, err := svc.executeSyncRequest(ctx, requestData.RelayUrl, filter, requestData.SignedEvent)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "Request failed or timed out",
			Error:   err.Error(),
		})
	}

	if err := svc.db.Model(&dbRequestEvent).Updates(RequestEvent{
		ResponseReceivedAt: time.Now(),
	}).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "Failed to update request event",
			Error:   err.Error(),
		})
	}

	dbResponseEvent := ResponseEvent{
		NostrId:   responseEvent.ID,
		Content:   responseEvent.Content,
		RepliedAt: responseEvent.CreatedAt.Time(),
		RequestId: &dbRequestEvent.ID,
	}

	if err := svc.db.Save(&dbResponseEvent).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "Failed to update subscription",
			Error:   err.Error(),
		})
	}

	return c.JSON(http.StatusOK, NIP47Response{
		Event: responseEvent,
		State: EVENT_PUBLISHED,
	})
}

func (svc *Service) NIP47WebhookHandler(c echo.Context) error {
	var requestData NIP47WebhookRequest
	if err := c.Bind(&requestData); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Error decoding nip-47 request",
			Error:   err.Error(),
		})
	}

	if requestData.WalletPubkey == "" || requestData.SignedEvent == nil || requestData.WebhookUrl == "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Missing required fields",
			Error:   "wallet pubkey, signed event, or webhook url is empty",
		})
	}

	if svc.db.Where("nostr_id = ?", requestData.SignedEvent.ID).Find(&RequestEvent{}).RowsAffected != 0 {
		return c.JSON(http.StatusBadRequest, NIP47Response{
			State: EVENT_ALREADY_PROCESSED,
		})
	}

	dbRequestEvent := RequestEvent{
		NostrId: requestData.SignedEvent.ID,
		Content: requestData.SignedEvent.Content,
		State:   EVENT_PUBLISH_PENDING,
	}

	if err := svc.db.Create(&dbRequestEvent).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "Failed to store request event",
			Error:   err.Error(),
		})
	}

	filter := nostr.Filter{
		Authors: []string{requestData.WalletPubkey},
		Kinds:   []int{NIP_47_RESPONSE_KIND},
		Tags:    nostr.TagMap{"e": []string{requestData.SignedEvent.ID}},
		Limit:   1,
	}

	go func() {
		if err := svc.processNIP47WebhookRequest(dbRequestEvent.ID, requestData.RelayUrl, requestData.WebhookUrl, filter, requestData.SignedEvent); err != nil {
			svc.Logger.WithError(err).WithFields(logrus.Fields{
				"request_event_id": requestData.SignedEvent.ID,
				"relay_url":        requestData.RelayUrl,
				"webhook_url":      requestData.WebhookUrl,
			}).Error("Failed to process webhook request")
			return
		}
	}()

	return c.JSON(http.StatusOK, NIP47Response{
		State: WEBHOOK_RECEIVED,
	})
}

func (svc *Service) SubscriptionHandler(c echo.Context) error {
	var requestData SubscriptionRequest
	if err := c.Bind(&requestData); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Error decoding subscription request",
			Error:   err.Error(),
		})
	}

	if requestData.Filter == nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Filters are empty",
			Error:   "no filters in request data",
		})
	}

	if requestData.WebhookUrl == "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Webhook URL is empty",
			Error:   "no webhook url in request data",
		})
	}

	subscription := subscriptionFromFilter(requestData.RelayUrl, requestData.Filter.Clone())
	subscription.WebhookUrl = requestData.WebhookUrl

	if err := svc.db.Create(&subscription).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "Failed to store subscription",
			Error:   err.Error(),
		})
	}

	svc.startSubscription(subscription, WebhookSubscriptionType)

	return c.JSON(http.StatusOK, SubscriptionResponse{
		SubscriptionId: subscription.Uuid,
		WebhookUrl:     requestData.WebhookUrl,
	})
}

func (svc *Service) StopSubscriptionHandler(c echo.Context) error {
	uuid := c.Param("id")

	subscription := Subscription{}
	if err := svc.db.First(&subscription, "uuid = ?", uuid).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return c.JSON(http.StatusNotFound, ErrorResponse{
				Message: "Subscription does not exist",
				Error:   err.Error(),
			})
		} else {
			return c.JSON(http.StatusInternalServerError, ErrorResponse{
				Message: "Error occurred while fetching subscription",
				Error:   err.Error(),
			})
		}
	}

	if !subscription.Open {
		return c.JSON(http.StatusAlreadyReported, StopSubscriptionResponse{
			Message: "Subscription is already closed",
			State:   SUBSCRIPTION_ALREADY_CLOSED,
		})
	}
	svc.cancelSubscription(subscription.Uuid)
	subscription.Open = false
	if err := svc.db.Save(&subscription).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "Failed to update subscription",
			Error:   err.Error(),
		})
	}

	return c.JSON(http.StatusOK, StopSubscriptionResponse{
		Message: "Subscription stopped successfully",
		State:   SUBSCRIPTION_CLOSED,
	})
}

func (svc *Service) NIP47NotificationHandler(c echo.Context) error {
	var requestData NIP47NotificationRequest
	if err := c.Bind(&requestData); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Error decoding notification request",
			Error:   err.Error(),
		})
	}

	if requestData.WebhookUrl == "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "webhook url is empty",
			Error:   "no webhook url in request data",
		})
	}

	if requestData.WalletPubkey == "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Wallet pubkey is empty",
			Error:   "no wallet pubkey in request data",
		})
	}

	if requestData.ConnPubkey == "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Connection pubkey is empty",
			Error:   "no connection pubkey in request data",
		})
	}

	filter := notificationFilter(requestData.WalletPubkey, requestData.ConnPubkey, requestData.Version)
	subscription := subscriptionFromFilter(requestData.RelayUrl, filter)
	subscription.WebhookUrl = requestData.WebhookUrl

	if err := svc.db.Create(&subscription).Error; err != nil {
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"wallet_pubkey": requestData.WalletPubkey,
			"relay_url":     requestData.RelayUrl,
			"webhook_url":   requestData.WebhookUrl,
		}).Error("Failed to store subscription")
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "Failed to store subscription",
			Error:   err.Error(),
		})
	}

	svc.startSubscription(subscription, WebhookSubscriptionType)

	return c.JSON(http.StatusOK, SubscriptionResponse{
		SubscriptionId: subscription.Uuid,
		WebhookUrl:     requestData.WebhookUrl,
	})
}

func (svc *Service) NIP47PushNotificationHandler(c echo.Context) error {
	var requestData NIP47PushNotificationRequest
	if err := c.Bind(&requestData); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Error decoding notification request",
			Error:   err.Error(),
		})
	}

	if requestData.PushToken == "" {
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

	if requestData.WalletPubkey == "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "wallet pubkey is empty",
			Error:   "no wallet pubkey in request data",
		})
	}

	if requestData.ConnPubkey == "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "connection pubkey is empty",
			Error:   "no connection pubkey in request data",
		})
	}

	encryptedPushToken, err := svc.encryptToken(requestData.PushToken)
	if err != nil {
		svc.Logger.WithError(err).Error("Failed to encrypt push token")
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "Internal server error",
			Error:   "failed to encrypt push token",
		})
	}

	var existingSubscriptions []Subscription
	if err := svc.db.
		Where("open = ?", true).
		Where("authors_json->>0 = ?", requestData.WalletPubkey).
		Where("tags_json->'p'->>0 = ?", requestData.ConnPubkey).
		Find(&existingSubscriptions).Error; err != nil {
		svc.Logger.WithError(err).Error("Failed to check existing subscriptions")
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "internal server error",
			Error:   err.Error(),
		})
	}

	for i := range existingSubscriptions {
		decrypted, err := svc.decryptToken(existingSubscriptions[i].PushToken)
		if err != nil {
			svc.Logger.WithError(err).Warn("Failed to decrypt push token in existing subscription")
			continue
		}
		if decrypted == requestData.PushToken {
			return c.JSON(http.StatusOK, PushSubscriptionResponse{
				SubscriptionId: existingSubscriptions[i].Uuid,
				PushToken:      requestData.PushToken,
				WalletPubkey:   requestData.WalletPubkey,
				AppPubkey:      requestData.ConnPubkey,
			})
		}
	}

	filter := notificationFilter(requestData.WalletPubkey, requestData.ConnPubkey, requestData.Version)
	subscription := subscriptionFromFilter(requestData.RelayUrl, filter)
	subscription.PushToken = encryptedPushToken
	subscription.IsIOS = requestData.IsIOS

	if err := svc.db.Create(&subscription).Error; err != nil {
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"wallet_pubkey": requestData.WalletPubkey,
			"relay_url":     requestData.RelayUrl,
		}).Error("Failed to store subscription")
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Message: "Failed to store subscription",
			Error:   err.Error(),
		})
	}

	svc.startSubscription(subscription, PushSubscriptionType)

	return c.JSON(http.StatusOK, PushSubscriptionResponse{
		SubscriptionId: subscription.Uuid,
		PushToken:      requestData.PushToken,
		WalletPubkey:   requestData.WalletPubkey,
		AppPubkey:      requestData.ConnPubkey,
	})
}
