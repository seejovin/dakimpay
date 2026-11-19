# Returning a finance classification for correction

A finance reviewer can return a pending explanation for a previously matched provider refund. Mimi keeps the first explanation, the reviewer’s reason for returning it, and every corrected version together. An approval identifies the exact version reviewed. A case can have one effective approval.

## Staff workflow

1. In **Reports → Provider refunds after sale**, open **Review proposal** and check the receipt, original deposit, later bank debit and supporting reference.
2. Select **Return for correction** and enter the reason. Include a reviewer evidence reference if available. The version now shows **Returned for correction** alongside the original proposer, reason, evidence and time. Its proposer cannot return or approve their own submission.
3. Open **Correct proposal**. Amend the explanation or supporting reference and submit. Mimi gives the correction a new proposal ID and links it to the returned version. The reviewer can return it again if another correction is needed.
4. A different authorized staff member reviews the corrected version and approves it. **View classification** shows the version history and the identity and time of each decision. An older browser tab can act only on the version it originally displayed. An already approved classification is final in this workflow.

The existing finance, supervisor and owner roles apply. All amounts and financial statuses come from the original receipt and settlement records. Returning or approving a classification produces a reporting record at Mimi. There is no payment instruction or journal entry.

## Verified synthetic case

| Event | Evidence |
|---|---|
| Original sale and refund | A $3.60 receipt matched a $3.53 bank deposit after fees. The acquirer subsequently refunded $1.20. Mimi booked the later debit once. |
| First proposal returned | The first explanation and evidence reference were retained with the independent reviewer’s return reason and reference. A changed reason on a replay was declined. |
| Correction submitted | Two concurrent submissions produced one successor to the returned proposal. The corrected evidence, new ID and predecessor ID are recorded together. |
| Corrected proposal approved | Six concurrent approval requests produced one effective approval. Self-approval and approval of the returned version were refused. The final report retained both versions through replay and restart. |
| Financial records compared | Row counts and SHA-256 sums matched across 26 financial tables and views in Mimi, dakim pay, the simulated acquirer and the simulated bank. The single refund, both bank entries, receipt, inventory and journals are included. |

A database concurrency test holds the first reviewer’s decision open until PostgreSQL confirms a competing writer is blocked. The test runs both commit orders. In each order the first decision commits and the opposite write rolls back. It also exercises a third version and two independent adjustments in the same bank debit.

An upgrade exercise restored two snapshots made by the previous release, one with a pending proposal and one with an approved proposal. After applying the migration twice, the original ID, evidence and disposition remained available as version one. The test compared 19 historical tables and views per snapshot, including both financial data and staff audit records. [Upgrade results](finance-corrections-upgrade.json)

Chrome exercised the staff workflow through return, correction, approval and lookup by the original proposal ID. A stale browser tab attempted to approve the returned version and received HTTP 409. The focused PostgreSQL race test passed three times. The broader merchant and settlement suites also passed, including the ordinary card return and receiptless reversal.

## Reproduce

The targets below use disposable local services with simulated money. Run from the dakim pay source with the updated Mimi source in the adjacent directory.

```sh
MIMI_SOURCE=../mimi ./scripts/verify-mimi --finance-only
MIMI_SOURCE=../mimi ./scripts/verify-mimi --settlement-regression
```

Set `DAKIM_SHIFT_EVIDENCE_FILE` to a new file path to save a run. Set `DAKIM_CLASSIFICATION_BROWSER=1` with the local Playwright and browser paths for browser checks of the review states. A full cashier-to-finance environment is needed for the cross-app exercise. The corrections patch applies after the base and reporting patches listed in the README.

[Recorded return and approval](finance-corrections-run.json) · [Cross-app exercise](../scripts/mimi_late_refund_check.py) · [Merchant changes and tests](../integrations/mimi-finance-corrections.patch)
