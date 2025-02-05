package nostr

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"time"

	expo "github.com/getAlby/exponent-server-sdk-golang/sdk"
	"github.com/labstack/echo/v4"
	"github.com/nbd-wtf/go-nostr"
	"github.com/sirupsen/logrus"
)

func (svc *Service) encryptToken(token string) (string, error) {
	key := []byte(svc.Cfg.EncryptionKey)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(token), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func (svc *Service) decryptToken(encryptedToken string) (string, error) {
	key := []byte(svc.Cfg.EncryptionKey)
	ciphertext, err := base64.StdEncoding.DecodeString(encryptedToken)
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	if len(ciphertext) < gcm.NonceSize() {
		return "", errors.New("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}

	return string(plaintext), nil
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

	var existingSubscription *Subscription
	for i, sub := range existingSubscriptions {
		decrypted, err := svc.decryptToken(sub.PushToken)
		if err != nil {
			svc.Logger.WithError(err).Warn("Failed to decrypt push token in existing subscription")
			continue
		}

		if decrypted == requestData.PushToken {
			existingSubscription = &existingSubscriptions[i]
			break
		}
	}

	if existingSubscription != nil {
		svc.Logger.WithFields(logrus.Fields{
			"wallet_pubkey": requestData.WalletPubkey,
			"relay_url":     requestData.RelayUrl,
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
	}).Debug("Subscribing to send push notifications")

	subscription := Subscription{
		RelayUrl:   requestData.RelayUrl,
		PushToken:  encryptedPushToken,
		IsIOS:      requestData.IsIOS,
		Open:       true,
		Since:      time.Now(),
		Authors:    &[]string{requestData.WalletPubkey},
		Kinds:      &[]int{LEGACY_NIP_47_NOTIFICATION_KIND},
	}

	if requestData.Version == "1.0" {
		subscription.Kinds = &[]int{NIP_47_NOTIFICATION_KIND}
	}

	tags := make(nostr.TagMap)
	tags["p"] = []string{requestData.ConnPubkey}
	subscription.Tags = &tags

	err = svc.db.Create(&subscription).Error
	if err != nil {
		svc.Logger.WithError(err).WithFields(logrus.Fields{
			"wallet_pubkey": requestData.WalletPubkey,
			"relay_url":     requestData.RelayUrl,
		}).Error("Failed to store subscription")
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Message: "Failed to store subscription",
			Error:   err.Error(),
		})
	}

	subCtx, subCancelFn := context.WithCancel(svc.Ctx)
	svc.subscriptionsMutex.Lock()
	svc.subCancelFnMap[subscription.Uuid] = subCancelFn
	svc.subscriptionsMutex.Unlock()
	go svc.startSubscription(subCtx, &subscription, nil, svc.handleSubscribedEventForPushNotification)

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

	decryptedPushToken, err := svc.decryptToken(subscription.PushToken)
	if err != nil {
		svc.Logger.WithError(err).Error("Failed to decrypt push token")
		return
	}

	pushToken, _ := expo.NewExponentPushToken(decryptedPushToken)

	pushMessage := &expo.PushMessage{
		To: []expo.ExponentPushToken{pushToken},
		Data: map[string]string{
			"content":   event.Content,
			"appPubkey": event.Tags.GetFirst([]string{"p", ""}).Value(),
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

	err = response.ValidateResponse()
	if err != nil {
		svc.Logger.WithError(err).Error("Failed to validate expo publish response")
		return
	}

	svc.Logger.WithFields(logrus.Fields{
		"event_id":   event.ID,
	}).Debug("Push notification sent successfully")
}
