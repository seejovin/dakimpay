# Mimi’s $97.46 deposit

Mimi’s synthetic $42.00 terminal sale and $68.00 online sale produced **one $97.46 posted bank deposit** after a $10.00 refund and $2.54 in fees. This run used disposable, separate dakim pay, acquirer and bank databases on 19 November 2026. Times below are UTC. The console represented Mimi’s online checkout.

| Time | Recorded event |
|---|---|
| 14:03:53.371661 | Terminal MIMI-01 lost the reply to its Visa payment. The acquirer had captured the sale. dakim pay displayed an unknown outcome. |
| 14:03:53.373514 | An inquiry about the original terminal operation confirmed the capture. Mimi had one $42.00 terminal charge. |
| 14:03:53.375195–14:03:53.377644 | The Mastercard checkout awaited its buyer. The acquirer recorded simulated buyer approval, and dakim pay verified the original $68.00 capture. |
| 14:03:53.380868 | The acquirer confirmed a $10.00 partial refund against the online charge. |
| 14:03:53.387047 | The payout was in transit. The bank’s posted-entry lookup returned HTTP 404, the merchant statement was unavailable (HTTP 409), and reconciliation showed $97.46 pending and $0.00 deposited. |
| 14:03:53.392611–14:03:53.393139 | The bank recorded its own posting. dakim pay then matched the bank trace, merchant `mimi`, currency USD and amount 9,746 cents. Mimi’s deposit statement became available. |

## Statement and references

| Statement item | Gross | Fee | Deposit contribution | dakim pay operation |
|---|---:|---:|---:|---|
| Terminal Visa | $42.00 | $0.84 | $41.16 | `dp_82930d34fec36e2babdcfb283de94ba3` |
| Online Mastercard | $68.00 | $1.70 | $66.30 | `dp_8075c2067766a2e7ad1b79f7c15e31b7` |
| Online refund | −$10.00 | $0.00 | −$10.00 | `dr_0394fe827581ec18334d117909f9d2e1` |
| **Total** | **$100.00** | **$2.54** | **$97.46** | |

The acquirer reported terminal charge `ch_eb19ccab-6b37-4a33-8504-2200a740b116`, online charge `ch_dcd61522-a547-4288-87ef-f974be87e88a` and refund `rf_2846d069-3dd1-40fd-a7e8-b80830e0817b`. Its payout `po_eed1b6f8-7abf-4671-9d82-aac4514b393f` matched bank trace `bt_4d7da942-cc84-4168-b08c-20d66c44f79d`.

To reproduce the journey in the local console, select **Run payment journey**, then **View journey trace**. Select **Run payment journey** again. In this run the second response returned the same five references, amount and matched status. The statement and totals were identical, with two payments, one refund and one payout. A fresh isolated run generates its own references.

To verify the lost reply, pre-post bank checks and replay automatically from the dakim pay directory, run `./scripts/verify`. The verifier creates and removes its own local installation. [Verification script](../scripts/verify)
