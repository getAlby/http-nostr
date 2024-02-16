package nostr

import (
	"time"

	"github.com/nbd-wtf/go-nostr"
)

const (
	NIP_47_INFO_EVENT_KIND = 13194
	NIP_47_REQUEST_KIND    = 23194
	NIP_47_RESPONSE_KIND   = 23195

	SUBSCRIPTION_STATE_RECEIVED = "received"
	SUBSCRIPTION_STATE_EXECUTED = "executed"
	SUBSCRIPTION_STATE_ERROR    = "error"

	SUBSCRIPTION_STATE_PUBLISH_CONFIRMED   = "confirmed"
	SUBSCRIPTION_STATE_PUBLISH_FAILED      = "failed"
	SUBSCRIPTION_STATE_PUBLISH_UNCONFIRMED = "unconfirmed"
)

type Subscription struct {
	ID           uint
	Kind         int
	WalletPubkey string    `validate:"required"`
	NostrId      string    `validate:"required"`
	RelayUrl     string
	WebhookUrl   string
	State        string
	PublishState string
	RepliedAt    time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type ErrorResponse struct {
	Message string `json:"message"`
}

type InfoRequest struct {
	RelayURL     string `json:"relayUrl"`
	WalletPubkey string `json:"walletPubkey"`
}

type NIP47Request struct {
	RelayUrl     string       `json:"relayUrl"`
	WalletPubkey string       `json:"walletPubkey"`
	SignedEvent  *nostr.Event `json:"event"`
	WebhookUrl   string       `json:"webhookURL"`
}
