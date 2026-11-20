# Bank-confirmed corrections to a booked fee

A routed merchant can retrieve one posting for a later processor correction to an individual booked fee. The original approved payout must contain the gateway capture and the fee. The correction receives a separate payout ID, bank trace, close proposal and journal entries. Historical inquiries using the earlier journal watermark return the same bytes.

## Source, evidence and approval

| Action | Required evidence | Financial result |
| --- | --- | --- |
| Submit to `POST /v2/settlement-fee-corrections` | The enrolled statement source signs the merchant program, route and provider account, connector, operation, order, tender allocation, capture, original payout/proposal/digest, fee line ID, native fee reference, original fee amount, corrected payout/trace/digest, correction reference and signed delta. | An accepted notice reserves the original fee and the correction reference. Status is `awaiting_bank`. The journal receives zero entries. |
| Import the separate bank posting through `POST /v1/bank-entries` | An independent bank importer supplies the booked entry, beneficiary, signed amount, currency, trace and evidence digest. Exactly one matching bank entry and one payout must qualify for that trace and amount in the book. | The existing bank observation records cash against unallocated bank movements. The correction remains unposted. |
| Prepare and approve the corrected payout | A finance preparer submits the bank entry ID on the routed payout's `/prepare-close` endpoint. A different reviewer submits the exact proposal ID on `/approve-close`. Both must be independent of both source importers. The transaction rechecks the original and corrected settlement evidence. | The correction posting, remittance, bank clearing, approved close and export commit together. A retry returns the existing result. |
| Read from `GET /v2/routes/{route}/accounts/{providerAccount}/payouts/{id}/confirmed-fee-correction` | Merchant, finance or reviewer grant in the same program. The gateway revalidates the archived signed terms, both payouts and the posted journal entry in one consistent snapshot. | HTTP 200 supplies one export with a stable `posting_id`. An existing correction awaiting evidence or approval returns 409. An unknown or ordinary payout returns 404 on this endpoint. |

The statement credential accepts optional `fee_correction_keys`, a map from a key ID to a lowercase hexadecimal 32-byte Ed25519 public key. Only a named route-scoped statement credential can enroll a key. The gateway retains the public key and exact signed notice with the new payout so later key rotation preserves the evidence used at approval. The OpenAPI file defines the field order, domain prefix and canonical signing bytes. [Wire contract](merchant-aggregation.openapi.json)

Amounts use signed integer minor units. The revised fee equals the original fee plus the delta. A positive delta refunds part or all of the fee. A negative delta increases it. The revised fee must stay zero or negative. This version supports one correction per original fee and one signed delta in the correcting payout. A valid staged notice that never receives qualifying bank evidence remains reserved for investigation. Cumulative corrections, replacements for disputed notices, and splitting a correction across multiple bank postings require a separate workflow.

The line ID, native fee reference, amount, capture and payout must jointly resolve to the same records. Two equal fees can therefore be distinguished. The independent bank posting verifies the cash amount and destination. Assigning the cash adjustment to the intended fee still depends on the issuer signing the correct original identity and the reviewer approving it. A later bank return, an ambiguous funding match or an unresolved financial case can suspend a fresh export lookup. A consumer that already imported an earlier confirmation retains that posting and must reconcile subsequent evidence separately.

## Synthetic evidence for Mimi

| Stage | Reference | Signed USD amount |
| --- | --- | ---: |
| Original Visa capture | `mimi-order-001` / `tender-card-001` / `native-001` | +110.00 |
| Original refund | `native-return-001` | −10.00 |
| Original fees | `03-fee` / `interchange-001` and `04-fee` / `assessment-001` | −1.27 each |
| Original approved deposit | `original-day` / `original-bank-credit` | +97.46 |
| Separate approved credit | `corrected-day` / `correction-bank-credit`, applied to `03-fee` | +0.27 |
| Revised interchange fee | `interchange-001` | −1.00 |

