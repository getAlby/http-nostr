![image](https://github.com/getAlby/http-nostr/assets/64399555/81516030-b3dd-44ab-be4f-5a5edf5dfcdd)

# HTTP-Nostr

HTTP-Nostr is designed to bridge the gap between the decentralized Nostr protocol and traditional HTTP endpoints with NIP-47 in mind. It offers seamless integration, allowing for real-time interaction with the Nostr network through a well-defined HTTP API. Developers/Nostr clients can fetch NIP-47 info, publish NIP-47 requests, or subscribe to events based on filters to create client side applications on Nostr without worrying about websockets.

For subscriptions/NIP-47 requests, HTTP-Nostr accepts a webhook URL to notify about the new events/response event.

<!-- ## 🛝 Try it out

Endpoints:  -->

## 🤙 Usage

### Fetch NIP-47 Info

Returns Relay's NIP-47 capabilities.

<details>
<summary>
<code>GET</code> <code><b>/info</b></code>
</summary>

#### Parameters

> None

#### Response

> ```json
> {
>   "id": "a16ye1391c22xx........xxxxx",
>   "pubkey": "a16y69effexxxx........xxxxx",
>   "created_at": 1708336682,
>   "kind": 13194,
>   "tags": [],
>   "content": "pay_invoice,pay_keysend,get_balance,get_info,make_invoice,lookup_invoice,list_transactions",
>   "sig": <signature>
> }
>```
</details>

------------------------------------------------------------------------------------------

### Publish NIP-47 Request

Returns the response event directly or to the Webhook URL if provided.

<details>
<summary>
<code>POST</code> <code><b>/nip47</b></code>
</summary>

#### Request Body

> | name      |  type     | data type               | description                                                           |
> |-----------|-----------|-------------------------|-----------------------------------------------------------------------|
> | relayUrl  |  optional | string           | If no relay is provided, it uses the default relay  |
> | webhookUrl  |  optional | string         | Webhook URL to publish the response event, returns the event directly if not provided  |
> | walletPubkey  |  required | string   | Pubkey of the NIP-47 Wallet Provider  |
> | event  |  required | JSON object ([nostr.Event](https://pkg.go.dev/github.com/nbd-wtf/go-nostr@v0.25.7#Event))  | **Signed** request event  |


#### Response (with webhook)

> "webhook received"

#### Response (without webhook)

> ```json
> {
>   "id": "a16ycf4a01bcxx........xxxxx",
>   "pubkey": "a16y69effexxxx........xxxxx",
>   "created_at": 1709033612,
>   "kind": 23195,
>   "tags": [
>       [
>           "p",
>           "f490f5xxxxx........xxxxx"
>       ],
>       [
>           "e",
>           "a41aefxxxxx........xxxxx"
>       ]
>   ],
>   "content": <encrypted content>,
>   "sig": <signature>
> }
>```
</details>

------------------------------------------------------------------------------------------

### Subscribe to Events

Notifies about new events matching the filter provided via webhooks.

<details>
<summary>
<code>POST</code> <code><b>/subscribe</b></code>
</summary>

#### Request Body

> | name      |  type     | data type               | description                                                           |
> |-----------|-----------|-------------------------|-----------------------------------------------------------------------|
> | relayUrl  |  optional | string           | If no relay is provided, it uses the default relay  |
> | webhookUrl  |  required | string         | Webhook URL to publish events |
> | filter  |  required | JSON object ([nostr.Filter](https://pkg.go.dev/github.com/nbd-wtf/go-nostr@v0.25.7#Filter)) | Filters to subscribe to |


#### Response

> ```json
> {
>   "subscription_id": 1,
>   "webhookUrl": "https://your.webhook.url"
> }
>```
</details>

------------------------------------------------------------------------------------------

### Delete Subscriptions

Delete previously requested subscriptions.

<details>
<summary>
<code>DELETE</code> <code><b>/subscribe/:id</b></code>
</summary>

#### Parameter

> | name      |  type     | data type               | description                                                           |
> |-----------|-----------|-------------------------|-----------------------------------------------------------------------|
> | id  |  required | string           | ID received on subscribing to a relay  |


#### Response

> ```json
> "subscription x stopped"
>```
</details>

------------------------------------------------------------------------------------------

## 🚀 Installation

### Requirements

The application has no runtime dependencies. (simple Go executable).

As data storage PostgreSQL should be used.

    $ cp .env.example .env
    # edit the config for your needs
    vim .env

## 🛠️ Development

`go run cmd/server/main.go`

## ⚙️ Configuration parameters

- `PORT`: the port on which the app should listen on (default: 8080)
- `DEFAULT_RELAY_URL`: the relay the app should subscribe to if nothing is provided (default: "wss://relay.getalby.com/v1")
- `DATABASE_URI`: postgres connection string

## ⭐ Contributing

Contributions to HTTP-Nostr are welcome! Whether it's bug reports, feature requests, or contributions to code, please feel free to make your suggestions known.

## 📄 License

HTTP-Nostr is released under the MIT License. See the [LICENSE file](./LICENSE) for more details.
