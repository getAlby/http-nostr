package nostr

const (
	NIP_47_INFO_EVENT_KIND            = 13194
	NIP_47_REQUEST_KIND               = 23194
	NIP_47_RESPONSE_KIND              = 23195
)

type NIP47HTTPInfo struct {
	NwcUri        string                 `json:"nwc_uri" validate:"required,uri"`
}

type NIP47HTTPReq struct {
	NwcUri        string                 `json:"nwc_uri" validate:"required,uri"`
	Method        string                 `json:"method"`
	Params        map[string]interface{} `json:"params"`
}

type WalletConnectConfig struct {
	RelayURL      string
	WalletPubkey  string
	Secret        string
}
