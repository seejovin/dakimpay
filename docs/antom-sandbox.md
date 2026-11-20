# Alipay wallet through Antom

The `antom_test` connector implements Antom online sandbox `CASHIER_PAYMENT` with `ALIPAY_CN`. It uses hosted WEB presentment and the real Antom request and response formats. The included automated test uses independently generated merchant and provider RSA keys and an in-process HTTP counterpart that verifies the merchant's exact wire signature. It runs against a real PostgreSQL connector journal. The merchant HTTP integration exercise enters through the gateway and route catalog, then verifies a $12.34 wallet payment, the independent goods-release hold, and an $8.00 original-payment refund. No provisioned Antom merchant or wallet participated in that test.

## Configure one test account

Create the merchant test credentials in Antom Dashboard and select the region assigned to that account. The connector configuration is private and includes the merchant's RSA private key. Give it its own empty PostgreSQL database and local database identity. Its durable one-time egress journal must survive worker and gateway restarts. Keep the selected partner credential, database and route exclusive to this merchant.

Gateway connector entry:

```json
{"id":"antom-mimi","kind":"antom_test","config_file":"/absolute/private/antom-mimi.json"}
```

Private connector file shape (placeholders require your sandbox values):

```json
{
  "database_url":"postgresql://adapter:password@127.0.0.1:5432/antom_test",
  "settings":{
    "merchant":"mimi",
    "gateway_account":"acct-mimi",
    "route_id":"alipay-apac",
    "connector_id":"antom-mimi",
    "currency":"USD",
    "client_id":"SANDBOX_<assigned identifier>",
    "merchant_account_id":"<assigned Antom account>",
    "region":"asia",
    "merchant_key_version":"1",
    "antom_key_version":"1",
    "merchant_private_key_pem":"<PKCS#8 or PKCS#1 RSA private key in PEM>",
    "antom_public_key_pem":"<Antom RSA verification key as PKIX PUBLIC KEY in PEM>",
    "redirect_url":"https://merchant.example/payment-return"
  }
}
```

Provision the matching merchant route with the original merchant, gateway account, provider account and currency, `methods:["alipay"]` and `channels:["hosted"]`. Only sandbox client IDs are accepted. The connector fixes the API path to `/ams/sandbox/api/v1/payments/` and the regional hostname to Antom’s published allowlist. This implementation supports `asia`, `europe` and `north-america-nonus`. Transactions request settlement in the same currency. Card, in-store, US-merchant North America, FX settlement and production endpoints are outside this integration. The merchant-controlled HTTPS return URL supports shopper navigation. Notifications, if enabled in the provider dashboard, do not establish a financial outcome in this connector.

## What the connector verifies

| Operation | Independent evidence and recovery |
| --- | --- |
| Select and submit original | The connector fences its private journal by random persistent ledger identity and durable client/program/region/account binding. It persists one permit and an alphanumeric request ID bound to the source ledger and full instruction before sending `pay`. It queries only originals from its own exclusive request namespace. |
| Present wallet | A signed original `pay` reply must return the exact request ID, amount, currency, stable provider payment ID and an allowed HTTPS URL. The connector stores the original hosted capability and verifies pending status with `inquiryPayment`. Losing the original redirect before it is durably saved can leave a pending checkout without a presentable link. Polling can still recover a later final payment. |
| Verify charge | A signed `inquiryPayment` reply must correlate the request ID, payment ID and amount/currency. API success and financial success are separately evaluated. A losing or missing response cannot cause a second financial POST. A lost or replaced journal fails the previously pinned identity. |
| Refund original | The parent must be the connector's captured charge in the same merchant, route, account, currency, outlet and order. The separate refund permit retains the original provider payment ID. The request includes provider-supported metadata binding the refund request to that payment. Signed `inquiryRefund` must echo the expected request ID, refund ID, amount, currency and metadata. When present, provider payment ID must also match. “Refunded” records the successful Antom result. Receipt by the buyer's wallet and bank funding are separate facts. |
| Verify the wire | RSA PKCS#1 v1.5 with SHA-256 signs the method, URI, client ID, request time and exact request bytes. The independent Antom public key verifies the response URI, client ID, response time and raw body. The connector rejects duplicate signatures, ambiguous JSON fields, unknown key versions, stale replies, different account/operation/amount/currency, and conflicting terminal histories. Financial commands never follow redirects. |
| Goods release | Payment inquiry alone does not prove a complete history of external refunds and disputes. This adapter returns incomplete first-fulfilment evidence. Merchant release remains held until an independently qualified inventory is implemented. |
| Settlement | This adapter has no automatic Antom statement or bank importer. A verified payment inquiry cannot match a payout or close a bank reconciliation. |

Run `go test -race ./internal/antomtest` with `DAKIM_TEST_DATABASE_URL` pointing to an isolated test PostgreSQL database. The signed fixture verifies the same HTTPS destination, path, POST fields and signatures built by the adapter without external network or merchant credentials.

Primary contracts: [sandbox setup](https://docs.antom.com/ac/ref/sandbox), [regional online sandbox paths](https://iopenhome.alipay.com/docs/ac/ams/api), [signature generation and verification](https://docs.antom.com/ac/ams/digital_signature), [pay](https://docs.antom.com/ac/ams/payment_cashier), [payment inquiry](https://docs.antom.com/ac/ams/paymentri_online), [refund](https://docs.antom.com/ac/ams/refund_online), [refund inquiry](https://docs.antom.com/ac/ams/ir_online).

For the packaged local launcher, use [the persistent partner route file](aggregate-installation.md#enable-an-external-test-connector) so provisioning retains your enrollment on restart.
