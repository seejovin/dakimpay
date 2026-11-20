# Merchant webhooks

Subscriptions deliver the committed merchant event feed to a local merchant application. The virtual qualification launcher accepts an optional private JSON array in `$DAKIM_GATEWAY_STATE/subscriptions.json`. The default state directory is `~/.local/share/dakim-gateway`. Keep the file owner-readable/writable only (0600). Each entry contains:

| Field | Meaning |
| --- | --- |
| `id` | Unique subscription ID bound on first use to this merchant, processor account and endpoint. |
| `merchant`, `account` | An enrolled program, for example `mimi` and `acct-mimi`. The account receives its events across its configured outlets. |
| `url` | An explicit local HTTP endpoint such as `http://127.0.0.1:8580/orders/events`. The qualification service disallows redirects and outbound proxies. |
| `key_id`, `secret` | An identifier and unique signing secret. Generate a dedicated random secret of at least 32 bytes using a cryptographically secure generator. Never reuse merchant, device or processor credentials. |

Start the merchant receiver, then run `scripts/gateway-local restart-gateway` on an existing installation. A new subscription starts at the beginning of the retained feed. Removing an entry and restarting pauses its delivery. Restoring the same subscription and its destination resumes its stored cursor. An endpoint or program change requires a new subscription ID. To rotate a key, install the new key under a new key ID at the receiver before updating the gateway. Retain the previous verification key through in-flight deliveries. No historical payload needs to be rewritten.

## Verify and acknowledge

Each POST contains `schema_version` (1), `event_id`, `merchant_id`, and the original `event` with its sequence, operation, processor account, outlet, resource version, status and monetary terms. The JSON field names and amount units match `GET /v2/events`. The receiver selects the configured key using the subscription and key ID and validates its configured merchant and account.

| Header | Value |
| --- | --- |
| `Dakim-Webhook-Version` | `1` |
| `Dakim-Webhook-Subscription` | Subscription ID |
| `Dakim-Webhook-Key-Id` | Signing key identifier |
| `Dakim-Webhook-Timestamp` | Decimal Unix time in seconds for this delivery attempt |
| `Dakim-Webhook-Signature` | `v1=` followed by the lowercase hexadecimal HMAC-SHA256 |

The HMAC input is the UTF-8 prefix below followed immediately by the exact request body bytes. Each `\n` denotes one LF byte.

```text
dakim.webhook.v1\n<Timestamp>\n<Key-Id>\n<Subscription>\n
```

Use the secret's literal UTF-8 bytes as the key. Verify the MAC in constant time before parsing the financial fields, reject unexpected versions or keys, and enforce a clock-skew window (for example five minutes). Deduplicate the stable event ID at the merchant. Commit that ID and the merchant-side effect or durable follow-up together. A duplicate must still return its original acknowledgement. A subsequent operation version may arrive while a previous request is being retried. Record versions and retrieve the authoritative original resource for payment or fulfilment decisions.

Return HTTP 200 or 202 with exactly one JSON field, `{"event_id":"<original event ID>"}`. Acknowledgements support JSON whitespace and a maximum response size of 2 KiB. A timeout, redirect, other status, wrong ID or malformed acknowledgment retains the pending event. The gateway retries with a capped exponential delay and jitter. It preserves the same event ID and body across service restarts. At-least-once delivery includes the case where the receiver committed but its response was lost.

With a merchant credential, query `GET /v2/webhook-subscriptions/{id}` for the scanned feed cursor, pending event, retry count and time, last failure class, and last acknowledged event. The cursor includes skipped records belonging to another processor account within the same merchant stream. A stuck endpoint holds its own next event in sequence; the merchant can still recover using the event feed or the original payment ID. Webhook delivery cannot initiate payments or alter the ledger.
