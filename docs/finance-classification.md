# Finance review of a matched refund debit

Mimi’s finance team can record why a provider returned money after a completed sale. This first reporting workflow covers a **merchant-funded service concession**. The simulated example begins with a $3.60 retail receipt, a $3.53 deposit after fees and a later $1.20 bank debit. Finance approval adds the classification to Mimi’s case. The existing debit stays booked to bank −$1.20 and provider refund suspense +$1.20. [Synthetic run](finance-classification-run.json)

## Using the report

1. Open **Reports → Provider refunds after sale**. Use **Find statement or refund** for a case outside the recent list. Open the case to compare the original receipt, refund and both bank references.
2. A member of the existing finance review team selects **Propose classification**, enters the business reason and cites the supporting evidence by reference. This version records the reference. The reviewer must consult the underlying business evidence.
3. A different authorized staff member opens **Review proposal**, checks the amount and evidence, and selects **Approve classification**. The reviewer sees the same reason and case evidence the proposer submitted. The approved report retains the category, reason, evidence reference, proposer, reviewer and their timestamps. The bank reconciliation status remains visible alongside the reporting decision.

Eligibility requires an original receipted capture with a matched positive deposit, an independently verified and closed later negative bank statement, and a matching refund allocation. A bank reference typed by a staff member cannot establish those facts. A missing or pending bank entry, a mismatched case, a different proposal, self-approval, or a negative line inside a positive payout cannot produce this approval.

## Reporting and replay

The classification runs in Mimi. It has no calls to the payment provider, acquirer or bank. The database binds one immutable proposal to each matched provider adjustment. Approval binds one independent reviewer to that exact proposal. Identical requests return the original decision. Mimi distinguishes a fresh request key that retrieves a prior decision from the original successful approval in the general command audit.

Finance can report the approved business reason immediately. The recorded approval has no accounting amount of its own. Sales, taxes, tender balances, refund limits, inventory, clearing and suspense all continue to derive from their original financial records. A reviewer can now return a pending proposal and review a replacement with its own evidence. The historical run below exercised direct approval. [Correction workflow](finance-corrections.md)

## Verification

From the dakim pay checkout with the local Mimi source next to it, run either the isolated exercise or the broader settlement regression.

```sh
MIMI_SOURCE=../mimi ./scripts/verify-mimi --finance-only
MIMI_SOURCE=../mimi ./scripts/verify-mimi --settlement-regression
```

Set `DAKIM_SHIFT_EVIDENCE_FILE` to a new file path to retain the amounts in cents, distinct bank references, classification identity, reviewer identities, counts and before/after hashes. Both commands use temporary loopback services, synthetic staff and simulated money. For browser inspection, the same run accepts `DAKIM_CLASSIFICATION_BROWSER=1` with a local Playwright installation and Chromium executable.

| Check | Observed outcome |
|---|---|
| Before independent bank posting | Mimi refused to classify the debit. |
| Self-approval or wrong proposal | Self-approval returned HTTP 403. A mismatched proposal returned HTTP 409. |
| Concurrent and restarted approval | Six concurrent requests and two restarts during classification produced one approved classification and one successful approval audit entry. Two different authorized reviewers also raced to approve one proposal. One succeeded and the other received a conflict. The approved record remained retrievable by its original reference. |
| Financial effects | Row counts and SHA-256 digests of 26 financial tables and views were identical across Mimi, dakim pay, the acquirer and the bank before proposing and after final replay. |
| Existing store | A restored store already contained the $3.53 deposit and $1.20 debit from the pre-classification release. Applying the reporting migration twice preserved all 15 historical financial fingerprints. The case became eligible for review with zero approvals created by the upgrade. |

The browser exercise used separate signed-in staff sessions to enter the proposal, approve it and find the approved case after reloading. The broader settlement regression also completed the ordinary register return and the previously verified receiptless reversal. The finance patch passed the full PostgreSQL-backed merchant race suite and six-lane workload against the prior adapter release.

[Cross-app exercise](../scripts/mimi_late_refund_check.py) · [Mimi reporting patch](../integrations/mimi-finance-reporting.patch) · [Observed case and financial fingerprints](finance-classification-run.json) · [Upgrade record](finance-classification-upgrade.json)

**Subsequent checkpoint.** Independent return and versioned correction are implemented in the [correction workflow](finance-corrections.md). Ledger reclassification requires separate accounting authority and journal rules.
