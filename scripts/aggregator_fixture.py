"""The four independent synthetic provider enrollments used by the local launcher."""
import hashlib
import json
import secrets

CARD = ("card", "visa", "mastercard", "amex")
PROVIDERS = {
    "card-primary": (CARD, ("hosted", "terminal"), 10),
    "card-secondary": (CARD, ("hosted",), 20),
    "wechat": (("wechat_pay",), ("hosted",), 10),
    "alipay": (("alipay",), ("hosted",), 10),
}
PORTS = (("card-primary", "DAKIM_CARD_PRIMARY_PORT", 8481),
         ("card-secondary", "DAKIM_CARD_SECONDARY_PORT", 8482),
         ("wechat", "DAKIM_WECHAT_PORT", 8483), ("alipay", "DAKIM_ALIPAY_PORT", 8484))


def new_secrets(merchants, authorities):
    return {"admin": secrets.token_hex(32),
            "db": {k: secrets.token_hex(32) for k in (*PROVIDERS, "gateway")},
            "connectors": {p: secrets.token_urlsafe(36) for p in PROVIDERS},
            "merchants": {
                m: {"authorities": {role: secrets.token_urlsafe(36) for role in authorities},
                    "providers": {p: {name: secrets.token_urlsafe(36) for name in ("export", "device", "import")}
                                  for p in PROVIDERS}}
                for m in merchants}}


def add_partner_routes(gateway, state, require_private):
    path = state / "partner-routes.json"
    if not path.exists():
        return
    require_private(path)
    if path.stat().st_size > 1 << 20:
        raise ValueError("partner-routes.json is too large")

    def strict_object(pairs):
        result = {}
        for name, value in pairs:
            if name in result:
                raise ValueError("duplicate field in partner-routes.json")
            result[name] = value
        return result

    cfg = json.loads(path.read_text(), object_pairs_hook=strict_object)
    if not isinstance(cfg, dict) or set(cfg) != {"connectors", "routes"}:
        raise ValueError("partner-routes.json must contain connectors and routes")
    connectors, routes = cfg["connectors"], cfg["routes"]
    if not isinstance(connectors, list) or not isinstance(routes, list) or not connectors or not routes:
        raise ValueError("partner connectors and enrollments are required")
    ids = set()
    for item in connectors:
        if (not isinstance(item, dict) or set(item) != {"id", "kind", "config_file"}
                or item["kind"] not in ("stripe_test", "antom_test") or not isinstance(item["id"], str)
                or item["id"] in ids or item["id"] in PROVIDERS):
            raise ValueError("invalid external test connector")
        ids.add(item["id"])
    if any(not isinstance(r, dict) or r.get("connector") not in ids for r in routes):
        raise ValueError("partner routes must use their own test connectors")
    gateway["connectors"].extend(connectors)
    gateway["routes"].extend(routes)


def configure(settings, state, keys, write_private, require_private, url):
    # This owner-controlled switch affects only new routing decisions. Existing
    # resources remain attached to their original immutable provider grant.
    disabled = set()
    overrides = state / "disabled-routes.json"
    if overrides.exists():
        require_private(overrides)
        data = json.loads(overrides.read_text())
        if (not isinstance(data, list) or not all(isinstance(item, str) and item in PROVIDERS for item in data)
                or len(set(data)) != len(data)):
            raise ValueError("disabled-routes.json must be a list of distinct known route IDs")
        disabled = set(data)
    gateway = {"origin": url(settings, "gateway"), "programs": [], "credentials": [], "connectors": [], "routes": []}
    subs = state / "subscriptions.json"
    if subs.exists():
        require_private(subs)
        gateway["subscriptions"] = json.loads(subs.read_text())
    public = {"gateway_url": gateway["origin"], "rail_url": url(settings, "card-primary"),
              "merchants": [], "providers": {}}
    rails = {}
    for pid in PROVIDERS:
        origin = url(settings, pid)
        key = keys["connectors"][pid]
        gateway["connectors"].append({"id": pid, "origin": origin, "token": key})
        rails[pid] = {"origin": origin, "rail_token": key, "programs": []}
        public["providers"][pid] = {"origin": origin, "merchants": {}}
    for index, merchant in enumerate(("mimi", "market-two"), start=1):
        m = keys["merchants"][merchant]
        acct, bank, terminal = "acct-" + merchant, "bank-" + merchant, f"terminal-{index}"
        program = {"merchant": merchant, "account": acct, "currency": "USD", "max_amount": 1000000,
                   "beneficiary": bank, "outlets": ["store-1"]}
        gateway["programs"].append({**program, "terminals": {terminal: "store-1"}})
        for role, secret in m["authorities"].items():
            gateway["credentials"].append({"merchant": merchant, "role": role, "subject": merchant + "-" + role,
                                          "sha256": hashlib.sha256(secret.encode()).hexdigest()})
        for pid, (methods, channels, priority) in PROVIDERS.items():
            downstream = pid + "-" + merchant
            creds = m["providers"][pid]
            route = {"id": pid, "merchant": merchant, "account": acct, "connector": pid,
                     "provider_account": downstream, "currency": "USD", "methods": methods,
                     "channels": channels, "min_amount_minor": 1, "max_amount_minor": program["max_amount"],
                     "priority": priority, "enabled": pid not in disabled}
            gateway["routes"].append(route)
            gateway["credentials"].append({"merchant": merchant, "role": "statement",
                                          "subject": merchant + "-" + pid + "-import",
                                          "route_id": pid, "provider_account": downstream,
                                          "sha256": hashlib.sha256(creds["import"].encode()).hexdigest()})
            terminals = []
            if "terminal" in channels:
                terminals.append({"id": terminal, "outlet": "store-1", "token": creds["device"]})
            rails[pid]["programs"].append({**program, "route_id": pid, "connector_id": pid,
                                           "provider_account": downstream, "payment_methods": methods,
                                           "statement_token": creds["export"], "terminals": terminals})
            public["providers"][pid]["merchants"][merchant] = {
                "provider_account": downstream, "statement_token": creds["export"], "import_token": creds["import"]}
        public["merchants"].append({"merchant": merchant, "account": acct, "beneficiary": bank, "currency": "USD",
                                    "outlet": "store-1", "terminal": terminal, "tokens": m["authorities"],
                                    "device_token": m["providers"]["card-primary"]["device"],
                                    "rail_statement_token": m["providers"]["card-primary"]["export"]})
    add_partner_routes(gateway, state, require_private)
    for name, value in {**rails, "gateway": gateway, "clients": public, "binding": settings}.items():
        write_private(state / (name + ".json"), json.dumps(value, indent=2) + "\n")
