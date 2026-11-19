"""Crash a disposable payment process between provider capture and local commit."""
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
from datetime import datetime, timezone
from http.client import RemoteDisconnected
import hashlib
import hmac
import json
import os
from pathlib import Path
import signal
import subprocess
import time
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen
import uuid

from mimi_reversal_check import retail_sql


def wait_for(description, check, seconds=65):
    end = time.monotonic() + seconds
    while time.monotonic() < end:
        result = check()
        if result:
            return result
        time.sleep(0.15)
    raise AssertionError("Timed out awaiting " + description)


class Database:
    def __init__(self, env, role):
        root = Path(env["DAKIM_STATE_DIR"])
        pg = Path(env.get("DAKIM_PG_BIN", Path.home() / ".local/share/mimi/pg18/bin"))
        if not (pg / "psql").exists():
            pg = Path(subprocess.check_output(["pg_config", "--bindir"], text=True).strip())
        self.args = [pg / "psql", "-XqAt", "-v", "ON_ERROR_STOP=1", "-h", root / "run",
                     "-p", env["DAKIM_PG_PORT"], "-U", "dakim_" + role, "-d", "dakim_" + role]

    def query(self, statement, **variables):
        args = list(self.args)
        for key, value in variables.items():
            args.extend(["-v", f"{key}={value}"])
        result = subprocess.run(args, input=statement, text=True, capture_output=True)
        if result.returncode:
            raise AssertionError(result.stderr)
        return result.stdout.strip()


@contextmanager
def hold_capture_commit(db, payment):
    # Only a verified capture UPDATE for this operation can reach this barrier.
    db.query("""
        CREATE FUNCTION pay.test_park_capture() RETURNS trigger LANGUAGE plpgsql AS $$
        BEGIN PERFORM pg_advisory_xact_lock(4538102); RETURN NEW; END $$;
        CREATE TRIGGER test_park_capture BEFORE UPDATE OF status ON pay.payments
        FOR EACH ROW WHEN (NEW.id=:'id' AND NEW.status='captured')
        EXECUTE FUNCTION pay.test_park_capture();
    """, id=payment)
    with subprocess.Popen(db.args, stdin=subprocess.PIPE, stdout=subprocess.DEVNULL,
                          text=True) as holder:
        try:
            holder.stdin.write("SET application_name='hosted_crash_barrier';\nSELECT pg_advisory_lock(4538102);\n")
            holder.stdin.flush()
            wait_for("private capture barrier", lambda: db.query("""
                SELECT 1 FROM pg_locks l JOIN pg_stat_activity a USING(pid)
                WHERE a.application_name='hosted_crash_barrier' AND l.locktype='advisory'
                AND l.objid=4538102 AND l.granted"""))
            yield
        finally:
            holder.stdin.close()
            holder.wait(timeout=10)
            db.query("DROP TRIGGER test_park_capture ON pay.payments; DROP FUNCTION pay.test_park_capture();")


