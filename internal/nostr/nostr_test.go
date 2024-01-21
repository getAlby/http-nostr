package nostr

import (
	"encoding/json"
	"strings"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip04"
)

// unused
func eventGenerator() (*nostr.Event) {
	pubkey := "0b48335c805607f26a88150135dedc5b6d998f404aa12a9f906da3d8c65ec7f9"
	secret := "b1166fd992669c4ffaa16224b1f94dd2597f9cf5cbcbacabcdebaf3339f2b535"
	var params map[string]interface{}
	jsonStr := `{
    "method": "pay_invoice",
    "params": {
    "invoice": "lnbc120n1pj6e529pp5r427asvnqetgdju8f9utd5wxq4xj34tpqzwvf53vu0c6rv7cl3sqdp8v3352nnpg42yxnnpwvcny5jyw3gkvmtkd4u9vjqcqzzsxqyz5vqsp5ylmdsdrh23pj65frym4330uj5a7p7qztxxnsvn6qyleqkakgcuqs9qyyssq09t3q4xsr0q76e4mhz9fv3fv9w4pkh3dplyr5fspa72mg8a0jnj374wlsmyswqd0j653tz33mtafggravwx8rykcpkchhvgftpde3qsqe6l0mp"
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