The reference file contains the exact HTTP 200 response body, including its final newline. SHA-256 is `80f8074f551fa42798f77c237e65b819670fa89dfea05f3f0e2db62104a6500d`. Its posting ID is `b083e86a72f2cdbdb38db3f2a52c874e0fd03f231b0e4122ceac77826585a362`, at journal sequence 14 in this disposable book. [Confirmed export](../integrations/mimi-fixtures/confirmed-fee-correction-v1.json)

A native importer can validate the response against `components.schemas.ConfirmedFeeCorrection`, match the signed order, allocation and operation to its retained receipt, compare the original capture and fee against its approved deposit, and consume the posting ID once in the same transaction as its journal entry. Replaying the fixture should leave one correction against that payment. After an ordinary importer restart, its receipt and original close should have identical bytes, and the corrected journal should contain exactly one eligible entry for this posting ID. These are acceptance criteria for the current native importer. This fixture was generated by the gateway. The native Mimi receipt and consumer have yet to be verified.

## Reproduce locally

Use a Go toolchain with this module's dependencies already cached, PostgreSQL server tools on the path and an unprivileged OS account. The following shell commands create a disposable cluster with a private Unix socket. They run only the named test with race detection.

```sh
(
  set -eu
  tmp_pg=$(mktemp -d /tmp/dakim-fee.XXXXXXXX)
  trap 'if test -f "$tmp_pg/db/postmaster.pid"; then pg_ctl -D "$tmp_pg/db" -m fast -w stop; fi; rm -rf -- "$tmp_pg"' EXIT
  initdb -D "$tmp_pg/db" --auth-local=peer --auth-host=reject --no-locale -E UTF8
  pg_ctl -D "$tmp_pg/db" -l "$tmp_pg/postgres.log" -w start \
    -o "-c listen_addresses='' -c unix_socket_directories='$tmp_pg' -c unix_socket_permissions=0700"
  export DAKIM_TEST_DATABASE_URL="host=$tmp_pg dbname=postgres sslmode=disable"
  export GOTOOLCHAIN=local GOWORK=off GOSUMDB=off GOPROXY=off GOTELEMETRY=off
  go test -mod=readonly -race -count=1 -run '^TestBookedFeeCorrectionPostgres$' ./internal/gateway
)
```

The test submits an original sale through the v2 payment handler, runs the real worker against an in-memory acquirer, and imports the approved mixed settlement. Six simultaneous source submissions and six approvals produce one correction for the identified fee. A fresh OS process reopens the PostgreSQL database, checks the old close and journal watermark byte-for-byte, and replays the payment, statement, bank entry and approval. It also checks an increased fee against the second original fee and the effect of a subsequent bank return. The standard test compares the confirmed HTTP body to the reference file. To write a fresh copy, set `DAKIM_FEE_FIXTURE_OUTPUT` to an unused absolute path before running it.

The active aggregator uses `cmd/dakim-gateway`, `internal/gateway` and the existing serializable `internal/gwstore` transactions. This change adds records within the existing store schema. These tests qualify the local PostgreSQL path. The separate Cloud donor archive and the current native Mimi working tree were unavailable for the gateway-only checkpoint.

The checkpoint also ran the following existing and new Go tests with `-mod=readonly -race -count=1` on the same isolated configuration. The correction PostgreSQL test additionally passed three consecutive runs with `-count=3`. The active gateway binary built offline and `go vet` passed for the gateway, aggregator, command and store packages.

| Package | Exact test names |
| --- | --- |
| `internal/gateway` | `TestBookedFeeCorrectionPostgres`, `TestFeeCorrectionSignedWire`, `TestFinancialControlsPostgres` (including `independent_processor_namespaces`), `TestFinanceWireAndDirection`, `TestMerchantRouteStaysPinnedAcrossRestartAndRefund` |
| `internal/aggregation` | `TestCatalogSelectionAndDiscovery`, `TestOriginalOnlyAfterSuspensionTimeoutAndRemap`, `TestMismatchedEvidenceAndPartialPinsFailClosed`, `TestConfigurationRejectsAmbiguity` |
| `cmd/dakim-gateway` | `TestFeeCorrectionKeyEnrollment`, `TestAggregatorEnforcesConnectorAndStatementAuthority`, `TestCredentialScopeAndRoleValidation` |
