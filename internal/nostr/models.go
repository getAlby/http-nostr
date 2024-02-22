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
	ID         uint
	RelayUrl   string       `validate:"required"`
	WebhookUrl string
	Open       bool         // "open" / "closed"
	Ids        *[]string     `gorm:"type:jsonb"`
	Kinds      *[]int        `gorm:"type:jsonb"`
	Authors    *[]string     `gorm:"type:jsonb"` // WalletPubkey is included in this
	Tags       *nostr.TagMap `gorm:"type:jsonb"` // RequestEvent ID goes in the "e" tag
	Since      time.Time
	Until      time.Time
	Limit      int
	Search     string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type RequestEvent struct {
	ID             uint
	SubscriptionId uint
	NostrId        string    `validate:"required"`
	Content        string
	State          string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type ResponseEvent struct {
	ID             uint
	SubscriptionId uint
	RequestId      uint      `validate:"required"`
	NostrId        string    `validate:"required"`
	Content        string
	RepliedAt      time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
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
	WebhookUrl   string       `json:"webhookURL"`
	SignedEvent  *nostr.Event `json:"event"`
}
