package nostr

import "github.com/nbd-wtf/go-nostr"

const (
	NIP_47_INFO_EVENT_KIND = 13194
	NIP_47_REQUEST_KIND    = 23194
	NIP_47_RESPONSE_KIND   = 23195
)

type WalletConnectInfo struct {
	RelayURL     string
	WalletPubkey string
	Secret       string
}

type ErrorResponse struct {
	Message string `json:"message"`
}

type InfoRequest struct {
	RelayURL     string `json:"relayUrl"`
	WalletPubkey string `json:"walletPubkey"`
}

type NIP47Request struct {
	RelayURL     string       `json:"relayUrl"`
	WalletPubkey string       `json:"walletPubkey"`
	SignedEvent  *nostr.Event `json:"event"`
	WebhookURL   string       `json:"webhookURL"`
}
