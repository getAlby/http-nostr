![image](https://github.com/getAlby/http-nostr/assets/64399555/81516030-b3dd-44ab-be4f-5a5edf5dfcdd)

# HTTP-Nostr

HTTP-Nostr is designed to bridge the gap between the decentralized Nostr protocol and traditional HTTP endpoints with NIP-47 in mind. It offers seamless integration, allowing for real-time interaction with the Nostr network through a well-defined HTTP API. Developers/Nostr clients can fetch NIP-47 info, publish NIP-47 requests, or subscribe to events based on filters to create client side applications on Nostr without worrying about websockets.

For subscriptions/NIP-47 requests, HTTP-Nostr accepts a webhook URL to notify about the new events/response event.

<!-- ## 🛝 Try it out

Endpoints:  -->

## 🤙 Usage

### Fetch NWC Info

Every NWC-enabled wallet has a pubkey according to the NWC specification.
This `GET` request returns a pubkey's NWC capabilities (if any)

<details>
<summary>
<code>POST</code> <code><b>/nip47/info</b></code>
</summary>

#### Request Body

| name      |  type     | data type               | description                                                           |
|-----------|-----------|-------------------------|-----------------------------------------------------------------------|
| relayUrl  |  optional | string           | If no relay is provided, it uses the default relay (wss://relay.getalby.com/v1)  |
| walletPubkey  |  required | string   | Pubkey of the NWC Wallet Provider  |

#### Response

```json
{
  "event": {
    "id": "a16ye1391c22xx........xxxxx",
    "pubkey": "a16y69effexxxx........xxxxx",
    "created_at": 1708336682,
    "kind": 13194,
    "tags": [],
    "content": "pay_invoice, pay_keysend, get_balance, get_info, make_invoice, lookup_invoice, list_transactions",
    "sig": "<signature>"
  }
}
```
</details>

------------------------------------------------------------------------------------------

### Publish NWC Request

Returns the response event directly or to the Webhook URL if provided.

<details>
<summary>
<code>POST</code> <code><b>/nip47</b></code>
</summary>

#### Request Body

| name      |  type     | data type               | description                                                           |
|-----------|-----------|-------------------------|-----------------------------------------------------------------------|
| relayUrl  |  optional | string           | If no relay is provided, it uses the default relay (wss://relay.getalby.com/v1)  |
| webhookUrl  |  optional | string         | Webhook URL to publish the response event, returns the event directly if not provided  |
| walletPubkey  |  required | string   | Pubkey of the NWC Wallet Provider  |
| event  |  required | JSON object (see [example](#event-example))  | **Signed** request event  |


#### Event Example

```json
{
  "id": "a16ycf4a01bcxx........xxxxx",
  "pubkey": "a16y69effexxxx........xxxxx",
  "created_at": 1700000021,
  "kind": 23194,
  "tags": [
    [
      "p",
      "a16y6sfa01bcxx........xxxxx"
    ]
  ],
  "content": "<encrypted content>",
  "sig":"<signature>"
}
// Source: https://pkg.go.dev/github.com/nbd-wtf/go-nostr@v0.30.0#Event
```

#### Response (with webhook)

```json
{
  "state": "WEBHOOK_RECEIVED"
}
```

#### Response (without webhook)

```json
{
  "event": {
    "id": "a16ycf4a01bcxx........xxxxx",
    "pubkey": "a16y69effexxxx........xxxxx",
    "created_at": 1709033612,
    "kind": 23195,
    "tags": [
      [
        "p",
        "f490f5xxxxx........xxxxx"
      ],
      [
        "e",
        "a41aefxxxxx........xxxxx"
      ]
    ],
    "content": "<encrypted content>",
    "sig": "<signature>",
  },
  "state": "PUBLISHED"
}
```
</details>

------------------------------------------------------------------------------------------

### Publish Event

Publishes any **signed** event to the specified relay.

<details>
<summary>
<code>POST</code> <code><b>/publish</b></code>
</summary>

#### Request Body

| name      |  type     | data type               | description                                                           |
|-----------|-----------|-------------------------|-----------------------------------------------------------------------|
| relayUrl  |  optional | string           | If no relay is provided, it uses the default relay (wss://relay.getalby.com/v1)  |
| event  |  required | JSON object (see [example](#event-example))  | **Signed** event  |

#### Response

```json
{
  "eventId": "a16ycf4a01bcxx........xxxxx",
  "relayUrl": "wss://relay.custom.com/v1",
  "state": "PUBLISHED",
}
```

</details>

------------------------------------------------------------------------------------------

### Subscribe to Events

Notifies about new events matching the filter provided via webhooks.

<details>
<summary>
<code>POST</code> <code><b>/subscriptions</b></code>
</summary>

#### Request Body

| name      |  type     | data type               | description                                                           |
|-----------|-----------|-------------------------|-----------------------------------------------------------------------|
| relayUrl  |  optional | string           | If no relay is provided, it uses the default relay  |
| webhookUrl  |  required | string         | Webhook URL to publish events |
| filter  |  required | JSON object (see [example](#filter-example)) | Filters to subscribe to |


#### Filter Example

```json
{
  "ids": ["id1", "id2"],
  "kinds": [1,2],
  "authors": ["author1", "author2"],
  "since": 1721212121,
  "until": 1721212121,
  "limit": 10,
  "search": "example search",
  "#tag1": ["value1", "value2"],
  "#tag2": ["value3"],
  "#tag3": ["value4", "value5", "value6"],
}
// Source: https://pkg.go.dev/github.com/nbd-wtf/go-nostr@v0.30.0#Filter
```

#### Response

```json
{
  "subscription_id": "f370d1fc-x0x0-x0x0-x0x0-8f68fa12f32c",
  "webhookUrl": "https://your.webhook.url"
}
```

#### Response to Webhook URL

```json
{
  "id": "a16ycf4a01bcxx........xxxxx",
  "pubkey": "a16y69effexxxx........xxxxx",
  "created_at": 1709033612,
  "kind": 23195,
  "tags": [
    [
      "p",
      "f490f5xxxxx........xxxxx"
    ],
    [
      "e",
      "a41aefxxxxx........xxxxx"
    ]
  ],
  "content": "<encrypted content>",
  "sig": "<signature>"
}
```

</details>

------------------------------------------------------------------------------------------

### Subscribe to NWC Notifications

Notifies about `23196` kind events (NWC notifications) of the connection pubkey from the wallet service.

<details>
<summary>
<code>POST</code> <code><b>/nip47/notifications</b></code>
</summary>

#### Request Body

| name      |  type     | data type               | description                                                           |
|-----------|-----------|-------------------------|-----------------------------------------------------------------------|
| relayUrl  |  optional | string           | If no relay is provided, it uses the default relay  |
| webhookUrl  |  required | string         | Webhook URL to publish events |
| walletPubkey  |  required | string         | Pubkey of the NWC Wallet Provider |
| connectionPubkey  |  required | string         | Public key of the user (derived from secret in NWC connection string) |


#### Response

```json
{
  "subscription_id": "f370d1fc-x0x0-x0x0-x0x0-8f68fa12f32c",
  "webhookUrl": "https://your.webhook.url"
}
```

#### Response to Webhook URL

```json
{
  "id": "a16ycf4a01bcxx........xxxxx",
  "pubkey": "a16y69effexxxx........xxxxx",
  "created_at": 1709033612,
  "kind": 23196,
  "tags": [
    [
      "p",
      "f490f5xxxxx........xxxxx"
    ],
    [
      "e",
      "a41aefxxxxx........xxxxx"
    ]
  ],
  "content": "<encrypted content>",
  "sig": "<signature>"
}
```

</details>

------------------------------------------------------------------------------------------

### Delete Subscriptions

Delete previously requested subscriptions.

<details>
<summary>
<code>DELETE</code> <code><b>/subscriptions/:id</b></code>
</summary>

#### Parameter

| name      |  type     | data type               | description                                                           |
|-----------|-----------|-------------------------|-----------------------------------------------------------------------|
| id  |  required | string           | UUID received on subscribing to a relay  |


#### Response

```json
{
  "message": "Subscription stopped successfully",
  "state": "CLOSED"
}
```
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
