# dakim pay

dakim pay is a multi-merchant payment rail aggregator. A merchant uses one API for attended and hosted card sales, WeChat Pay QR payments and Alipay checkout. The gateway selects an enrolled acquiring route and permanently binds it to the operation. The card networks in this build are Visa, Mastercard and American Express. A bank account, network membership or acquiring contract is never implied by configuring a route.

## Run the virtual gateway from source

Install Go 1.25 or later, Python 3.9 or later and PostgreSQL server and client tools. From the repository root, run these commands as an ordinary local user.

```sh
./scripts/aggregator-local start
./scripts/aggregator-local open
```

The launcher opens the merchant register and virtual reader. Choose hosted checkout for WeChat Pay or Alipay. It runs four local provider processes with separate databases and credentials. Use `./scripts/aggregator-local stop` to shut down. [Local installation](docs/aggregate-installation.md)

The gateway pins each accepted payment to its acquiring route. A signed fee correction identifies one booked fee and its original capture. Confirmation requires independent bank evidence and approval. [Merchant API](docs/merchant-aggregation.openapi.json) · [Route catalog](docs/route-catalog.md) · [Settlement and fee corrections](docs/booked-fee-corrections.md)

The default environment simulates payments. The release includes virtual rail tests and separate partner protocol adapters. Live acquiring and physical terminal qualification require independent acceptance. [Gateway qualification](docs/aggregation-qualification.md) · [Terminal protocol](docs/terminal-protocol-v1.md)

The original single-merchant simulator remains available through `./scripts/local start`. Its state is separate from the aggregator.
