# Stripe sandbox acquiring connection

The optional `stripe_test` connector sends authenticated API requests to Stripe’s real test environment. The default release runs entirely against independent virtual processors. No external Stripe account participated in the local acceptance results. The code test exercises provider-shaped HTTP responses, a real PostgreSQL sidecar, lost responses, restarts, refund lineage and conflicting financial evidence.

| Supported operation | Evidence |
| --- | --- |
| Hosted card payment | Stripe Checkout collects the card. The connector uses `card` at selection, then records the card brand Stripe actually observed and the charge's processing network when Stripe returns it. A customer can use eligible Visa, Mastercard or American Express test instruments. Exact brand availability depends on Stripe account and card terms. |
| Hosted WeChat Pay and Alipay | Each eligible wallet is explicitly enabled per route and currency. Stripe provides the hosted experience. These two methods use Stripe’s wallet integration and its settlement relationship. A direct WeChat or Antom contract is a separate integration. |
| Capture | A freshly retrieved test Checkout Session must match merchant operation metadata, amount, currency and selected method. Capture requires its expanded succeeded PaymentIntent and the paid, captured original Charge to agree. Buyer redirection alone leaves payment unresolved. |
| Refund | Uses the original verified Charge ID. A fresh Refund retrieval verifies amount, currency, parent, immutable metadata and status. The adapter also rechecks the parent test Session and Charge. Pending and failed refunds remain distinct. |
| Missing create response | A durable write-once PostgreSQL record precedes external submission. No second POST is permitted for that operation, even after restart. A read-only retrieval can establish the original from an independently obtained provider object ID. |
| Retail fulfilment and settlement | For a clean capture, the connector retrieves the original capture twice, reads the charge-scoped refund and dispute lists in between, and requires both complete lists to be empty and both original reads to agree. Any returned refund or dispute (including pending activity), incomplete page, changed original or unavailable source holds first fulfilment. This bounded observation covers the time of inquiry. A later dispute or refund is possible. The gateway separately checks local reservations and finance exceptions. Partner statement ingestion and independent bank funding remain outside this connector. |

## Configure a sandbox account

1. Obtain a **Stripe test-mode secret** from your own sandbox. Enable the intended methods there and verify the business-country and settlement-currency table for each wallet. Store the credential outside the repository in the connector configuration. The default gateway does not contact Stripe.
2. Create a **dedicated local PostgreSQL database and role** for this connector. The initializing role needs CREATE on that database and DDL/DML rights for the `partnerstripe` schema. Use a persistent database. A new empty database gets a new random ledger identity and cannot replay an enrolled gateway route. A point-in-time restore requires independent reconciliation before re-enabling dispatch.
3. Save this file under your service account with mode 0600. The example assumes a US sandbox with USD wallet support. Replace the account, secret, credential and merchant HTTPS return URL. Each declared method/currency must also be allowed in the gateway catalog.

```json
{
  "database_url":"postgres://stripe_test_role:<password>@127.0.0.1:5432/dakim_stripe_test",
  "settings":{
    "test_key":"<sk_test_...>",
    "account_id":"acct_REPLACE",
    "merchant":"mimi",
    "gateway_account":"acct-mimi",
    "connector_id":"stripe-test",
    "return_url":"https://merchant.example/sandbox/return",
    "allowed_routes":{"stripe-hosted":["card","wechat_pay","alipay"]},
    "capabilities":{"card":["USD"],"wechat_pay":["USD"],"alipay":["USD"]}
  }
}
```

4. Add the connector `{"id":"stripe-test","kind":"stripe_test","config_file":"/absolute/private/stripe.json"}` and explicitly provision its hosted route in the gateway catalog. Set the provider account to the same `acct_…`, merchant and gateway account to the above values and permitted method/currency pairs to the enabled capabilities. Keep separate program or currency permissions when actual access differs. The connector checks `GET /v1/account` at initialization, requires the configured identity and enforces its supported account-country/currency combinations. Confirm account activation in Stripe. Where the sandbox returns a definitive API rejection, retain the original command for investigation.
5. Start the gateway and create an ordinary hosted merchant payment for one eligible method. Complete the buyer interaction on the returned Stripe-hosted page using the provider's test instructions. Query the original dakim operation for the capture and original charge reference. Submit an original-linked refund through the ordinary merchant API. The connector uses the default Stripe-hosted checkout domain and direct merchant-account credentials. Custom checkout domains, Connect account switching and physical readers are outside this connector's qualified surface.

## Account inspection and original recovery

Build the operator utility from the release source using `go build -o dakim-partner ./cmd/dakim-partner`. It reads the same private connector configuration as the optional gateway route. `check` contacts Stripe and opens/initializes the isolated sidecar. `inspect` reads only the original from Stripe. Its JSON may include an active buyer URL, which should be shared only with the intended buyer.

```sh
./dakim-partner check -config /absolute/private/stripe.json
./dakim-partner inspect -config /absolute/private/stripe.json -operation pi_original_gateway_id
./dakim-partner recover -config /absolute/private/stripe.json -operation pi_original_gateway_id -object cs_test_original
```

After an ambiguous response, locate the original test Checkout Session or Refund in the authenticated Stripe Dashboard or request log using the operation metadata. Supply that original object's ID to `recover`. It re-reads the original over the fixed Stripe API origin, compares the ledger ID, operation ID, hashed financial terms, parent and amount, and binds that source ID to the stored operation. Recheck the original gateway record. If no matching object can be established, keep the operation on hold. The operator utility can neither create a new payment nor force a charge to succeeded.

The API version is pinned to `2025-06-30.basil`. Keep remote qualification evidence per account, currency and method and exercise declines, approval/rejection, refunds, timeouts and lost responses before expanding the route. A test-mode secret and a successful account inquiry alone do not prove successful wallet or card acceptance.

## Provider references

- [Create a Checkout Session](https://docs.stripe.com/api/checkout/sessions/create) · [Retrieve a Session](https://docs.stripe.com/api/checkout/sessions/retrieve)
- [Charge evidence](https://docs.stripe.com/api/charges/object) · [Create a refund](https://docs.stripe.com/api/refunds/create) · [Refund evidence](https://docs.stripe.com/api/refunds/object)
- [Refund inventory](https://docs.stripe.com/api/refunds/list) · [Dispute inventory](https://docs.stripe.com/api/disputes/list)
- [Idempotency-key lifetime](https://docs.stripe.com/api/idempotent_requests) · [Retrieve the account](https://docs.stripe.com/api/accounts/retrieve)
- [Alipay account and currency requirements](https://docs.stripe.com/payments/alipay) · [WeChat Pay account and currency requirements](https://docs.stripe.com/payments/wechat-pay) · [Stripe test payment methods](https://docs.stripe.com/testing)

For the packaged local launcher, use [the persistent partner route file](aggregate-installation.md#enable-an-external-test-connector) so provisioning retains your enrollment on restart.
