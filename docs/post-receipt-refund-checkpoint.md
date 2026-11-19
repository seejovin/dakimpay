# An acquirer refund after Mimi's bank deposit

19 November 2026 · Local synthetic USD merchant, acquirer and bank

Mimi sold a carton of eggs for $3.60. Its original bank deposit was $3.53 after the seven-cent fee. Later, the acquirer returned $1.20 to the customer. Mimi retained the original receipt and settled deposit, opened one finance exception, and matched the new refund to a separate $1.20 bank debit. [Recorded run with both bank references](post-receipt-refund-run.json)

| Stage | Authoritative evidence | Accounting and operational result |
|---|---|---|
| Finish the sale | Mimi's receipt and original card payment, the acquirer's capture and fee, and the first independently posted bank trace. | The original settlement journal records bank +353, processing fees +7 and card clearing −360, all in cents. |
| Observe the refund | A signed message identifies a source operation. dakim pay retrieves the original acquirer refund and binds it to the merchant, currency and capture. Mimi checks the original payout, bank trace and deposited net against its closed settlement. A later remittance can also trigger a lookup by refund reference if the notification is missing. | One immutable refund identity and one Mimi finance exception. The receipt, stock, retail returns and original settlement stay as recorded. |
| Await the bank | The later acquirer remittance names the source refund. The bank's posted-entry endpoint has no entry for the new trace yet. The exercise imports the exact prospective figures and tries closing the case. | The close is refused. No debit is booked. The exception survives restart. |
| Match the debit | The separate bank posts −120 cents. dakim pay matches it to the later remittance. Mimi independently fetches the matched export and compares the payout, bank trace, currency, amount and every statement line to the import. | A second authorized reviewer closes the statement. The new journal records bank −120 and provider refund suspense +120. Repeated requests, restarts and a repeated bank-post command preserve one debit. |

**Suspense is an open accounting question.** It records the offset to cash until finance knows why the provider returned the money. A bank debit alone cannot establish that a carton came back, how to restate tax, or who bears the loss. The bank match closes the cash reconciliation. Further classification needs business evidence.

## Operator and service boundaries

In **dakim pay → Payments**, the refund is labeled **Acquirer refund**. Open the payment and choose **Check refund evidence** for its local and acquirer references. The later payout is labeled **Bank debit**. Mimi's finance view ties the exception to its original receipt, payout and current bank status. An unmatched original stays with finance for review. The original sale and its first payout remain searchable.

The merchant cannot initiate an acquirer-originated refund. The test driver uses the existing acquirer simulator role and a private local database socket. It exposes no HTTP write route. Ordinary merchant and source-originated refunds compete for the same remaining amount at the acquirer. An inconclusive response leaves a merchant reservation pending until an authoritative answer resolves it. [Simulator entry point](../cmd/simulate-source-refund/main.go) · [Acquirer transaction](../internal/simulator/source_refund.go)

dakim pay publishes verified adjustments through the existing merchant credential at `GET /v1/adjustments?after=0&limit=50`. A connector translates a processor lookup by operation or statement reference into the refund facts used by the ledger. Sequence assignment is serialized through commit so concurrent writers cannot cause a reader to skip an adjustment. Mimi persists each notice and rejects conflicting reuse of its identity. The worker retrieves evidence outside the retail writer lock. Matching uses currency and original references on both sides. [Payment service and feed](../internal/pay/adjustments.go) · [Matching and bank export](../internal/pay/settlement.go)

## Reproduce the evidence

With the companion Mimi checkout at `../mimi`, run `./scripts/verify` and `MIMI_SOURCE=../mimi ./scripts/verify-mimi` from dakim pay. Both allocate and clean up disposable installations. To save a new synthetic trace, set `DAKIM_SHIFT_EVIDENCE_FILE` to a new file path before running the second command. It records the source operation, both bank traces, journal entries and a before-and-after digest of the original receipt, settlement records, retail-return count and stock-movement count. [Cross-app exercise](../scripts/mimi_late_refund_check.py) · [Mimi integration patch](../integrations/mimi-simulator.patch)

The targeted tests include concurrent source and merchant refunds, recovery of a declined merchant request whose reply was lost, a missing source notification recovered from the statement, duplicate or inconsistent evidence, and an attempt to close a debit using an unverified bank reference. [Acquirer tests](../internal/simulator/source_refund_test.go) · [Payment service tests](../internal/pay/adjustments_test.go)

Both applications passed PostgreSQL-backed race tests, static checks and dependency verification. An upgrade from Mimi migration 130 to 135 preserved the row counts and SHA-256 fingerprints of 14 historical tables and views. The upgraded store then reconciled the provider refund against a receipt and deposit recorded by the previous version. [Upgrade fingerprints](post-receipt-upgrade.json)

The browser regression exposed a kiosk transition that could reject the next shopper's first scan while receipt access was closing. Mimi now hides the previous receipt immediately and waits for the store to confirm closure before showing the scanner. The Chrome test dropped the closure acknowledgment, reloaded the kiosk, retried the original close and scanned an unknown item through normal staff assistance. The full browser suite passed. A backup restore reproduced 81 table fingerprints.

## Benchmarks and next checkpoint

Finance can observe the original refund outcome, its assignment to a later remittance, and the merchant bank posting independently. Stripe documents refund states and itemized reporting for automatic payouts. WeChat Pay Global documents a separate lookup for an individual refund. Alipay Global distinguishes its transaction file from the settlement file. Each product's actual references, timing, fees and funding rules still require a partner contract and qualification. [Stripe refunds](https://docs.stripe.com/refunds) · [Stripe payout reconciliation](https://docs.stripe.com/reports/payout-reconciliation) · [WeChat Pay refund query](https://pay.wechatpay.cn/doc/global/v3/en/4012356521) · [Alipay reconciliation](https://global.alipay.com/developer/helpcenter/detail?_route=sg&categoryId=50478&knowId=201602452430&sceneCode=AC_DEV)

**Subsequent checkpoint.** Mimi finance can now record the business reason for a bank-matched provider refund and obtain independent approval. The approved reporting annotation leaves the full amount booked to provider refund suspense. Ledger reclassification, merchandise returns and tax corrections would each require a separate business decision. [Finance reporting checkpoint](finance-classification.md)

**Release status.** This is a synthetic local exercise. Real counterparties, live bank statements, regulated production operations and scheme certification have not been assessed.
