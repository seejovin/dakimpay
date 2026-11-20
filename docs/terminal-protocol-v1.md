# Virtual terminal protocol v1

The reader displays a charge already assigned to its lane and records a simulated customer decision. The independent virtual acquirer owns the presentation and its outcome. The merchant gateway obtains monetary evidence by querying that acquirer using the original operation ID. A reader receipt alone cannot release goods or book a capture.

## Start a presentation

`scripts/gateway-local start` provisions the gateway on `http://127.0.0.1:8470` and the acquirer on `http://127.0.0.1:8471` by default. `scripts/gateway-local open` opens the merchant and reader interfaces. The reader page is `/terminal` on the acquirer. Local fixture credentials are in the private `clients.json` in the installation state directory. The `device_token` belongs to one reader with a fixed merchant, processor account and outlet. The merchant uses its separate `tokens.merchant` on the gateway.

Submit `POST /v2/payment-intents` on the gateway with `Authorization: Bearer <merchant token>`, `Content-Type: application/json` and a stable `Idempotency-Key`. A single-tender example is shown below.

```json
{"order_id":"order-271","allocation_id":"tender-1","outlet_id":"store-1","terminal_id":"terminal-1","channel":"terminal","amount_minor":1234,"currency":"USD"}
```

`amount_minor` is a positive integer. The merchant's program fixes the currency and maximum amount. The optional `order_total_minor` defaults to `amount_minor`, and `quote_version` defaults to `"1"`. `replaces_id` identifies a permitted replacement after an authoritative decline. The response contains the gateway operation ID and a `Location` header. A new instruction returns 201. A matching repeat returns 200 with the original resource. A lane already reserved by another instruction returns 409 `terminal_busy`. On a timeout, reuse the original request and key, or retrieve the known original ID.

## Device requests

Send `Authorization: Bearer <device_token>` to the acquirer. Authenticate every poll and decision. A device cannot select its merchant, outlet or reader using request fields. The enrollment maps its credential to all three and to the processor account. The browser polls approximately every 1.4 seconds and holds the credential in memory.

| Method and route | Request | Successful response |
| --- | --- | --- |
| `GET /device/v1/command` | Empty body | 200 with `version`, `terminal_id`, `merchant_id`, `outlet_id` and `command`. An idle lane returns `command: null`. |
| `POST /device/v1/commands/{operation_id}/decision` | `Content-Type: application/json` and exactly `{"decision":"approve"}` or `{"decision":"decline"}` | 200 with the final processor fact for that original charge. A capture includes its stable movement `reference`. |

A pending poll can return the following body. The wrapper and `command` repeat the enrolled identifiers so the reader can verify the displayed program and lane. `kind` is `charge`, `channel` is `terminal` and `state` is `pending`. Each poll identifies at most one presentable charge. `parent_id` and `checkout_url` are absent in this device flow.

```json
{
  "version": 1,
  "terminal_id": "terminal-1",
  "merchant_id": "mimi",
  "outlet_id": "store-1",
  "command": {
    "operation_id": "pi_example", "merchant_id": "mimi", "processor_account": "acct-mimi",
    "kind": "charge", "order_id": "order-271", "outlet_id": "store-1", "terminal_id": "terminal-1",
    "channel": "terminal", "amount_minor": 1234, "currency": "USD", "state": "pending"
  }
}
```

Display the amount, currency and order before a decision. Submit the displayed operation ID. The final fact echoes its immutable economic fields. Approval produces `state: "captured"` and a nonempty `reference`. Decline produces `state: "declined"`. The device API carries no card number, card cryptogram, track data or PIN.

## Lost responses, expiry and conflicting decisions

| Observation | Meaning and recovery |
| --- | --- |
| 200 matching prior decision | The original decision was already recorded. Repeating it returns the original outcome and movement reference. |
| 400 or 422 | Malformed JSON, an unknown field, or an unsupported decision. Correct the request for the displayed original. |
| 401 | The credential cannot authenticate this reader. Stop presentation and restore the correct enrollment. |
| 404 | The original is absent from this enrolled reader's scope. A second reader cannot take it over. |
| 409 | A different final outcome already won, including a late approval after expiry. The merchant queries the original. |
| Network loss, timeout or 503 | The outcome is unknown to the caller. Preserve the operation ID and decision. A matching retry is safe. An idle later poll alone does not determine the original result. |

The processor uses its database clock and a row lock to arbitrate the deadline, decision and concurrent repeats. The virtual deadline is 15 minutes from command insertion. Expiry is applied on observation or before another submission takes the lane. Gateway restart, device disconnect and a lost HTTP response do not cancel or refund an accepted charge. The merchant can retrieve `GET /v2/payment-intents/{id}` on the gateway. For a fulfilment decision, read `GET /v2/payment-intents/{id}/release-evidence` using the merchant credential and check the returned eligibility and observation times. Eligibility describes that observation and can change with a later refund or dispute.

## Physical reader integration boundary

The stable handoff is the gateway's immutable processor command, scoped original-operation inquiry and evidence-backed result. A hardware integration needs a separately qualified processor adapter and enrollment system. The simulator's `approve` message is a synthetic decision. It must never authorize a real-card capture. A physical integration obtains the financial outcome and opaque reference from the contracted acquirer. Preserve the same original ID across process, communications and device recovery.

| Qualification work outstanding | Evidence to obtain before live card use |
| --- | --- |
| Choose the terminal, kernel, schemes, markets and acquiring partner | Document the exact hardware, firmware and kernel combination, its EMV Level 1 and Level 2 approvals, and the acquirer's applicable Level 3 end-to-end test results. [EMVCo terminal integration testing](https://www.emvco.com/news-publications/what-is-level-3-terminal-integration-testing/) |
| Establish the card-data and PIN boundary | Verify the specific reader's applicable approval, key-injection and encryption arrangements, and the deployment's assessed PCI scope. Device approval alone does not establish that a deployment uses a validated P2PE solution. [PCI SSC PTS POI](https://www.pcisecuritystandards.org/standards/pts-point-of-interaction-poi/) · [PCI SSC terminal encryption guidance](https://www.pcisecuritystandards.org/faqs/1301/) |
| Provision and manage a real fleet | Implement device identity, hardware-backed keys, authenticated transport, controlled enrollment, rotation and revocation, inventory, tamper response and signed firmware updates. Demonstrate recovery after reader replacement and device compromise under the partner's rules. |
| Reconcile physical outcomes | Exercise approved, declined, timeout, cancellation, reversal, duplicate, power-loss and recovered transactions using the partner's certification environment. Match terminal receipts, authoritative original-operation records, remittance and bank evidence. Agree how offline, fallback and partial approval states behave before offering them. |

Run `scripts/verify-gateway` for the current virtual qualification. It drives independent merchant and reader HTTP surfaces, stops the gateway during a capture, restarts it and verifies the original financial movement. `scripts/with-spanner-emulator scripts/verify-gateway` exercises the same contract with the native Spanner gateway adapter and an independent PostgreSQL acquirer.
