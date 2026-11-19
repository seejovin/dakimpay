"""Receiptless return through the independent, disposable merchant and acquirer."""
import csv
import io
import os
from pathlib import Path
import subprocess
import uuid


def retail_sql(merchant_dir, pg_port):
    pg_bin = os.environ.get("MIMI_PG_BIN") or Path.home() / ".local/share/mimi/pg18/bin"
    if not (Path(pg_bin) / "psql").is_file():
        pg_bin = subprocess.check_output(["pg_config", "--bindir"], text=True).strip()

    def sql(query, **values):
        args = [Path(pg_bin) / "psql", "-XqAt", "-v", "ON_ERROR_STOP=1",
                "-h", merchant_dir / "run", "-p", str(pg_port),
                "-U", "mimi_installer", "-d", "mimi"]
        for name, value in values.items():
            args.extend(["-v", f"{name}={value}"])
        return subprocess.run(args, input="SET search_path=retail,public;\n" + query,
                              capture_output=True, check=True, text=True).stdout.strip()

    assert sql("SELECT training FROM store_config") == "t"
    return sql


def verify_reversal(cash, manager, finance, owner, pay, merchant_dir, pg_port, until, restart_payment_services):
    sql = retail_sql(merchant_dir, pg_port)
    cash.cmd("commerce.open", {})
    def basket():
        return cash.req("/api/state?view=register")["commerce"]["basket"]
    original = basket()
    cash.cmd("commerce.add", {"basketId": original["id"], "version": original["version"], "code": "EGGS"})
    order = basket()
    assert order["totalMinor"] == 360
    # Delay only this disposable till's next inspection to order the race. Both
    # refunds and all other financial facts below go through the applications.
    sql("""
        CREATE FUNCTION delay_test_inspection() RETURNS trigger LANGUAGE plpgsql AS $body$
        BEGIN NEW.next_check := now() + interval '1 hour'; RETURN NEW; END $body$;
        CREATE TRIGGER delay_test_inspection BEFORE UPDATE OF next_check ON card_attempt
        FOR EACH ROW WHEN (NEW.basket_id = :'basket' AND NEW.stage = 'inspect')
        EXECUTE FUNCTION delay_test_inspection();
    """, basket=order["id"])
    attempt = cash.cmd("commerce.card.start", {
        "basketId": order["id"], "version": order["version"],
        "quoteKey": order["quoteKey"], "expectedTotalMinor": 360,
    })["id"]
    def parked():
        return sql("""SELECT remote_id FROM card_attempt WHERE id=:'attempt'
                      AND stage='inspect' AND lease='' AND next_check > now()+interval '30 minutes'""",
                   attempt=attempt)
    payment = until(parked)
    path = "/v1/payments/" + payment
    assert pay.req(path + "/recover", {})["status"] == "captured"
    def refund(amount):
        key = str(uuid.uuid4())
        result = pay.req(path + "/refunds", {"amount": amount}, key)
        assert result["status"] == "succeeded"
        assert pay.req(path + "/refunds", {"amount": amount}, key)["id"] == result["id"]
        return result["id"]

    first = refund(120)
    lot = sql("SELECT lot_id FROM lot_claim WHERE basket_id=:'basket'", basket=order["id"])
    def quarantined():
        return int(sql("""SELECT coalesce(sum(qty_milli),0) FROM lot_balance
                          WHERE lot_id=:'lot'::bigint AND zone='quarantine'""", lot=lot))
    before = quarantined()
    sql("""DROP TRIGGER delay_test_inspection ON card_attempt;
           DROP FUNCTION delay_test_inspection();
           UPDATE card_attempt SET next_check=now() WHERE id=:'attempt' AND stage='inspect';""",
        attempt=attempt)
    def phase():
        state = finance.req("/api/state")
        return next(row.get("reversalState") for row in state["card"]["attempts"] if row["id"] == attempt)
    def receipt_count():
        return sql("SELECT count(*) FROM register_receipt WHERE basket_id=:'basket'", basket=order["id"])
    def decision(actor, action, evidence):
        return actor.cmd("commerce.card.reversal" + action, {"id": attempt, "evidence": evidence})

    until(lambda: phase() == "custody")
    assert receipt_count() == "0"
    decision(owner, "Custody", "Counted one carton at the lane and retained it for inspection.")
    owner.req("/api/command", {"action": "commerce.card.reversalApprove",
              "input": {"id": attempt, "evidence": "Checking review separation."}},
              str(uuid.uuid4()), expect_error=403)
    decision(finance, "Approve", "Check the original capture and first refund.")
    until(lambda: phase() == "finance")
    assert receipt_count() == "0" and quarantined() == before
    assert sql("SELECT count(*) FROM lot_claim WHERE basket_id=:'basket'", basket=order["id"]) == "1"
    restart_payment_services()
    assert phase() == "finance" and pay.req(path)["refunded"] == 120
    second = refund(240)
    pay.req(path + "/refunds", {"amount": 1}, str(uuid.uuid4()), expect_error=409)
    decision(finance, "Approve", "Both original refund references are available for verification.")
    until(lambda: phase() == "released")
    assert receipt_count() == "0" and quarantined() == before + 1000
    assert sql("SELECT count(*) FROM lot_claim WHERE basket_id=:'basket'", basket=order["id"]) == "0"

    payout = pay.req("/v1/settle", {}, str(uuid.uuid4()))
    assert payout["net"] == -7 and payout["status"] == "in_transit", payout
    export = "/v1/payouts/" + payout["id"] + "/statement"
    pay.req(export, expect_error=409)
    pay.req("/app/bank-post/" + payout["bank_trace"], {})
    assert pay.req("/v1/reconcile", {})["matched"] == 1
    statement = pay.req(export)
    lines = [(leg["reference"], leg["grossMinor"], leg["feeMinor"]) for leg in statement["lines"]]
    assert statement["depositMinor"] == -7 and sorted(lines) == sorted([
        (payment, 360, 7), (first, -120, 0), (second, -240, 0)]), statement
    batch = finance.cmd("finance.settlement.import", statement)["id"]
    manager.cmd("finance.settlement.close", {"id": batch})
    assert sql("SELECT count(*) FROM external_reversal_applied WHERE batch_id=:'batch'", batch=batch) == "3"
    with finance.http.open(finance.origin + "/api/export/journal.csv") as ledger:
        rows = list(csv.DictReader(io.StringIO(ledger.read().decode("utf-8-sig"))))
    entries = sorted((r["account"], int(r["amount_minor"])) for r in rows
                     if (r["source"], r["source_id"], r["currency"]) == ("settlement_decision", batch, "USD"))
    assert entries == [("bank", -7), ("processing_fees", 7)], entries
    assert pay.req("/v1/reconcile", {})["matched"] == 0
    print("PASS unreceipted 3.60 capture; partial 1.20 held through restart; remaining 2.40 proved; "
          "two reviewers, original lot quarantined, three matched legs, bank -0.07 and fees +0.07")