def verify_hosted(cash, pay, env, repo, restart_apps):
    state = Path(env["DAKIM_STATE_DIR"])
    source, local = Database(env, "acquirer"), Database(env, "pay")
    store = retail_sql(Path(env["MIMI_LOCAL_DIR"]), env["MIMI_PG_PORT"])
    acquirer = "http://127.0.0.1:" + env["DAKIM_ACQUIRER_PORT"]
    # Reschedule only transport. The provider still writes genuine event bytes,
    # captures, fees and closures through its ordinary hosted checkout endpoints.
    source.query("""
        CREATE FUNCTION test_hold_hosted_events() RETURNS trigger LANGUAGE plpgsql AS $$
        BEGIN NEW.next_attempt_at := now()+interval '1 hour'; RETURN NEW; END $$;
        CREATE TRIGGER test_hold_hosted_events BEFORE INSERT ON sim_callback_outbox
        FOR EACH ROW WHEN (NEW.kind IN ('payment.awaiting_customer','payment.captured','payment.expired'))
        EXECUTE FUNCTION test_hold_hosted_events();
    """)

    def start():
        cash.cmd("commerce.open", {})
        def basket():
            return cash.req("/api/state?view=register")["commerce"]["basket"]
        b = basket()
        cash.cmd("commerce.add", {"basketId": b["id"], "version": b["version"], "code": "EGGS"})
        b = basket()
        assert b["totalMinor"] == 360, b
        details = {"basketId": b["id"], "version": b["version"], "quoteKey": b["quoteKey"],
                   "expectedTotalMinor": b["totalMinor"]}
        key = str(uuid.uuid4())
        attempt = cash.cmd("commerce.card.startHosted", details, key)["id"]
        def pending():
            return next((p for p in pay.req("/v1/dashboard")["payments"] if
                         p["order_id"] == "MIMI-" + attempt and p["status"] == "awaiting_customer"), None)
        payment = wait_for("original hosted payment", pending)
        def merchant_checkout():
            try:
                with cash.http.open(cash.origin + "/api/card/" + attempt + "/checkout", timeout=5) as page:
                    return page.geturl() if b"Approve simulated payment" in page.read() else None
            except HTTPError as error:
                if error.code != 409:
                    raise
        link = wait_for("Mimi's original hosted checkout link", merchant_checkout)
        assert link == payment["checkout_url"] and link.startswith(acquirer + "/checkout/")
        return attempt, b["id"], details, key, payment

    def approve(link):
        req = Request(link, data=b"", headers={"Origin": acquirer})
        with urlopen(req, timeout=6) as response:
            return response.read()

    def inquiry(pid):
        service = (state / "service_token.key").read_text().strip()
        req = Request(acquirer + "/sim/charges/" + pid, headers={"Authorization":"Bearer " + service})
        with urlopen(req, timeout=5) as response:
            return json.load(response)

    def counts(attempt, basket, payment):
        return {
            "provider_operations": int(source.query("SELECT count(*) FROM sim_charge WHERE order_id=:'order'", order="MIMI-"+attempt)),
            "provider_capture_postings": int(source.query("""SELECT count(*) FROM sim_line
                WHERE kind='capture' AND reference=:'ref' AND amount=360""", ref=payment["reference"])),
            "payment_capture_events": int(local.query("SELECT count(*) FROM pay.events WHERE subject=:'id' AND kind='captured'", id=payment["id"])),
            "merchant_attempts": int(store("SELECT count(*) FROM card_attempt WHERE basket_id=:'basket'", basket=basket)),
            "merchant_receipts": int(store("SELECT count(*) FROM register_receipt WHERE basket_id=:'basket'", basket=basket)),
            "merchant_card_postings": int(store("""SELECT count(*) FROM tender_journal j JOIN register_receipt r ON r.id=j.source_id
                WHERE r.basket_id=:'basket' AND j.source='sale' AND j.account='card_clearing' AND j.amount_minor=360""", basket=basket)),
            "eligible_fulfilment_signals": int(store("SELECT count(*) FROM hosted_fulfilment_signal WHERE attempt_id=:'id'", id=attempt)),
        }

    def delayed_event(pid, kind):
        # Release the genuine sender first, then replay exactly its signed bytes,
        # as a transport could after losing its acknowledgement. No financial fixture writes.
        event = source.query("""UPDATE sim_callback_outbox SET next_attempt_at=now()
            WHERE operation_id=:'id' AND kind=:'kind' RETURNING event_id""", id=pid, kind=kind)
        assert event
        wait_for("acquirer delivery of " + kind, lambda: source.query("""SELECT 1 FROM sim_callback_outbox
            WHERE event_id=:'evt' AND acknowledged_at IS NOT NULL""", evt=event))
        raw = bytes.fromhex(source.query("SELECT encode(body,'hex') FROM sim_callback_outbox WHERE event_id=:'evt'", evt=event))
        key = (state / "callback_key.key").read_bytes().strip()
        def redeliver(_):
            ts = str(int(time.time()))
            sig = hmac.new(key, b"dakim.callback.v1." + ts.encode() + b"." + raw, hashlib.sha256).hexdigest()
            req = Request(pay.origin + "/callbacks/processor", data=raw, headers={"Content-Type": "application/json",
                          "Dakim-Event-Timestamp": ts, "Dakim-Event-Signature": "v1=" + sig})
            with urlopen(req, timeout=5) as res:
                assert res.status == 200 and json.load(res) == {"event_id":event}
        with ThreadPoolExecutor(max_workers=3) as pool:
            list(pool.map(redeliver, range(3)))
        wait_for("verified inbox record for " + kind, lambda: local.query("""SELECT 1 FROM pay.processor_callbacks
            WHERE operation_id=:'id' AND event_id=:'evt' AND status='applied'""", id=pid, evt=event))
        return {"type":kind, "event":event, "duplicate_acknowledgements":3}

    attempt, basket, details, key, p = start()
    pid, ref = p["id"], p["reference"]
    path = "/v1/payments/" + pid
    # A second key for the same merchant order cannot create another charge.
    replay = {k:p[k] for k in ("order_id","channel","brand","amount","currency")}
    pay.req("/v1/payments", replay, str(uuid.uuid4()), expect_error=409)
    assert cash.cmd("commerce.card.startHosted", details, key)["id"] == attempt
    with hold_capture_commit(local, pid):
        source.query("UPDATE sim_charge SET checkout_expires_at=clock_timestamp()+interval '4 seconds' WHERE operation_id=:'id'", id=pid)
        assert b"Payment approved" in approve(p["checkout_url"])
        paid = inquiry(pid)
        assert paid["status"] == "captured" and paid["reference"] == ref
        def recover_during_crash():
            try:
                pay.req(path + "/recover", {})
            except (RemoteDisconnected, URLError, ConnectionError):
                return
            raise AssertionError("Parked capture returned before process interruption")
        with ThreadPoolExecutor(max_workers=1) as pool:
            request = pool.submit(recover_during_crash)
            wait_for("dakim pay blocked inside its capture write", lambda: local.query("""
                SELECT 1 FROM pg_stat_activity a
                WHERE a.datname=current_database() AND a.wait_event_type='Lock'
                AND a.query ILIKE '%UPDATE pay.payments SET status%'
                AND EXISTS(SELECT FROM pg_stat_activity h WHERE h.application_name='hosted_crash_barrier'
                           AND h.pid=ANY(pg_blocking_pids(a.pid)))"""))
            os_pid = int((state/"pay.pid").read_text())
            command = subprocess.check_output(["ps","-p",str(os_pid),"-o","command="], text=True).strip()
            assert command == f"{state}/dakimpay pay", command
            os.kill(os_pid, signal.SIGKILL)
            request.result(timeout=10)
        wait_for("checkout deadline while payment process is stopped", lambda: source.query(
            "SELECT 1 FROM sim_charge WHERE operation_id=:'id' AND captured_at<checkout_expires_at AND checkout_expires_at<=clock_timestamp()", id=pid))
        assert local.query("SELECT status FROM pay.payments WHERE id=:'id'", id=pid) == "awaiting_customer"
        before = counts(attempt, basket, p)
        assert list(before.values()) == [1,1,0,1,0,0,0], before
        cash.req("/api/command", {"action":"commerce.card.startHosted","input":details}, str(uuid.uuid4()), expect_error=409)
        assert inquiry(pid)["status"] == "captured"
    subprocess.run([repo/"scripts/local", "start"], env=env, check=True, stdout=subprocess.DEVNULL)

    def fulfilled():
        row = cash.req("/api/card/" + attempt)
        return row if row.get("fulfilmentEligible") and row.get("status") == "succeeded" else None
    wait_for("Mimi's recovery and fulfilment decision with all notifications withheld", fulfilled, 95)
    def receipt_id():
        return store("SELECT receipt_id FROM hosted_fulfilment_signal WHERE attempt_id=:'id'", id=attempt)
    receipt = receipt_id()
    assert receipt
    assert local.query("SELECT count(*) FROM pay.processor_callbacks WHERE operation_id=:'id'", id=pid) == "0"
    recovered = pay.req(path)
    assert (recovered["id"], recovered["reference"], recovered["status"]) == (pid,ref,"captured")
    first_counts = counts(attempt, basket, p)
    assert all(n == 1 for n in first_counts.values()), first_counts
    assert store("SELECT sum(amount_minor) FROM tender_journal WHERE source='sale' AND source_id=:'rid'", rid=receipt) == "0"
    events = [delayed_event(pid, kind) for kind in ("payment.captured","payment.awaiting_customer")]
    assert b"Payment approved" in approve(p["checkout_url"])
    restart_apps()
    assert cash.cmd("commerce.card.startHosted",details,key)["id"] == attempt
    assert receipt_id() == receipt
    assert fulfilled() and counts(attempt, basket, p) == first_counts
    assert local.query("SELECT count(*) FROM pay.processor_callbacks WHERE operation_id=:'id'", id=pid) == "2"

    # Exercise the other side of the deadline on an independent unpaid basket.
    unpaid, unpaid_basket, _, _, q = start()
    qid = q["id"]
    source.query("UPDATE sim_charge SET checkout_expires_at=clock_timestamp() WHERE operation_id=:'id'", id=qid)
    closed = inquiry(qid)
    assert closed["status"] == "expired" and closed["reference"] == q["reference"]
    try:
        body = approve(q["checkout_url"])
        assert b"expired" in body.lower() and b"Payment approved" not in body
    except HTTPError as error:
        assert error.code in (409,410), error.code
    assert pay.req("/v1/payments/"+qid+"/recover", {})["status"] == "expired"
    wait_for("Mimi's verified unpaid closure", lambda: cash.req("/api/card/"+unpaid)["status"] == "declined")
    unpaid_events = [delayed_event(qid, kind) for kind in ("payment.expired","payment.awaiting_customer")]
    assert not cash.req("/api/card/"+unpaid)["fulfilmentEligible"]
    none = counts(unpaid, unpaid_basket, q)
    assert list(none.values()) == [1,0,0,1,0,0,0], none
    assert store("SELECT status FROM sale_basket WHERE id=:'id'", id=unpaid_basket) == "open"
    timing = json.loads(source.query("""SELECT json_build_object('captured_at',captured_at,
        'checkout_deadline',checkout_expires_at) FROM sim_charge WHERE operation_id=:'id'""", id=pid))
    evidence = {"environment":"disposable local online checkout simulation", "verified_at":datetime.now(timezone.utc).isoformat(),
                "currency":"USD","amount_minor":360,"provider_reference":ref,"payment":pid,"merchant_attempt":attempt,
                "receipt":receipt,**timing,"after_process_kill_before_restart":before,"after_recovery_and_replay":first_counts,
                "callbacks_received_at_first_fulfilment":0,"delivery_order":events,"unpaid_control":{
                    "payment":qid,"provider_reference":q["reference"],"provider_status":closed["status"],
                    "basket_status":"open","delivery_order":unpaid_events,"counts":none}}
    if target := env.get("DAKIM_HOSTED_EVIDENCE_FILE"):
        Path(target).write_text(json.dumps(evidence, indent=2) + "\n")
    print("PASS provider accepted hosted $3.60; SIGKILL during local capture write; original inquiry after expiry released one receipt, one card posting and one eligible fulfilment signal with callbacks withheld")
    print("PASS late capture and earlier checkout events replayed three times each; restart preserved all counts; verified unpaid expiry reopened its basket with zero captures, receipts or fulfilment signals")
