# Run the independent virtual providers

Run `scripts/aggregator-local start` and `scripts/aggregator-local open` in the source release. The register opens alongside its enrolled virtual card reader. For either wallet, choose **Hosted checkout** in the register. The buyer opens the processor page, then enters the simulated wallet and approves. The WeChat Pay flow also renders a QR code for the local wallet URL. Scanning that loopback URL on an external phone cannot reach the computer. The Alipay flow uses a buyer handoff.

| Service | Default local port | Authority |
| --- | ---: | --- |
| Merchant gateway | 8480 | Merchant instructions, route selection, journal and finance review |
| Primary virtual card acquirer | 8481 | Hosted and attended Visa, Mastercard, American Express and generic card orders |
| Secondary virtual card acquirer | 8482 | Hosted card payments when admitted on its own route |
| Virtual WeChat Pay provider | 8483 | Buyer QR and wallet decisions for WeChat Pay instructions |
| Virtual Alipay provider | 8484 | Buyer handoff and wallet decisions for Alipay instructions |
| Private PostgreSQL | 55595 | Separate database and database credential per service in the installation |

The first merchant is Mimi. Each provider has a distinct process, connector credential, database login, database, merchant settlement account and statement exporter/importer credentials. The local launcher owns a separate setup under `~/.local/share/dakim-aggregator`. The earlier single-acquirer environment continues to use `scripts/gateway-local` and its own state directory. Set `DAKIM_GATEWAY_STATE` to an empty owner-only directory to create a distinct installation. A PostgreSQL installation persists ordinary stop/start cycles. Spanner qualification replaces the gateway's store with the local emulator. The four synthetic acquirers continue to own independent PostgreSQL databases.

## Operator qualification

```sh
./scripts/verify-aggregator
./scripts/with-spanner-emulator ./scripts/verify-aggregator
./scripts/verify-aggregator --concurrency
./scripts/verify-aggregator --browser
```

The first two commands test the same multi-rail payment and finance journey on separate gateway backends. The concurrency exercise uses eight HTTP clients to race duplicate payment and refund creation across two merchants and four sources. It compares every original reference, currency amount, journal group and independent source movement. To run it with the Spanner gateway store, use `scripts/with-spanner-emulator scripts/verify-aggregator --concurrency`. The browser check uses Node.js, Playwright and Chromium. Set `DAKIM_CHROMIUM_BIN` to an installed compatible executable if needed. All checks use private temporary PostgreSQL state and numeric loopback listeners.

The standard exercise confirms observed card brands, authenticates both wallet handoffs, checks tenant access, and duplicates each buyer decision. It stops the gateway mid-sale, accepts the original at the primary acquirer, stops that acquirer, suspends its route, and restarts the gateway. A new Visa order can use the secondary. The old order and its eventual refund keep their primary binding. The exercise reads the independent financial movements in every provider database and matches four different frozen sources sharing the same native report ID to four distinct synthetic bank entries.

Merchant staff can suspend a route immediately through `POST /v2/program/routes/{route}/sales/freeze` under a finance or reviewer credential. This applies to the named merchant's enrollment. Finance proposes resumption and a different reviewer approves. Gateway restart preserves the control decision.

To alter the local fixture's static configuration across all enrolled merchants, write a JSON list of route IDs to the private state directory's `disabled-routes.json` using mode 0600. The allowed IDs are `card-primary`, `card-secondary`, `wechat` and `alipay`. Then run `scripts/aggregator-local restart-gateway`. Previously dispatched money operations still require evidence from the original provider. Disabling the primary removes the attended card lane from new admissions. The secondary is configured for hosted cards only. Starting and stopping a single provider uses `scripts/aggregator-local stop-provider card-primary` and `scripts/aggregator-local start-provider card-primary`. Stop the entire installation with `scripts/aggregator-local stop`.

These controls run on a single OS account and synthetic local processors. The isolated databases simulate payment and settlement evidence and cannot approve live network participation or establish live funds receipt.

## Enable an external test connector

The default launcher contacts local virtual providers only. Put an optional `partner-routes.json` in the private aggregator state directory and set its file mode to 0600. It persists across launcher restarts. Store the partner credential in a separate private file and use a dedicated persistent local PostgreSQL database. Follow [Stripe test settings](partner-stripe.md) or [Antom test settings](antom-sandbox.md) to configure that file. The gateway validates the route against its merchant program and the partner settings. It also binds native account ownership in the gateway and guards against reusing the same database for two service roles.

For the Mimi fixture, this example enrolls one hosted Stripe test route. The `provider_account` must equal the actual sandbox account ID. Only enable the methods the test account can accept. A lower priority number wins for a new eligible instruction.

```json
{
  "connectors": [
    {"id":"stripe-test","kind":"stripe_test","config_file":"/absolute/private/stripe.json"}
  ],
  "routes": [
    {"id":"stripe-hosted","merchant":"mimi","account":"acct-mimi","connector":"stripe-test","provider_account":"acct_REPLACE","currency":"USD","methods":["card","wechat_pay","alipay"],"channels":["hosted"],"min_amount_minor":1,"max_amount_minor":1000000,"priority":5,"enabled":true}
  ]
}
```

Run `scripts/aggregator-local restart-gateway` and use hosted checkout on the register. To leave the route available exclusively for original inquiries and refunds, suspend sales for that merchant and route. Keep the original credential, enrollment and database accessible while historical transactions exist. A lost, rotated or reassigned upstream account must be independently reconciled before migration. Merchant settlement import, bank feed, physical acceptance and remote partner certification require their own qualification.
