# Payment recovery checkpoint

19 November 2026 · Local simulation · Mimi grocery, USD

Mimi issues a receipt after it verifies the original capture. Finance closes the settlement after it matches the itemized acquirer statement to a separately posted bank entry. This checkpoint tests both decisions while the terminal, the payment service or the message delivery worker loses a reply.

## Financial evidence

| Exercise | Observed result in disposable installations |
|---|---|
| Terminal and online merchant journey | The terminal lost its reply after a $42.00 capture. Recovery found the original. The buyer approved a separate $68.00 hosted checkout. The $10.00 online refund and $2.54 in fees left a $97.46 bank deposit. Replaying the journey preserved its original transactions. All six synthetic brand and channel combinations captured. |
| Mimi register, return and finance close | Eight concurrent requests with the same tender key produced one payment operation. Mimi receipted $7.20 and a second operator approved the $3.60 return. Finance matched $3.46 in the bank after the $0.14 fee. Every exported journal balanced by source, reference and currency. The importer could not approve their own close. |
| Capture reply and notification acknowledgement both lost | After service restart the sender replayed the same event. The receiver verified a single $12.34 capture. Two original-card refunds of $4.34 and $8.00 exhausted the capture. Another cent was refused. The partner statement explained a $0.25 **debit** for the retained fee. The bank had to post it before the merchant could export a matched statement. |
| Authorization before capture | A processor fixture held a $42.00 online authorization. The checkout link closed and concurrent refund and settlement attempts moved zero cents. The service kept the same reference through a missing lookup, recovered the later capture and ignored a late authorization. One submission produced one capture and $40.95 in transit after the simulated online fee. |
| Duplicate, stale and conflicting notifications | Concurrent deliveries created one inbox record. A changed body for the same event ID returned a conflict and preserved the original. A stale provider snapshot could not undo a capture. A refund reporting another parent or amount went to review. Nine invalid delivery acknowledgements left the same event queued until the sender received a matching receipt. |
| Interrupted Mimi worker | Concurrent recovery, an expired worker lease and a late response still produced one receipt. Missing, null, negative or excessive cumulative refund evidence held the sale for review. A provider reference is required for a successful capture or refund. |

## Delivery and operator recovery

The simulated acquirer writes the financial change and notification in one database transaction. The sender signs the original bytes and a fresh Unix timestamp with HMAC-SHA256 under its dedicated key. The receiver allows five minutes of clock skew, authenticates the merchant binding and commits a receipt keyed by source and event ID. The sender requires HTTP 200 or 202 with a JSON object containing the matching event ID. Acknowledgement records successful delivery. A worker then retrieves the original payment or refund and checks its financial terms. The two operations have separate audit records. [Wire contract](../internal/processor/event.go) · [Delivery](../internal/simulator/callbacks.go) · [Verification](../internal/pay/callbacks.go)

1. When the till loses its reply, keep the order and tender open. In **Payments**, open the original order and select **Recover payment** or **Check payment**. The worker also resumes on restart. Keep the goods on hold while the outcome is unknown or the processor reports an authorization awaiting capture.
2. Open **Activity → Processor notifications**. Use the event ID and original operation ID to find the specific verification, conflict or outstanding delivery. A missing notification can be recovered by polling the original operation. Keep mismatched evidence open for investigation with the acquirer. Supply the merchant, operation, acquirer reference, amount, currency and event ID.
3. In **Deposits**, match the gross lines, refunds and fees to the payout reference. Keep it in transit until the bank supplies the posting. For the training exercise use **Post bank entry**, then **Reconcile deposits**. In Mimi, finance imports the matched statement and a different authorized user closes it. Preserve the original receipt, refund, payout and bank trace for review.

## Reproduce

From the dakim pay checkout, run the following with the Mimi checkout next to it. Both scripts start and stop their own temporary services.

```sh
./scripts/verify
MIMI_SOURCE=../mimi ./scripts/verify-mimi
```

For the PostgreSQL race suite, set `DAKIM_TEST_DATABASE_URL` to an administrative connection on a **disposable** cluster, then run `go test -race -count=1 ./...`. Without that connection the database tests skip. [Combined failure-injection test](../internal/pay/callback_roundtrip_test.go) · [Mimi integration and journal checks](../scripts/verify-mimi)

## Release status

**Verified locally.** The service, register, database transactions, signed delivery, refund limits and independent bank-record comparison ran against synthetic instruments. The local console holds buyer and bank-training credentials for one operator. Mimi exercised named, separate users for return approval and financial close.

**Real-partner evidence.** None. The local acquirer uses automatic capture. Separate partner authorization, capture, expiry, void, dispute and funding semantics need qualification for the chosen acquirer. A separate database can exercise a bank discrepancy, but a contracted bank statement is required to prove an actual deposit.

**Next local milestone.** Give Mimi an itemized, read-only view of original refunds and reversals with pending or disputed status. A full reversal before a retail receipt currently leaves a held sale. Test that operations can resolve it with independent goods-custody and finance evidence.

The subsequent [receiptless refund checkpoint](receiptless-refund-checkpoint.md) implements and tests this handoff and sets out the remaining limits.

**Before live use.** Obtain the merchant agreement and brand/channel approvals, confirm the legal and PCI responsibilities, and commission the exact reader and hosted checkout. Separate bank and payment-operator identities, verify key rotation and recovery from backup across all companies, rehearse disputes and bank returns, and reconcile an authorized pilot to Mimi’s own bank records. [Brand acceptance guide](network-acceptance.md) · [Owners and release gates](../DESIGN.md)
