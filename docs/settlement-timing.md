# Which settlements are still outstanding?

Open **Deposits → Open timing queue**. Select **Record current observation** to save the processor’s frozen payouts, the bank’s independently posted entries, and any verified local booking. The queue opens from retained observations and displays its UTC cutoffs. [Replay a shift as finance knew it](settlement-knowledge.md) Open items are ordered by age. Expanding a row shows its bank trace, payment currency, signed amount and original timestamps, plus the processor payout and settlement operation when present.

| What finance sees | What the evidence establishes | What to check |
|---|---|---|
| Payout in transit | The processor issued a payout. Its bank trace has no posting through the cutoff. | Look up the trace on the bank statement and check the funding date. |
| Bank posting only | The bank posted a credit or debit. The processor supplied no payout with that trace. | Ask the processor for the remittance that owns the bank trace. An equal amount on another trace cannot clear it. |
| Amounts differ | Both sources report the same merchant, currency and trace, with different signed amounts. | Compare the remittance and bank entry. The item stays open. |
| Source matched | Both sources report the same merchant, currency, bank trace and signed amount. | Check the separate local ledger match timestamp to see whether dakim pay has booked it. |

For open items, age runs from the earliest available source event to the cutoff. For source matches, the displayed duration is the interval between the two source events, even if the bank posted first. A pending bank instruction supplies no posting timestamp. The local match timestamp comes from dakim pay’s original booking event and retained bank proof. Reading the queue cannot record a match or initiate a payment.

## Fixed-clock example

The following are synthetic observations from 7 October 2026. Times are UTC.

| Trace and signed cash | At 12:00 | Later evidence |
|---|---|---|
| Processor payout, $29.40 | Open for 2h 30m since 09:30. | The bank posted at 12:15. The item left the open queue and showed 2h 45m between source events. |
| Bank debit, −$3.00 | Open for 1h 50m since 10:10. | The processor issued a payout on the same trace at 12:30. The source match took 2h 20m. |
| −$3.00 processor debit, −$2.99 bank posting | The one-cent difference stayed open. | The two unrelated matches did not clear this difference. |
| Previously booked $0.98 payout | The sources had matched at 08:05. Local booking was recorded at 08:06. | A replay at 08:05 showed a source match with no local booking yet. |

The test added only the later observations to their respective sources. It compared existing records before and after those additions, then compared all 20 checked payment, processor and bank tables across report reads and a restart. It also replayed the noon report after both later events had arrived. [Three-database regression](../internal/pay/settlement_timing_test.go)

A separate three-service browser run followed a $42.00 payout through a 19:15 UTC bank posting. Its open age of 9h 30m at 18:00 became a completed interval of 10h 45m. The −$12.50 bank-only item stayed open. Two browser reads sent no write requests, and the fingerprints of 14 financial tables stayed identical across each read. [Operator screenshot](settlement-timing-ui.png) · [Recorded before and after reports](settlement-timing-run.json)

## Live source-time API

`GET /v1/settlement-timing` uses the current time. To reproduce a shift cutoff, use an explicit timestamp such as `GET /v1/settlement-timing?as_of=2026-10-07T12:00:00Z`. The cutoff is inclusive and normalized to UTC at PostgreSQL’s microsecond precision. The response contains the open items and source matches at that instant.

Each source reads all eligible Mimi USD records in its own database snapshot. The service compares the two completed inventories at the same cutoff. If either source is unavailable, incomplete, contradictory about its identity, or exceeds the connector’s 4 MiB response limit, the queue is unavailable. Finance can retry it. An unavailable inventory cannot establish that a payout or posting is missing.

A fresh source-time comparison can change when a late record carries an older timestamp. To reproduce what finance knew, use the retained-history API with the original effective and knowledge cutoffs. Coverage starts with the first explicit observation. [Settlement history and late-delivery evidence](settlement-knowledge.md)

## Reproduce locally

With `DAKIM_TEST_DATABASE_URL` set to an administrative connection on a disposable PostgreSQL cluster, run the following from dakim pay. The tests create isolated databases.

```sh
go test -race -count=3 -run SettlementTiming ./internal/pay ./internal/simulator
MIMI_SOURCE=../mimi ./scripts/verify-mimi --settlement-regression
```

The timing tests passed three times with race detection, including source failure, zero-value cash, and refusal to return partial inventories. The Mimi register-to-finance settlement regression passed. [Source inventory and size-limit tests](../internal/simulator/settlement_timing_test.go)
