package nostr

const (
	NIP_47_INFO_EVENT_KIND            = 13194
	NIP_47_REQUEST_KIND               = 23194
	NIP_47_RESPONSE_KIND              = 23195
)

type WalletConnectConfig struct {
	RelayURL      string
	WalletPubkey  string
	Secret        string
}
