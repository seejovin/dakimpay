"""Bounded hosted workload for the private two-merchant verify-gateway fixture."""
from collections import Counter
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
import json
import os
from time import perf_counter
from urllib.parse import urlsplit


@dataclass
class Checkout:
    party: object
    body: dict
    key: str
    operation: str = ""
    buyer_path: str = ""
    reference: str = ""


def require(ok, message):
    if not ok:
        raise AssertionError(message)


def verify_concurrency(test, processor_postings):
    workers, per_merchant = 8, 16
    cases = []
    for index in range(per_merchant):
        # Both merchants deliberately reuse the same business and transport keys.
        for merchant_index, party in enumerate((test.a, test.b)):
            amount = 100 + 17 * index + 53 * merchant_index
            suffix = f"hosted-{index}"
            cases.append(Checkout(party, test.command(party, "hosted", amount, suffix), test.key(suffix)))

    def create(case):
        return test.make(case.party, case.body, case.key, codes=(200, 201))

    def pending(case):
        op = test.wait_resource(case.party, "/v2/payment-intents/" + case.operation, "pending")
        url = urlsplit(op["checkout_url"])
        require(f"{url.scheme}://{url.netloc}" == test.rail.origin and
                url.path.startswith("/checkout/") and not url.query and not url.fragment,
                "Unexpected hosted checkout destination")
        case.buyer_path = url.path

    def approve(case):
        test.rail.request(case.buyer_path, data={"decision": "approve"}, form=True, text=True)

    def captured(case):
        op = test.wait_resource(case.party, "/v2/payment-intents/" + case.operation, "captured")
        require(op["merchant_id"] == case.party.merchant and op["processor_account"] == case.party.account
                and op["amount_minor"] == case.body["amount_minor"] and op["currency"] == case.party.currency
                and op["operation_id"] == case.operation and op["order_id"] == case.body["order_id"]
                and op["reference"], "Captured operation terms differ from the original")
        case.reference = op["reference"]

    # Measure outside the services; fixture setup, schema creation, build and teardown are excluded.
    start = perf_counter()
    with ThreadPoolExecutor(max_workers=workers) as pool:
        repeated = [case for case in cases for _ in range(2)]
        receipts = list(pool.map(create, repeated))
        for index, case in enumerate(cases):
            pair = receipts[index * 2:index * 2 + 2]
            case.operation = pair[0]["operation_id"]
            require(all(r["id"] == case.operation and r["merchant_id"] == case.party.merchant and
                        r["amount_minor"] == case.body["amount_minor"] for r in pair),
                    "Duplicate admission changed the identity, merchant, or amount")
        require(len({c.operation for c in cases}) == len(cases), "Two unique checkouts share an operation")
        admitted = perf_counter()
        list(pool.map(pending, cases))
        presented = perf_counter()
        list(pool.map(approve, repeated))
        approved = perf_counter()
        list(pool.map(captured, cases))
        finished = perf_counter()
    require(len({c.reference for c in cases}) == len(cases), "Processor movement reference reused")

    totals = {}
    expected = []
    for party in (test.a, test.b):
        group = [c for c in cases if c.party == party]
        journal = test.merchant(party, "/v1/journal?limit=250")
        entries = journal["entries"]
        require(len(entries) == len(group) == journal["through_sequence"] == journal["next_after"],
                "Isolated fixture has missing or additional journal entries")
        amounts = {c.operation: (c.body["amount_minor"], c.reference, c.body["order_id"]) for c in group}
        seen = set()
        balances = Counter()
        for entry in entries:
            op = entry["operation_id"]
            require(op in amounts and op not in seen, "Unrecognized or repeated journal operation")
            seen.add(op)
            amount, ref, order = amounts[op]
            require(entry["kind"] == "capture" and entry["reference"] == ref and entry["order_id"] == order
                    and entry["merchant_id"] == party.merchant and entry["processor_account"] == party.account
                    and entry["currency"] == party.currency, "Journal terms differ from original capture")
            legs = {leg["account"]: (leg["debit_minor"], leg["credit_minor"]) for leg in entry["legs"]}
            require(legs == {"processor_clearing": (amount, 0), "merchant_proceeds": (0, amount)},
                    "Capture did not debit and credit the original amount")
            for account, (debit, credit) in legs.items():
                balances[account] += debit - credit
            expected.append((party.merchant, party.currency, "capture", str(amount), op, ref))
        total = sum(c.body["amount_minor"] for c in group)
        require(balances == {"processor_clearing": total, "merchant_proceeds": -total},
                "Merchant's aggregate ledger differs from original instructions")
        totals[party.merchant] = {"currency": party.currency, "captures": len(group), "amount_minor": total}
    require(processor_postings() == sorted(expected),
            "Independent processor movement table has missing, additional or inconsistent financial postings")
    verified = perf_counter()
    stages = {"admission": admitted - start, "wait_for_hosted_urls": presented - admitted,
              "buyer_approvals": approved - presented, "wait_for_gateway_captures": finished - approved,
              "reconciliation": verified - finished, "total": verified - start}
    print(json.dumps({
        "result": "pass", "gateway_store": "spanner-emulator" if os.environ.get("DAKIM_SPANNER_DATABASE") else "postgres",
        "unique_checkouts": len(cases), "concurrent_clients": workers, "create_requests": len(repeated),
        "buyer_approval_requests": len(repeated), "merchants": totals,
        "external_wall_seconds": {name: round(seconds, 3) for name, seconds in stages.items()},
    }, sort_keys=True), flush=True)
