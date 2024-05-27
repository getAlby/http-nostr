package nostr

import (
	"encoding/json"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"gorm.io/gorm"
)

const (
	NIP_47_INFO_EVENT_KIND = 13194
	NIP_47_REQUEST_KIND    = 23194
	NIP_47_RESPONSE_KIND   = 23195

	// state of request event
	REQUEST_EVENT_PUBLISH_CONFIRMED   = "confirmed"
	REQUEST_EVENT_PUBLISH_FAILED      = "failed"
)

type Subscription struct {
	ID            uint
	RelayUrl      string       `validate:"required"`
	WebhookUrl    string
	Open          bool
	Ids           *[]string     `gorm:"-"`
	Kinds         *[]int        `gorm:"-"`
	Authors       *[]string     `gorm:"-"` // WalletPubkey is included in this
	Tags          *nostr.TagMap `gorm:"-"` // RequestEvent ID goes in the "e" tag
	Since         time.Time
	Until         time.Time
	Limit         int
	Search        string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	Uuid          string        `gorm:"type:uuid;default:gen_random_uuid()"`

	// TODO: fix an elegant solution to store datatypes
	IdsString     string
	KindsString   string
	AuthorsString string
	TagsString    string
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

type RequestEvent struct {
	ID             uint
	SubscriptionId uint      `validate:"required"`
	NostrId        string    `validate:"required"`
	Content        string
	State          string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type ResponseEvent struct {
	ID             uint
	RequestId      *uint
	SubscriptionId uint      `validate:"required"`
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
	RelayUrl     string `json:"relayUrl"`
	WalletPubkey string `json:"walletPubkey"`
}

type NIP47Request struct {
	RelayUrl     string       `json:"relayUrl"`
	WalletPubkey string       `json:"walletPubkey"`
	WebhookUrl   string       `json:"webhookUrl"`
	SignedEvent  *nostr.Event `json:"event"`
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
