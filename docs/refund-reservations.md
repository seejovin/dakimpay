# How much can Mimi still refund?

For a captured payment, dakim pay adds all successful refunds and the active reservations for distinct pending merchant requests in the capture currency. A new request can spend only the remainder. The local integration uses USD integer cents. An acquirer inquiry that finds the original refund still unresolved keeps the money reserved. Only a definitive result bound to that exact operation, merchant, original charge, amount and currency can release its reservation or convert it to a successful refund.

The merchant payment detail and API show the confirmed amount (`refunded`), funded reservations (`refund_reserved`), unresolved requests at face value (`refund_pending_requested`), amount available (`refundable`) and whether the capture is under review (`refund_balance_review`). Existing idempotency keys return their original results, including a decline. Changing the amount on a used key returns a conflict.

## Concurrent cashiers and a lost reply

Each row is an observed state of the same $10.00 capture in the local database test.

| Event | Successful | Reserved | Available |
|---|---:|---:|---:|
| First refund succeeded. Separate $4.01 and $2.65 requests lost their replies. | $3.33 | $6.66 | $0.01 |
| Eight cashiers separately requested the final cent. One succeeded. | $3.34 | $6.66 | $0.00 |
| An inquiry confirmed that the $2.65 request was declined. | $3.34 | $4.01 | $2.65 |
| A new $2.65 refund succeeded. An inquiry then confirmed the original $4.01 success. | $10.00 | $0.00 | $0.00 |

The original two requests each reached the simulated acquirer once. Client replay, worker recovery and restart preserved their identities. A response with a different currency or an ambiguous repeated field kept the original reservation in place. A response timeout alone gave Mimi no new spending capacity.

A separate Chrome check used synthetic dashboard responses and the actual merchant screen. With $0.01 available, entering $0.02 showed the limit and sent zero requests. Entering $0.01 sent one request with an idempotency key. On a capture under review the screen displayed the pending face value, the funded reserve and zero available. The refund form was unavailable.

## When the acquirer reports a different refund

On another $10.00 capture, Mimi had already received the original $9.80 deposit. A merchant refund of $8.00 remained unresolved locally. The acquirer independently refunded $7.00 and the later remittance arrived before its notification. Posting $7.00 locally while continuing to reserve $8.00 would have accounted for $15.00 against one $10.00 sale.

dakim pay retains the provider observation, pauses new refunds, and withholds the adjustment from Mimi and the later payout from the local ledger. It keeps the original matched bank deposit. After an inquiry confirms the $8.00 refusal, the recovery worker verifies the held $7.00 by its original identifiers and publishes it once. The later debit can then reconcile to its own bank posting. If the source stays unavailable or contradictory, the capture stays under review. The operator checks the original operations and the acquirer’s itemized report before resolving the exception.

## Upgrading previously recorded cases

An older installation could already have published overlapping figures. The migration keeps its posted refunds, original requests and settlement history and records the discrepancy. In a restored fixture with $7.00 already posted and $10.00 nominally pending, the remaining $3.00 was held at the capture level. The merchant could request $0.00. After an independently confirmed $8.00 refusal, the $2.00 sibling stayed reserved and exactly $1.00 became available. All ten pre-existing payment tables matched their original fingerprints across two startup migrations. [Restored fixture evidence](refund-balance-upgrade-run.json)

## Verification and scope

These are local simulations using disposable PostgreSQL databases. Set `DAKIM_TEST_DATABASE_URL` to an administrative connection on a disposable cluster and run the following commands from dakim pay. The focused refund and provider tests passed three consecutive runs with race detection. The separate Mimi settlement regression also verified balancing, bank matching and replay across both applications.

```sh
go test -race -count=3 -run 'Refund|Reservation|Source' ./internal/pay ./internal/simulator
MIMI_SOURCE=../mimi ./scripts/verify-mimi --settlement-regression
```

A partner pilot still needs to establish which responses are definitive, the lifespan of its idempotency keys, its refund lookup and remittance identifiers, and the treatment of late reversals and disputes. The current reservation calculation covers refunds. Dispute exposure and bank-imposed reserves have their own financial treatment. [Merchant reservation tests](../internal/pay/refund_reservation_test.go) · [External refund and recovery tests](../internal/pay/adjustments_test.go) · [Upgrade test](../internal/pay/refund_balance_upgrade_test.go)
