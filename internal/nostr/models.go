package nostr

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"gorm.io/gorm"
)

const (
	NIP_47_INFO_EVENT_KIND   = 13194
	NIP_47_REQUEST_KIND      = 23194
	NIP_47_RESPONSE_KIND     = 23195
	NIP_47_NOTIFICATION_KIND = 23196

	REQUEST_EVENT_PUBLISH_CONFIRMED = "CONFIRMED"
	REQUEST_EVENT_PUBLISH_FAILED    = "FAILED"
	EVENT_PUBLISHED                 = "PUBLISHED"
	EVENT_ALREADY_PROCESSED         = "ALREADY_PROCESSED"
	WEBHOOK_RECEIVED                = "WEBHOOK_RECEIVED"
	SUBSCRIPTION_CLOSED             = "CLOSED"
	SUBSCRIPTION_ALREADY_CLOSED     = "ALREADY_CLOSED"
)

type Subscription struct {
	ID                uint
	RelayUrl          string
	WebhookUrl        string
	PushToken         string
	IsIOS             bool
	Open              bool
	Ids               *[]string           `gorm:"-"`
	Kinds             *[]int              `gorm:"-"`
	Authors           *[]string           `gorm:"-"` // WalletPubkey is included in this
	Tags              *nostr.TagMap       `gorm:"-"` // RequestEvent ID goes in the "e" tag
	Since             time.Time
	Until             time.Time
	Limit             int
	Search            string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Uuid              string              `gorm:"type:uuid;default:gen_random_uuid()"`
	EventChan         chan *nostr.Event   `gorm:"-"`
	RequestEvent      *RequestEvent       `gorm:"-"`
	RelaySubscription *nostr.Subscription `gorm:"-"`

	IdsJson           json.RawMessage     `gorm:"type:jsonb"`
	KindsJson         json.RawMessage     `gorm:"type:jsonb"`
	AuthorsJson       json.RawMessage     `gorm:"type:jsonb"`
	TagsJson          json.RawMessage     `gorm:"type:jsonb"`
}

func (s *Subscription) BeforeSave(tx *gorm.DB) error {
	var err error
	if s.Ids != nil {
		if s.IdsJson, err = json.Marshal(s.Ids); err != nil {
			return err
		}
	}

	if s.Kinds != nil {
		if s.KindsJson, err = json.Marshal(s.Kinds); err != nil {
			return err
		}
	}

	if s.Authors != nil {
		if s.AuthorsJson, err = json.Marshal(s.Authors); err != nil {
			return err
		}
	}

	if s.Tags != nil {
		if s.TagsJson, err = json.Marshal(s.Tags); err != nil {
			return err
		}
	}

	return nil
}

func (s *Subscription) AfterFind(tx *gorm.DB) error {
	var err error
	if len(s.IdsJson) > 0 {
		if err = json.Unmarshal(s.IdsJson, &s.Ids); err != nil {
			return err
		}
	}

	if len(s.KindsJson) > 0 {
		if err = json.Unmarshal(s.KindsJson, &s.Kinds); err != nil {
			return err
		}
	}

	if len(s.AuthorsJson) > 0 {
		if err = json.Unmarshal(s.AuthorsJson, &s.Authors); err != nil {
			return err
		}
	}

	if len(s.TagsJson) > 0 {
		if err = json.Unmarshal(s.TagsJson, &s.Tags); err != nil {
			return err
		}
	}

	return nil
}

type OnReceiveEOSFunc func(ctx context.Context, subscription *Subscription)

type HandleEventFunc func(event *nostr.Event, subscription *Subscription)

type RequestEvent struct {
	ID                 uint
	SubscriptionId     *uint
	NostrId            string       `validate:"required"`
	Content            string
	State              string
	ResponseReceivedAt time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
	SignedEvent        *nostr.Event `gorm:"-"`
}

type ResponseEvent struct {
	ID             uint
	RequestId      *uint
	SubscriptionId *uint
	NostrId        string    `validate:"required"`
	Content        string
	RepliedAt      time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type ErrorResponse struct {
	Message string `json:"message"`
	Error   string `json:"error"`
}

type InfoRequest struct {
	RelayUrl     string `json:"relayUrl"`
	WalletPubkey string `json:"walletPubkey"`
}

type InfoResponse struct {
	Event *nostr.Event `json:"event"`
}

type NIP47Request struct {
	RelayUrl     string       `json:"relayUrl"`
	WalletPubkey string       `json:"walletPubkey"`
	SignedEvent  *nostr.Event `json:"event"`
}

type NIP47WebhookRequest struct {
	RelayUrl     string       `json:"relayUrl"`
	WalletPubkey string       `json:"walletPubkey"`
	WebhookUrl   string       `json:"webhookUrl"`
	SignedEvent  *nostr.Event `json:"event"`
}

type NIP47NotificationRequest struct {
	RelayUrl     string `json:"relayUrl"`
	WebhookUrl   string `json:"webhookUrl"`
	WalletPubkey string `json:"walletPubkey"`
	ConnPubkey   string	`json:"connectionPubkey"`
}

type NIP47PushNotificationRequest struct {
	RelayUrl     string `json:"relayUrl"`
	PushToken    string `json:"pushToken"`
	WalletPubkey string `json:"walletPubkey"`
	ConnPubkey   string `json:"connectionPubkey"`
	IsIOS        bool   `json:"isIOS"`
}

type NIP47Response struct {
	Event  *nostr.Event `json:"event,omitempty"`
	State  string       `json:"state"`
}

type PublishRequest struct {
	RelayUrl    string       `json:"relayUrl"`
	SignedEvent *nostr.Event `json:"event"`
}

type PublishResponse struct {
	EventId  string `json:"eventId"`
	RelayUrl string `json:"relayUrl"`
	State    string `json:"state"`
}

type SubscriptionRequest struct {
	RelayUrl   string        `json:"relayUrl"`
	WebhookUrl string        `json:"webhookUrl"`
	Filter     *nostr.Filter `json:"filter"`
}

type SubscriptionResponse struct {
	SubscriptionId string `json:"subscription_id"`
	WebhookUrl     string `json:"webhookUrl"`
}

type PushSubscriptionResponse struct {
	SubscriptionId string `json:"subscriptionId"`
	PushToken      string `json:"pushToken"`
	WalletPubkey   string `json:"walletPubkey"`
	AppPubkey      string `json:"appPubkey"`
}

type StopSubscriptionResponse struct {
	Message string `json:"message"`
	State   string `json:"state"`
}
