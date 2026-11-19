# Replaying what finance knew about a settlement

Open **Deposits → Open timing queue**. **Record current observation** checks the processor, bank and local booking evidence and saves a complete observation in the reporting archive. **View latest retained report** and **Replay** read that archive. Opening or replaying a report never contacts the processor or the bank.

Use the two cutoffs to ask different questions about the same store shift.

| Clock | What it means |
|---|---|
| Source effective cutoff | Include a payout, posting or booking if its original event timestamp is at or before this instant. Open-item ages run to this cutoff. |
| Knowledge cutoff | Include the evidence only if the reporting archive had accepted it by this instant. The server assigns first-observed time after checking a complete observation. |

The report displays both cutoffs, the last observation available by the knowledge cutoff, and the time through which the sources were checked. Each row shows the effective and first-observed times for each available source. Local booking has its own pair of timestamps. If the requested effective cutoff extends beyond the last check, finance sees the coverage gap.

## What changed when a late bank posting arrived

In a three-service browser run, Mimi’s $42.00 payout had a source time of **09:00 UTC on 18 November**. The first observation saw the payout alone. The bank subsequently delivered a $42.00 posting with an **08:30 UTC** source time on that same date.

| Effective cutoff | Knowledge cutoff | What finance saw |
|---|---|---|
| 18 November, 10:00 UTC | First observation | $42.00 still in transit, open for one hour. |
| 18 November, 10:00 UTC | Second observation | $42.00 source match, 30 minutes between source records. The bank posting’s first-observed time belonged to the second observation. |
| 18 November, 10:00 UTC | First observation, replayed after delivery | The original in-transit report, including the same amounts and timestamps. |

The browser recorded two observations. Fourteen financial tables retained their contents across reporting operations and no payout was booked. The evidence file contains both full responses and their original UTC cutoffs. [Recorded run](settlement-knowledge-run.json) · [Operator screenshot](settlement-knowledge-ui.png)

## Archival behavior

- Recording is explicit. Before the first complete observation, the service reports **history unavailable**. A successfully observed empty pair of inventories establishes an empty baseline. Opening the legacy source-time API does not create a baseline.
- A complete observation adds a coverage record and stores each new processor, bank or local-booking fact once. The complete observation and its facts commit together. Re-observing the same facts preserves their first-known times. A new successful check can extend source coverage even when no facts changed.
- A missing source, a changed or removed original, a stale cutoff from an overlapping capture, or a backward publication clock cannot replace earlier evidence. Historical reads work through a source outage using the retained observations.
- Knowledge time measures acceptance by this reporting archive. A person may have seen the bank portal earlier. Wall-time replay assumes a trustworthy local clock. The reporting tables are independent of payments, bookings, bank entries and the source inventories. Retain the reporting archive when backing up or restoring the application. Reconstructing it from current financial rows loses first-observation history.

## API and local verification

`POST /v1/settlement-timing/observations` records the current complete observation with server-assigned timestamps. It accepts no transaction details or caller-supplied time. The response includes the effective and knowledge cutoffs that reproduce it.

`GET /v1/settlement-timing/history?as_of=E&known_as_of=K` replays retained knowledge. Supply RFC 3339 times with a zone. The cutoffs are inclusive at microsecond precision and E must be at or before K. Omitting E uses K for both clocks. Without parameters the route shows what is retained through the current instant. Save both cutoffs to reproduce a specific result. The original `GET /v1/settlement-timing?as_of=E` remains available for a fresh comparison against the sources.

The fixed-clock integration test saw a $29.40 payout at noon. A later bank delivery carried a 09:40 source timestamp. The next full source check covered through 13:58 and became available to the report at 14:00. At 13:59:59.999999 the original payout still appeared open. At 14:00 the same effective cutoff included the bank posting. Repeated offline reads after a service restart returned the same original and revised responses byte for byte, and 20 pre-existing payment, processor and bank tables retained their fingerprints. Six concurrent redeliveries also preserved the original facts and their first-known times.

The timing regressions passed three times with race detection. Mimi’s register-to-finance regression also passed after its verifier was updated to parse fractional-second timestamps of all supported PostgreSQL lengths on Python 3.9. To reproduce them from dakim pay, set `DAKIM_TEST_DATABASE_URL` to an administrative connection on a disposable PostgreSQL cluster.

```sh
go test -race -count=3 -run 'SettlementTiming|TimingObservation' ./internal/pay ./internal/simulator
MIMI_SOURCE=../mimi ./scripts/verify-mimi --settlement-regression
```

[Late delivery and restart regression](../internal/pay/timing_knowledge_test.go) · [Atomic recording and concurrency regressions](../internal/pay/timing_observations_test.go)
