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
	ID             uint
	RelayUrl       string
	WebhookUrl     string
	PushToken      string
	Open           bool
	Ids            *[]string         `gorm:"-"`
	Kinds          *[]int            `gorm:"-"`
	Authors        *[]string         `gorm:"-"` // WalletPubkey is included in this
	Tags           *nostr.TagMap     `gorm:"-"` // RequestEvent ID goes in the "e" tag
	Since          time.Time
	Until          time.Time
	Limit          int
	Search         string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Uuid           string            `gorm:"type:uuid;default:gen_random_uuid()"`
	EventChan      chan *nostr.Event `gorm:"-"`
	RequestEvent   *RequestEvent     `gorm:"-"`

	// TODO: fix an elegant solution to store datatypes
	IdsString      string
	KindsString    string
	AuthorsString  string
	TagsString     string
}

func (s *Subscription) BeforeSave(tx *gorm.DB) error {
	var err error
	if s.Ids != nil {
			var idsJson []byte
			idsJson, err = json.Marshal(s.Ids)
			if err != nil {
					return err
			}
			s.IdsString = string(idsJson)
	}

	if s.Kinds != nil {
			var kindsJson []byte
			kindsJson, err = json.Marshal(s.Kinds)
			if err != nil {
					return err
			}
			s.KindsString = string(kindsJson)
	}

	if s.Authors != nil {
			var authorsJson []byte
			authorsJson, err = json.Marshal(s.Authors)
			if err != nil {
					return err
			}
			s.AuthorsString = string(authorsJson)
	}

	if s.Tags != nil {
			var tagsJson []byte
			tagsJson, err = json.Marshal(s.Tags)
			if err != nil {
					return err
			}
			s.TagsString = string(tagsJson)
	}

	return nil
}

func (s *Subscription) AfterFind(tx *gorm.DB) error {
	var err error
	if s.IdsString != "" {
			err = json.Unmarshal([]byte(s.IdsString), &s.Ids)
			if err != nil {
					return err
			}
	}

	if s.KindsString != "" {
			err = json.Unmarshal([]byte(s.KindsString), &s.Kinds)
			if err != nil {
					return err
			}
	}

	if s.AuthorsString != "" {
			err = json.Unmarshal([]byte(s.AuthorsString), &s.Authors)
			if err != nil {
					return err
			}
	}

	if s.TagsString != "" {
			err = json.Unmarshal([]byte(s.TagsString), &s.Tags)
			if err != nil {
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

type NIP47ExpoNotificationRequest struct {
	RelayUrl     string `json:"relayUrl"`
	PushToken    string `json:"pushToken"`
	WalletPubkey string `json:"walletPubkey"`
	ConnPubkey   string `json:"connectionPubkey"`
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
	SubscriptionId string `json:"subscriptionId"`
	WebhookUrl     string `json:"webhookUrl"`
}

type ExpoSubscriptionResponse struct {
	SubscriptionId string `json:"subscriptionId"`
	PushToken      string `json:"pushToken"`
	WalletPubkey   string `json:"walletPubkey"`
	AppPubkey      string `json:"appPubkey"`
}

type StopSubscriptionResponse struct {
	Message string `json:"message"`
	State   string `json:"state"`
}
