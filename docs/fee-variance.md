# Comparing processor fees with Mimi’s deposit

In **Deposits → Processing fee comparisons**, choose the settlement request and select **Compare fees**. A request that failed the normal fee check is listed under its original operation ID even when dakim pay has not booked a payout. The report shows bank posting, local import status and fee differences separately.

## A deposit that balances with two incorrect fees

The PostgreSQL regression created two terminal sales with the normal local capture flow. It varied only their unsettled synthetic statement fees before the processor froze the payout. The original processor estimates were already recorded.

| Sale | Estimate at capture | Fee on frozen statement | Statement minus estimate |
|---|---:|---:|---:|
| $10.00 | $0.20 | $0.25 | +$0.05 |
| $20.00 | $0.40 | $0.35 | −$0.05 |
| **Total** | **$0.60** | **$0.60** | **$0.00** |

The bank independently posted **$29.40**, matching the net on the statement. Finance saw **two fee differences**, a matched bank posting and a settlement still awaiting local import. The normal settlement path continued to reject the changed fee lines. An earlier deposit of $0.98 retained its original matched statement. [Operator screenshot](fee-variance.png) · [Three-service run, item references and before/after checks](fee-variance-run.json)

## Reading the evidence

- **Estimate.** At capture the processor records the original payment ID, charge reference, gross amount, currency, estimated fee, rate in basis points, pricing policy and timestamp in the same transaction. The report reads that observation. Older captures without an estimate show *Unavailable*; their actual statement fees remain visible. When any estimate is missing, the expected total and aggregate difference are unavailable.
- **Actual fee.** The report uses the identified fee line from the processor’s frozen payout. Each item links the original Mimi order and payment, processor charge, and statement line. Fee costs appear as positive amounts. Positive variance means the processor charged more than it estimated. Aggregate variance can equal zero while individual fees differ.
- **Bank cash.** A separate lookup checks the posted bank trace, merchant, currency and signed amount against the frozen remittance. A pending posting, inconsistent evidence and an unavailable bank each have distinct states. A successful bank lookup does not authorize a ledger entry.
- **Local booking.** For an imported payout the report also compares the frozen statement with the retained local payout and lines. For an unbooked payout it checks original movements and the minimum expected contents. Duplicate lines, reused statement line identities and evidence referring to another operation are rejected.

The report fetches evidence with GET and reads the local ledger in a single consistent snapshot. Reading or retrying a report has no accounting effect. Finance can use the references to request corrected itemization from the processor. Fee dispute handling and corrective accounting require separate workflows.

## Verification

The backend regression used separate disposable payment, processor and bank databases. It checked missing historical quotes, changed bank amounts, bank unavailability, duplicate fields, foreign line references, replay, and two payment-service restarts. Fingerprints of all rows in 20 checked tables stayed identical across report reads and restarts. The processor test separately checked rounding, estimate immutability and original frozen batch membership. Both tests passed three times with race detection. A refund-only payout reported zero fee lines and its signed bank debit. The existing Mimi register-to-finance settlement regression also passed.

In a separate browser run, the rejected import appeared in Deposits without a manual refresh. Opening the report twice sent two GET requests. Row counts and SHA-256 fingerprints across 14 financial tables were identical before and after those reads.

From dakim pay, set `DAKIM_TEST_DATABASE_URL` to an administrative connection on a disposable PostgreSQL cluster, then run the following commands.

```sh
go test -race -count=3 -run 'ItemizedFeeVariance|FrozenPayoutFeeEvidence' ./internal/pay ./internal/simulator
MIMI_SOURCE=../mimi ./scripts/verify-mimi --settlement-regression
```

The comparison currently covers the simulator’s USD per-capture processing fee. A live partner would need to supply original fee estimates, stable item identifiers and itemized statements. Separate scheme, tax, tiered or batch-level charges would need their own mapping. [Finance regression](../internal/pay/fee_variance_test.go) · [Processor regression](../internal/simulator/fee_evidence_test.go)
