"""Bounded multi-merchant, multi-provider verification for scripts/verify-aggregator."""
from collections import Counter
from concurrent.futures import ThreadPoolExecutor
from copy import copy
from dataclasses import dataclass
import json
from http.client import RemoteDisconnected
from threading import Lock
from time import monotonic, perf_counter, sleep
from urllib.error import URLError


@dataclass
class Order:
    owner: object
    route: str
    method: str
    tag: str
    body: dict
    key: str
    original: dict = None
    refund: dict = None


def verify(test, wait, require):
    a = test
    b = copy(test)
    b.m, b.other = a.other, a.m
    scope = "/v2/program/routes/card-primary/sales/freeze"
    b.gateway.request(scope, token=b.m["tokens"]["finance"], key="market-two-secondary",
                      data={"reason": "Measure an independent merchant and card acquiring route"})
    orders = []
    for i in range(8):
        for j, (who, route, method, tag) in enumerate((
                (a, "card-primary", "visa" if i % 2 else "amex", "card"),
                (b, "card-secondary", "mastercard", "card"),
                ((a, b)[i % 2], "wechat", "wechat_pay", "wechat"),
                ((b, a)[i % 2], "alipay", "alipay", "alipay"))):
            identity = tag + "-" + str(i)
            body = {"order_id": identity, "allocation_id": "main", "outlet_id": who.m["outlet"], "channel": "hosted",
                    "currency": "USD", "payment_method": method, "amount_minor": 135 + 11*i + 17*j}
            orders.append(Order(who, route, method, identity, body, "req-" + identity))

    retries = Counter()
    counter_lock = Lock()

    def replayable_post(owner, path, key, body, kind):
        # Timeout can occur after commit. Retry only this exact request until
        # the original resource is returned. Record all additional transmissions.
        deadline = monotonic() + 65
        for attempt in range(5):
            if attempt:
                with counter_lock:
                    retries[kind] += 1
            try:
                result = owner.merchant(path, key=key, data=body, codes=(200, 201, 503))
                if result is not None:
                    return result
            except (TimeoutError, RemoteDisconnected, URLError):
                pass
            if monotonic() >= deadline:
                break
            sleep(0.15 * 2 ** attempt)
        raise AssertionError("Unable to resolve original idempotent " + kind + " request")

    def create(o):
        result = replayable_post(o.owner, "/v2/payment-intents", o.key, o.body, "create")
        o.owner.bound(result, o.route, o.method)
        return result

    def present(o):
        path = "/v2/payment-intents/" + o.original["id"]
        pending = wait("routed presentment", lambda: o.owner.at(path, "pending"))
        o.owner.bound(pending, o.route, o.method)
        o.owner.decide({"id": o.original["id"], "route": o.route, "body": o.body, "pending": pending})

    def capture(o):
        path = "/v2/payment-intents/" + o.original["id"]
        captured = wait("original capture", lambda: o.owner.at(path, "captured"))
        o.owner.bound(captured, o.route, o.method)
        require(captured["amount_minor"] == o.body["amount_minor"], "Captured amount differs from order")
        if o.method in ("visa", "amex", "mastercard"):
            require(captured["card_brand"] == o.method, "Settled card differs from selection")
        o.original = captured

    selected = orders[:8]  # Two independent original refunds at each provider.

    def request_refund(o):
        key = "refund-" + o.tag
        path = "/v2/payment-intents/" + o.original["id"] + "/refunds"
        return replayable_post(o.owner, path, key, {"merchant_refund_id": key, "amount_minor": 25}, "refund")

    def observe_refund(o):
        path = "/v2/refunds/" + o.refund["id"]
        o.refund = wait("linked refund", lambda: o.owner.at(path, "refunded"))
        o.owner.bound(o.refund, o.route, o.method)
        require(o.refund["parent_id"] == o.original["id"] and o.refund["amount_minor"] == 25,
                "Incorrect routed refund reference or amount")

    start = perf_counter()
    with ThreadPoolExecutor(max_workers=8) as pool:
        duplicates = [o for o in orders for _ in range(2)]
        creates = list(pool.map(create, duplicates))
        for index, o in enumerate(orders):
            first, second = creates[index*2:index*2+2]
            require(first["id"] == second["id"], "Duplicate admission at one merchant")
            o.original = first
        list(pool.map(present, orders))
        list(pool.map(capture, orders))
        results = list(pool.map(request_refund, [o for o in selected for _ in range(2)]))
        for index, o in enumerate(selected):
            first, second = results[index*2:index*2+2]
            require(first["id"] == second["id"], "Duplicate refund admitted")
            o.refund = first
        list(pool.map(observe_refund, selected))

    seen = set()
    for route in test.providers:
        expected = [(o.original["id"], o.original["reference"], "capture", o.body["amount_minor"])
                    for o in orders if o.route == route]
        expected += [(o.refund["id"], o.refund["reference"], "refund", -25)
                     for o in selected if o.route == route]
        require(len(expected) == 10 and test.records(route) == sorted(expected),
                "Independent provider has missing, duplicate or cross-routed movements")
        for op, _, _, _ in expected:
            require(op not in seen, "One operation ID occurs on two providers")
            seen.add(op)
    summaries = {}
    for who in (a, b):
        expected = Counter()
        for o in orders:
            if o.owner is who:
                expected[o.route, "capture", o.original["reference"], o.body["amount_minor"]] += 1
                if o.refund:
                    expected[o.route, "refund", o.refund["reference"], -25] += 1
        journal = who.merchant("/v1/journal?limit=250")["entries"]
        observed = Counter()
        for e in journal:
            require(e["merchant_id"] == who.m["merchant"] and e["processor_account"] == who.m["account"],
                    "Ledger contains foreign settlement authority")
            require(sum(l["debit_minor"] - l["credit_minor"] for l in e["legs"]) == 0, "Unbalanced entry")
            clearing = sum(l["debit_minor"] - l["credit_minor"] for l in e["legs"] if l["account"] == "processor_clearing")
            observed[e["route_id"], e["kind"], e["reference"], clearing] += 1
        require(observed == expected, "Ledger movements differ from original instructions")
        summaries[who.m["merchant"]] = sum(amt * count for (_, _, _, amt), count in observed.items())
    print(json.dumps({"result":"pass","clients":8,"routes":4,"create_requests":64+retries["create"],"approvals":64,"captures":32,
                      "additional_transport_retries":dict(retries),"refund_requests":16+retries["refund"],"refunds":8,"provider_movements":40,"merchant_net_minor":summaries,
                      "traced_wall_seconds":round(perf_counter()-start,3)}, sort_keys=True), flush=True)
