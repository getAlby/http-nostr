package nostr

import (
	"encoding/json"
	"strings"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip04"
)

// unused
func eventGenerator() (*nostr.Event) {
	pubkey := "xxxx"
	secret := "xxxx"
	var params map[string]interface{}
	jsonStr := `{
    "method": "pay_invoice",
    "params": {
    "invoice": "lnbcxxx"
    }
	}`
	decoder := json.NewDecoder(strings.NewReader(jsonStr))
	err := decoder.Decode(&params)
	if err != nil {
		return &nostr.Event{}
	}

	payloadJSON, err := json.Marshal(params)
	if err != nil {
		return &nostr.Event{}
	}

	ss, err := nip04.ComputeSharedSecret(pubkey, secret)
	if err != nil {
		return &nostr.Event{}
	}

	payload, err := nip04.Encrypt(string(payloadJSON), ss)
	if err != nil {
		return &nostr.Event{}
	}

	req := &nostr.Event{
		PubKey:    pubkey,
		CreatedAt: nostr.Now(),
		Kind:      NIP_47_REQUEST_KIND,
		Tags:      nostr.Tags{[]string{"p", pubkey}},
		Content:   payload,
	}

	err = req.Sign(secret)
	if err != nil {
		return &nostr.Event{}
	}

	return req
}