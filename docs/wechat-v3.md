# WeChat Pay cross-border Native protocol qualification

`internal/wechatv3` implements a loopback-only client for the Hong Kong cross-border v3 Native API, in institutional mode. It binds the institution app, institution merchant ID and sub-merchant ID. The independently signing test server generates fresh merchant and platform RSA keys at runtime. Run `go test -race -v ./internal/wechatv3` from source. No platform credentials are required.

| Operation | Implemented wire behavior | Meaning of the response |
| --- | --- | --- |
| Native QR order | POST `/v3/global/transactions/native`. The JSON binds institution, sub-merchant, merchant order, pricing currency, integer amount, merchant category, description, notification URL and NATIVE trade type. | A platform-signed `code_url` is the QR payload. Receipt of that value leaves the payment unsettled. |
| Original payment inquiry | GET `/v3/global/transactions/out-trade-no/{original}` with both `sp_mchid` and `sub_mchid` in the signed query string. | The response must agree on institution app, merchant, sub-merchant, original order, NATIVE product, priced amount and currency. A successful observation requires a processor transaction ID and parseable completion time. |
| Partial refund submission | POST `/v3/global/refunds` with the verified original processor transaction ID, stable merchant refund number, original total, refund amount and priced currency. | Returns an acceptance identifier. The client requires an unaltered original inquiry from the same instance before it signs a refund request. |
| Original refund inquiry | GET `/v3/global/refunds/out-refund-no/{original}` using the institutional query scope. | The signed response must match both payment identities, refund number, amount, currency and the acceptance ID when available. Processing and abnormal outcomes remain independently visible. |

The client signs the exact HTTP method, path and query, timestamp, nonce and transmitted body using SHA-256 with RSA PKCS#1 v1.5. Response verification uses the original response bytes and a provisioned platform verification key selected by the signed-message header serial. It enforces one value for each required header, an explicitly supported signature algorithm and a five-minute timestamp window. An unknown key, changed scope, conflicting object fields, unrecognized state or unverifiable response produces an error. The HTTP transport uses a single explicit numeric loopback destination and rejects redirects. No POST retry logic is supplied. Order inquiry and refund inquiry use the caller’s original identifiers.

The test peer independently verifies merchant request signatures and exact API fields. Positive cases cover a synthetic HKD 12.34 payment and HKD 2.00 partial refund. Negative cases cover wrong sub-merchant, original, pricing currency, amount, product, refund parent or acceptance ID, ambiguous JSON, missing/duplicate headers, stale yet correctly signed replies, unknown keys and tampered signatures.

**Qualification boundary.** The running virtual WeChat Pay wallet uses dakim’s synthetic processor. This separate client exercises the published wire protocol. It has no real issuer or acquirer connection, participant credentials, real callback service, certificate lifecycle, funds-flow or remittance feed. It does not perform notification authentication/decryption, dispute inventory, commercial retry scheduling, refund eligibility windows or bank reconciliation. A signed code-only placement reply provides no immutable merchant-order echo. A signed refund inquiry does not by itself prove the absence of other refunds. Production enablement requires provider-issued assignments, timestamp/key monitoring, independently provisioned and rotated trust, authenticated order/refund notification handling and qualification of those missing flows. Each new partner contract must identify supported products, currencies and settlement destinations.

## Primary specifications

- [WeChat Pay Native order placement](https://pay.wechatpay.cn/doc/global/v3/en/4012356795)
- [Query the original order](https://pay.wechatpay.cn/doc/global/v3/en/4012356519)
- [Submit a refund](https://pay.wechatpay.cn/doc/global/v3/en/4012356520)
- [Query one refund](https://pay.wechatpay.cn/doc/global/v3/en/4012356521)
- [Sign requests](https://pay.wechatpay.cn/doc/global/v3/en/4012357147)
- [Verify response signatures](https://pay.wechatpay.cn/doc/global/v3/en/4012357149)
