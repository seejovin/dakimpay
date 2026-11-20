# Merchant acceptance and route enrollment

The gateway evaluates a new charge against the authenticated merchant, its gateway account, the transaction currency, requested method, presentment channel and amount. The lowest eligible positive priority wins. Bounds include both endpoints. Route selection, order reservation and immutable instruction creation use the gateway admission transaction. Omitting the payment method requests a card. Unknown or unavailable requested methods are rejected before dispatch.

| Configuration field | Contract |
| --- | --- |
| `id`, `merchant`, `account` | Stable route identifier inside one gateway merchant/account. Supply a distinct account for each merchant program. |
| `connector`, `provider_account` | Stable connector identity and downstream account. One connector/account pair has one merchant program and route identity throughout the catalog. Multiple accepted card brands can share that route. A processor client must implement authenticated lookup, persistent ledger identity and release inquiry. |
| `methods`, `channels` | Explicit lists. The initial method IDs are `card`, `visa`, `mastercard`, `amex`, `alipay` and `wechat_pay`. Card routes may advertise `terminal` and/or `hosted`. The current wallet presentation uses `hosted`. A card network name in the request records the requested brand. The processor supplies the observed brand independently. |
| `currency`, `min_amount_minor`, `max_amount_minor` | Exact currency and a positive amount range in its minor units. Merchant-level authorization applies in addition to these limits. |
| `priority`, `enabled` | Lower numbers win among enabled eligible routes. Overlapping equal-priority rules are rejected at startup. Disabling a route stops new sale transmission. |

For example, a primary card acquirer may have priority 1 and a separately enrolled card acquirer priority 2. Removing eligibility of the first makes the second available **only for a newly admitted payment**. Timeouts, malformed responses, contradictory statuses and lost connections cannot choose another processor for any recorded charge.

The committed original holds the method, route, connector and provider account. Refunds inherit all four terms. Looking up the original or returning money continues after new sales have been suspended. Keep the original connector, merchant enrollment and credentials available for historic queries; deleting or repointing that enrollment holds existing instructions. A record from the earlier single-acquirer release has a distinct unbound format. An explicitly configured historical connector may query it and return its original funds; fresh charge transmission through the unbound path is held. A partially bound record is never sent to that connector.

The amount-aware method list displays what is available to the authenticated merchant on that channel. At an unspecified amount it shows methods that have at least one authorized positive amount. It does not query account balances, partner uptime, or the issuing bank. Availability, price and approval rate telemetry can inform future priority changes after qualification.

## Drain one processor route

Operators use `/v2/program/routes/{route_id}/sales` on the gateway. The route must belong to the authenticated merchant and program account. Each route has a durable state and its own audit sequence. Operations apply to both hosted and terminal sales on the specified route.

| Operation | Method and suffix | Authority and input |
| --- | --- | --- |
| Inspect | GET the base path | Merchant, finance or reviewer |
| Audit | GET `/audit?after=0` | Finance or reviewer |
| Freeze | POST `/freeze` | Finance or reviewer supplies `{"reason":"original processor inquiries timing out"}` |
| Propose resumption | POST `/resume-proposals` | Finance supplies the current `suspension_version` and a reason |
| Approve resumption | POST `/resume-approvals` | A different reviewer supplies the returned `proposal_id`, `proposal_digest` and exact `suspension_version` |

Each POST requires an actor-specific idempotency key. A new freeze invalidates an outstanding resume proposal. A proposal expires after 15 minutes. Operators can resume one route while leaving another suspended.

A new payment selects the highest-priority eligible, open route in the same transaction that checks the order and reserves its value. If a route is drained during its independent identity inquiry, admission checks availability again and requires fresh identity evidence for the replacement. If every eligible route is drained, the gateway rejects the new operation. Amount-specific method discovery also checks the current controls.

Once admitted, an original payment and its refunds keep their processor ledger, connector and account. Draining stops new egress even for a queued sale on that route. Original inquiries and authorized refunds continue. A captured payment can therefore be recovered and refunded during an incident. A reviewer cannot forcibly change the original to a new provider. A valid suspension can occur after the one-time dispatch authorization and before the network receives it; inquiries resolve any resulting operation against that original.

The dispatcher assigns separate durable work queues to connector identities. A slow response from one provider can occupy its own execution task while independent processors continue. The database and gateway host remain shared components. The gateway continues to drain the previous version’s global queue after an upgrade.
