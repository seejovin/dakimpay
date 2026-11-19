"""A refund originating at the simulated acquirer after the first deposit."""
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import subprocess
from urllib.error import HTTPError
from urllib.request import Request, urlopen
import uuid

from mimi_reversal_check import retail_sql


CATEGORY = "merchant_funded_service_concession"
PROPOSE = "finance.settlement.classification.propose"
APPROVE = "finance.settlement.classification.approve"
RETURN = "finance.settlement.classification.return"


def proposal_input(issue, batch):
    return {"id": issue["id"], "batchId": batch, "category": CATEGORY,
            "reason": "Mimi agreed to a partial service concession following a documented delivery delay.",
            "evidenceRef": "MIMI-SYNTHETIC-CASE-" + issue["id"]}


def rejected(session, action, body, status):
    return session.req("/api/command", {"action": action, "input": body},
                       str(uuid.uuid4()), expect_error=status)


def table_fingerprints(query, tables):
    # Identifiers come exclusively from the fixed lists below. Include every
    # column and sort whole rows, so counts alone cannot mask an edited amount.
    selects = [f"""SELECT '{table}', count(*)::text,
        encode(sha256(convert_to(coalesce(jsonb_agg(to_jsonb(r) ORDER BY to_jsonb(r)::text)::text, '[]'), 'UTF8')), 'hex')
        FROM {table} r""" for table in tables]
    statement = "BEGIN READ ONLY;\n" + " UNION ALL ".join(selects) + " ORDER BY 1;\nCOMMIT;"
    lines = query(statement).splitlines()
    return {name: {"rows": int(count), "sha256": digest}
            for name, count, digest in (line.split("|") for line in lines)}


def simulator_reader(env, role):
    state = Path(env["DAKIM_STATE_DIR"])
    pg = Path(env.get("DAKIM_PG_BIN", Path.home() / ".local/share/mimi/pg18/bin"))
    if not (pg / "psql").is_file():
        pg = Path(subprocess.check_output(["pg_config", "--bindir"], text=True).strip())
    database = "dakim_" + role
    args = [pg / "psql", "-XqAt", "-v", "ON_ERROR_STOP=1", "-h", state / "run",
            "-p", str(env["DAKIM_PG_PORT"]), "-U", database, "-d", database]

    def query(statement):
        result = subprocess.run(args, input=statement, text=True, capture_output=True,
                                env={**env, "PGOPTIONS": "-c default_transaction_read_only=on"})
        if result.returncode:
            raise AssertionError(result.stderr)
        return result.stdout.strip()
    return query


def financial_snapshot(env, sql):
    return {
        "mimi": table_fingerprints(sql, [
            "sale_basket", "basket_item", "card_attempt", "register_receipt", "register_refund",
            "stock", "stock_move", "lot_balance", "credit_entry", "settlement_batch", "settlement_line",
            "settlement_journal", "tender_journal", "provider_refund_notice", "provider_refund_applied"]),
        "dakim_pay": table_fingerprints(simulator_reader(env, "pay"), [
            "pay.payments", "pay.refunds", "pay.lines", "pay.payouts", "pay.settlement_requests"]),
        "acquirer": table_fingerprints(simulator_reader(env, "acquirer"), [
            "sim_charge", "sim_refund", "sim_refund_decline", "sim_line", "sim_payout"]),
        "bank": table_fingerprints(simulator_reader(env, "bank"), ["sim_bank_entry"]),
    }


def classification_browser(stage, env, repo, issue):
    if env.get("DAKIM_CLASSIFICATION_BROWSER") == "1":
        mimi = Path(env.get("MIMI_SOURCE", repo.parent / "mimi"))
        subprocess.run(["node", mimi / "webtest/refund-classification.cjs"], check=True,
                       env={**env, "MIMI_CLASSIFICATION_STAGE": stage,
                            "MIMI_REFUND_ADJUSTMENT": issue["id"],
                            "MIMI_UAT_URL": "http://" + env["MIMI_LISTEN"],
                            "MIMI_UAT_CREDENTIALS": str(Path(env["MIMI_LOCAL_DIR"]) / "first-sign-in.tsv")})


def verify_classification(finance, manager, pay, env, repo, restart_apps, sql, issue, batch, receipt):
    proposal = proposal_input(issue, batch)
    def case():
        rows = finance.req("/api/state")["settlement"]["providerRefunds"]
        matching = [r for r in rows if r["id"] == issue["id"]]
        assert len(matching) == 1, matching
        return matching[0]

    initial = case()
    assert (initial["status"], initial["classificationStatus"], initial["receiptId"], initial["debitBatchId"]) == (
        "bank_matched", "unclassified", receipt, batch)
    before = financial_snapshot(env, sql)
    classification_browser("eligible", env, repo, issue)
    proposal_key = str(uuid.uuid4())
    created = finance.cmd(PROPOSE, proposal, proposal_key)
    assert created["status"] == "pending_approval"
    decision = {"id": issue["id"], "batchId": batch, "proposalId": created["id"]}
    rejected(finance, APPROVE, decision, 403)
    rejected(manager, APPROVE, {**decision, "proposalId": str(uuid.uuid4())}, 409)
    rejected(finance, PROPOSE, {**proposal, "reason": "Attempted change to approved terms"}, 409)
    pending = case()
    assert pending["classificationStatus"] == "pending_approval" and pending["proposalId"] == created["id"]
    assert pending["reason"] == proposal["reason"] and pending["evidenceRef"] == proposal["evidenceRef"]
    restart_apps()
    assert case() == pending
    assert finance.cmd(PROPOSE, proposal, proposal_key)["id"] == created["id"]
    assert finance.cmd(PROPOSE, proposal)["id"] == created["id"]

    original_id = created["id"]
    def stored_version(table, column, identifier):
        return json.loads(sql(f"SELECT to_jsonb(r) FROM {table} r WHERE {column}=:'id'", id=identifier))
    original_row = stored_version("provider_refund_classification_proposal", "id", original_id)
    assert original_row["reason"] == proposal["reason"] and original_row["evidence_ref"] == proposal["evidenceRef"]

    send_back = {**decision, "reason": "The draft cites a delivery delay; the customer service log records missed assistance.",
                 "evidenceRef": "MIMI-REVIEW-LOG-" + issue["id"]}
    corrected = {**proposal, "supersedesId": original_id,
                 "reason": "Mimi approved a service concession for missed assistance documented by the store supervisor.",
                 "evidenceRef": "MIMI-SIGNED-SERVICE-LOG-" + issue["id"]}
    rejected(finance, RETURN, send_back, 403)
    rejected(finance, PROPOSE, corrected, 409)
    return_key = str(uuid.uuid4())
    return_receipt = manager.cmd(RETURN, send_back, return_key)
    assert return_receipt["id"] == original_id and return_receipt["status"] == "returned", return_receipt
    returned = case()
    assert returned["classificationStatus"] == "returned" and returned["proposalId"] == original_id
    assert returned["returnReason"] == send_back["reason"] and returned["returnEvidenceRef"] == send_back["evidenceRef"]
    assert returned["returnedBy"] and returned["returnedBy"] != returned["proposedBy"]
    assert returned["reason"] == proposal["reason"] and returned["evidenceRef"] == proposal["evidenceRef"]
    returned_row = stored_version("provider_refund_classification_return", "proposal_id", original_id)
    assert returned_row["returned_by"] == returned["returnedBy"] and returned_row["reason"] == send_back["reason"]
    assert returned_row["evidence_ref"] == send_back["evidenceRef"]
    restart_apps()
    assert case() == returned
    assert manager.cmd(RETURN, send_back, return_key) == return_receipt
    assert manager.cmd(RETURN, send_back) == return_receipt
    rejected(manager, RETURN, {**send_back, "reason": "Silently rewrite the prior review note."}, 409)
    rejected(manager, APPROVE, decision, 409)
    rejected(finance, PROPOSE, {**proposal, "supersedesId": original_id}, 409)
    rejected(finance, PROPOSE, {**corrected, "supersedesId": str(uuid.uuid4())}, 409)
    assert finance.cmd(PROPOSE, proposal, proposal_key) == created
    assert finance.cmd(PROPOSE, proposal) == return_receipt
    classification_browser("returned", env, repo, issue)

    # Two independent submissions of the corrected paper trail converge on the
    # same successor. A return is a terminal decision on the old version.
    correction_key = str(uuid.uuid4())
    with ThreadPoolExecutor(max_workers=2) as pool:
        replacements = list(pool.map(lambda key: finance.cmd(PROPOSE, corrected, key),
                                     [correction_key, str(uuid.uuid4())]))
    replacement = replacements[0]
    assert replacements[1] == replacement and replacement["status"] == "pending_approval", replacements
    effective_id = replacement["id"]
    assert effective_id != original_id
    latest = case()
    assert (latest["revision"], latest["supersedesId"], latest["proposalId"]) == (2, original_id, effective_id)
    assert latest["reason"] == corrected["reason"] and latest["evidenceRef"] == corrected["evidenceRef"]
    effective = {**decision, "proposalId": effective_id}
    rejected(finance, APPROVE, effective, 403)
    rejected(manager, APPROVE, decision, 409)
    rejected(finance, PROPOSE, {**corrected, "reason": "Fork the returned proposal into another correction."}, 409)

    approval_key = str(uuid.uuid4())
    # Mix transport retries (same key) with independent double-clicks (fresh keys).
    keys = [approval_key] * 3 + [str(uuid.uuid4()) for _ in range(3)]
    with ThreadPoolExecutor(max_workers=len(keys)) as pool:
        outcomes = list(pool.map(lambda key: manager.cmd(APPROVE, effective, key), keys))
    assert all(row["id"] == effective_id and row["status"] == "approved" for row in outcomes), outcomes
    approved = case()
    assert (approved["status"], approved["classificationStatus"], approved["proposalId"], approved["category"]) == (
        "bank_matched", "approved", effective_id, CATEGORY)
    assert approved["proposedBy"] and approved["approvedBy"] and approved["proposedBy"] != approved["approvedBy"]
    assert approved["proposedAt"] and approved["approvedAt"]
    assert approved["reason"] == corrected["reason"] and approved["evidenceRef"] == corrected["evidenceRef"]

    restart_apps()
    assert manager.cmd(APPROVE, effective, approval_key)["id"] == effective_id
    assert manager.cmd(APPROVE, effective)["id"] == effective_id
    assert manager.cmd(RETURN, send_back) == return_receipt
    assert finance.cmd(PROPOSE, corrected, correction_key) == replacement
    assert finance.cmd(PROPOSE, corrected)["id"] == effective_id
    rejected(manager, APPROVE, decision, 409)
    rejected(manager, RETURN, {**send_back, "proposalId": effective_id}, 409)
    assert case() == approved
    classification_browser("approved", env, repo, issue)
    assert original_row == stored_version("provider_refund_classification_proposal", "id", original_id)
    assert returned_row == stored_version("provider_refund_classification_return", "proposal_id", original_id)
    history = approved["classificationHistory"]
    assert [(row["revision"], row["proposalId"], row["classificationStatus"]) for row in history] == [
        (1, original_id, "returned"), (2, effective_id, "approved")], history
    assert (history[0]["reason"], history[0]["evidenceRef"], history[0]["returnReason"], history[0]["returnEvidenceRef"]) == (
        proposal["reason"], proposal["evidenceRef"], send_back["reason"], send_back["evidenceRef"])
    assert history[0]["returnedBy"] == returned["returnedBy"]
    # Go trims trailing fractional zeros. Python 3.9's %f accepts all one
    # through six digits present in a PostgreSQL microsecond timestamp.
    def instant(value):
        pattern = "%Y-%m-%dT%H:%M:%S.%f%z" if "." in value else "%Y-%m-%dT%H:%M:%S%z"
        return datetime.strptime(value, pattern)
    assert instant(history[0]["returnedAt"]) == instant(returned["returnedAt"]) == instant(returned_row["returned_at"]), (history[0], returned_row)
    assert instant(history[1]["approvedAt"]) == instant(approved["approvedAt"]), history[1]
    assert history[1]["supersedesId"] == original_id and history[1]["approvedBy"] == approved["approvedBy"]
    counts = sql("""WITH proposals AS (
        SELECT id FROM provider_refund_classification_proposal WHERE adjustment_id=:'adjustment' AND batch_id=:'batch')
        SELECT
        (SELECT count(*) FROM proposals),
        (SELECT count(*) FROM provider_refund_classification_return r JOIN proposals p ON p.id=r.proposal_id),
        (SELECT count(*) FROM provider_refund_classification_approval a JOIN proposals p ON p.id=a.proposal_id),
        (SELECT count(*) FROM provider_refund_classification_approval WHERE proposal_id=:'original'),
        (SELECT count(*) FROM provider_refund_applied WHERE adjustment_id=:'adjustment' AND batch_id=:'batch'),
        (SELECT count(*) FROM audit_log WHERE action=:'approval_action' AND outcome='ok' AND object_id IN (SELECT id FROM proposals)),
        (SELECT count(*) FROM audit_log WHERE action=:'return_action' AND outcome='ok' AND object_id=:'original')
        """, adjustment=issue["id"], batch=batch, original=original_id, approval_action=APPROVE, return_action=RETURN)
    assert counts == "2|1|1|0|1|1|1", counts
    assert pay.req("/v1/payments/" + issue["payment_id"])["refunded"] == issue["amount"]
    after = financial_snapshot(env, sql)
    assert before == after, {company: [table for table in before[company] if before[company][table] != now[table]]
                             for company, now in after.items() if before[company] != now}
    return {
        "scope": "reporting-only; no financial journal entry or payment instruction",
        "proposal": effective_id, "original_proposal": original_id, "adjustment": issue["id"], "debit_batch": batch,
        "category": CATEGORY, "reason": approved["reason"], "evidence_ref": approved["evidenceRef"],
        "proposed_by": approved["proposedBy"], "proposed_at": approved["proposedAt"],
        "approved_by": approved["approvedBy"], "approved_at": approved["approvedAt"],
        "classification_states": ["unclassified", "pending_approval", "returned", "pending_approval", "approved"],
        "classification_history": history,
        "self_return_http": 403, "stale_approval_http": 409, "approved_version_return_http": 409,
        "return_note_rewrite_http": 409, "uncorrected_resubmission_http": 409,
        "concurrent_correction_submissions": len(replacements), "successful_return_audit_entries": 1,
        "premature_proposal_http": 409, "self_approval_http": 403, "wrong_proposal_http": 409,
        "changed_proposal_terms_http": 409, "concurrent_approval_submissions": len(keys),
        "classification_proposals": 2, "classification_returns": 1, "classification_approvals": 1, "matched_debits": 1, "successful_approval_audit_entries": 1,
        "financial_fingerprints_before": before, "financial_fingerprints_after": after,
        "service_restarts_during_classification": 3,
    }


def verify_late_refund(cash, finance, manager, pay, env, repo, restart_apps, until):
    sql = retail_sql(Path(env["MIMI_LOCAL_DIR"]), env["MIMI_PG_PORT"])
    state = Path(env["DAKIM_STATE_DIR"])
    tool = state / "simulate-source-refund"
    subprocess.run(["go", "build", "-o", tool, "./cmd/simulate-source-refund"], cwd=repo, check=True)
    def bank_entry(trace):
        read_token = (state / "service_token.key").read_text().strip()
        req = Request(f"http://127.0.0.1:{env['DAKIM_BANK_PORT']}/bank/entries/{trace}",
                      headers={"Authorization": "Bearer " + read_token})
        try:
            with urlopen(req, timeout=5) as response:
                return json.load(response)
        except HTTPError as error:
            if error.code == 404:
                return None
            raise

    cash.cmd("commerce.open", {})
    def basket():
        return cash.req("/api/state?view=register")["commerce"]["basket"]
    b = basket()
    cash.cmd("commerce.add", {"basketId": b["id"], "version": b["version"], "code": "EGGS"})
    b = basket()
    assert b["totalMinor"] == 360
    attempt = cash.cmd("commerce.card.start", {"basketId": b["id"], "version": b["version"],
                       "quoteKey": b["quoteKey"], "expectedTotalMinor": 360})["id"]
    def receipt_id():
        return sql("SELECT receipt_id FROM card_attempt WHERE id=:'id' AND status='succeeded'", id=attempt)
    receipt = until(receipt_id)
    original = next(p for p in pay.req("/v1/dashboard")["payments"] if p["order_id"] == "MIMI-" + attempt)
    assert original["status"] == "captured" and original["amount"] == 360
    first = pay.req("/v1/settle", {}, str(uuid.uuid4()))
    assert first["net"] == 353 and first["status"] == "in_transit"
    pay.req("/app/bank-post/" + first["bank_trace"], {})
    assert pay.req("/v1/reconcile", {})["matched"] == 1
    first_export_path = "/v1/payouts/" + first["id"] + "/statement"
    first_export = pay.req(first_export_path)
    original_bank = bank_entry(first["bank_trace"])
    assert original_bank == {"trace": first["bank_trace"], "merchant_id": "mimi", "currency": "USD",
                             "amount": 353, "status": "posted"}
    first_batch = finance.cmd("finance.settlement.import", first_export)["id"]
    manager.cmd("finance.settlement.close", {"id": first_batch})

    def journal(batch):
        return sql("""SELECT account,amount_minor FROM settlement_journal
                      WHERE batch_id=:'id' ORDER BY account""", id=batch)
    def original_books():
        return sql("""SELECT to_jsonb(r), to_jsonb(b),
              (SELECT count(*) FROM register_refund WHERE receipt_id=r.id),
              (SELECT count(*) FROM stock_move),
              (SELECT jsonb_agg(to_jsonb(l) ORDER BY l.remote_id) FROM settlement_line l WHERE l.batch_id=b.id)
              FROM register_receipt r CROSS JOIN settlement_batch b
              WHERE r.id=:'receipt' AND b.id=:'batch'""", receipt=receipt, batch=first_batch)
    first_journal = "bank|353\ncard_clearing|-360\nprocessing_fees|7"
    assert journal(first_batch) == first_journal
    baseline = original_books()
    assert baseline and first_export["depositMinor"] == 353

    operation = "sr_" + uuid.uuid4().hex
    def source_refund():
        result = subprocess.run([tool, "-state", state, "-port", env["DAKIM_PG_PORT"],
                                 "-operation", operation, "-capture", original["reference"],
                                 "-amount", "120"], capture_output=True, text=True)
        assert result.returncode == 0, result.stderr
        return json.loads(result.stdout)
    with ThreadPoolExecutor(max_workers=4) as pool:
        responses = list(pool.map(lambda _: source_refund(), range(4)))
    source = responses[0]
    assert all(row == source for row in responses), responses
    assert source["status"] == "succeeded" and source["charge_reference"] == original["reference"]

    def adjustment():
        return next((row for row in pay.req("/v1/adjustments")["adjustments"]
                     if row["source_operation"] == operation), None)
    issue = until(adjustment)
    assert (issue["payment_id"], issue["original_payout"], issue["reference"], issue["amount"]) == (
        original["id"], first["id"], source["reference"], 120)
    assert (issue["original_bank"], issue["original_deposit_minor"]) == (first["bank_trace"], 353)
    def exceptions():
        return [r for r in finance.req("/api/state")["settlement"]["providerRefunds"] if r["id"] == issue["id"]]
    def status_is(status):
        rows = exceptions()
        return len(rows) == 1 and rows[0]["status"] == status
    until(lambda: status_is("awaiting_bank"))
    exception = exceptions()[0]
    assert (exception["receiptId"], exception["originalPayout"], exception["amountMinor"]) == (receipt, first["id"], 120)
    assert original_books() == baseline and journal(first_batch) == first_journal

    later = pay.req("/v1/settle", {}, str(uuid.uuid4()))
    assert later["net"] == -120 and later["status"] == "in_transit"
    assert later["id"] != first["id"] and later["bank_trace"] != first["bank_trace"]
    assert bank_entry(later["bank_trace"]) is None
    later_path = "/v1/payouts/" + later["id"] + "/statement"
    pay.req(later_path, expect_error=409)
    # Supply exact prospective figures before the bank posts. An independently
    # fetched, matched bank statement must still gate Mimi's decision.
    line = {"kind": "provider_refund", "reference": issue["id"], "original": original["id"],
            "grossMinor": -120, "feeMinor": 0}
    prospective = {"payout": later["id"], "bank": later["bank_trace"], "depositMinor": -120,
                   "currency": "USD", "lines": [line]}
    import_key = str(uuid.uuid4())
    batch = finance.cmd("finance.settlement.import", prospective, import_key)["id"]
    close = {"action": "finance.settlement.close", "input": {"id": batch}}
    manager.req("/api/command", close, str(uuid.uuid4()), expect_error=409)
    rejected(finance, PROPOSE, proposal_input(issue, batch), 409)
    assert not journal(batch) and status_is("awaiting_bank")

    restart_apps()
    assert source_refund() == source and adjustment() == issue and status_is("awaiting_bank")
    assert finance.cmd("finance.settlement.import", prospective, import_key)["id"] == batch
    assert pay.req("/v1/reconcile", {})["matched"] == 0
    pay.req("/app/bank-post/" + later["bank_trace"], {})
    assert pay.req("/v1/reconcile", {})["matched"] == 1
    statement = pay.req(later_path)
    assert statement["depositMinor"] == -120 and statement["bank"] == later["bank_trace"]
    assert len(statement["lines"]) == 1 and all(statement["lines"][0][k] == v for k,v in line.items())
    def debit_verified():
        rows = finance.req("/api/settlement/" + batch)["rows"]
        return len(rows) == 1 and rows[0]["result"] == "matched"
    until(debit_verified)
    finance.req("/api/command", close, str(uuid.uuid4()), expect_error=403)
    close_key = str(uuid.uuid4())
    manager.cmd("finance.settlement.close", {"id": batch}, close_key)
    assert status_is("bank_matched")
    restart_apps()
    manager.cmd("finance.settlement.close", {"id": batch}, close_key)
    manager.req("/api/command", close, str(uuid.uuid4()), expect_error=409)
    assert source_refund() == source
    pay.req("/app/bank-post/" + later["bank_trace"], {})
    posted_bank = bank_entry(later["bank_trace"])
    assert bank_entry(first["bank_trace"]) == original_bank
    assert posted_bank == {"trace": later["bank_trace"], "merchant_id": "mimi", "currency": "USD",
                           "amount": -120, "status": "posted"}
    assert len([a for a in pay.req("/v1/adjustments")["adjustments"] if a["source_operation"] == operation]) == 1
    assert status_is("bank_matched") and journal(batch) == "bank|-120\nprovider_refund_suspense|120"
    assert original_books() == baseline and journal(first_batch) == first_journal
    assert pay.req(first_export_path) == first_export and pay.req(later_path) == statement
    assert pay.req("/v1/reconcile", {})["matched"] == 0
    classification = verify_classification(finance, manager, pay, env, repo, restart_apps, sql, issue, batch, receipt)
    assert original_books() == baseline and journal(first_batch) == first_journal
    assert bank_entry(first["bank_trace"]) == original_bank and bank_entry(later["bank_trace"]) == posted_bank
    assert pay.req(first_export_path) == first_export and pay.req(later_path) == statement
    assert journal(batch) == "bank|-120\nprovider_refund_suspense|120"
    if evidence_path := env.get("DAKIM_SHIFT_EVIDENCE_FILE"):
        digest = lambda value: hashlib.sha256(value.encode()).hexdigest()
        sample = {
            "environment": "local simulated acquirer and bank; disposable Mimi store",
            "verified_at": datetime.now(timezone.utc).isoformat(), "currency": "USD", "units": "cents",
            "receipt": receipt, "capture": {"payment": original["id"], "reference": original["reference"], "amount": 360},
            "original_payout": first["id"], "original_bank_entry": original_bank,
            "source_refund": source, "finance_exception": {"id": issue["id"], "sequence": issue["sequence"],
                "receipt": exception["receiptId"], "original_bank": issue["original_bank"],
                "original_deposit_minor": issue["original_deposit_minor"],
                "states": ["awaiting_bank", "bank_matched"], "count_after_replay": 1},
            "later_statement": statement, "later_bank_lookup_http_before_post": 404, "later_bank_after_post": posted_bank,
            "original_settlement_journal": journal(first_batch).splitlines(),
            "later_settlement_journal": journal(batch).splitlines(),
            "original_record_sha256_before": digest(baseline), "original_record_sha256_after": digest(original_books()),
            "premature_close_http": 409, "self_approval_http": 403, "service_restarts": 2 + classification["service_restarts_during_classification"],
            "reporting_classification": classification,
            "concurrent_source_submissions": len(responses),
        }
        with Path(evidence_path).open("x") as file:
            json.dump(sample, file, indent=2)
            file.write("\n")
    print("PASS post-receipt source refund: original 3.60 receipt and 3.53 bank deposit intact; "
          "one 1.20 finance exception; premature close denied; one independently matched bank debit; "
          "two restarts and concurrent replay preserved all postings")
    print("PASS matched debit returned for documented correction, replayed after restart and independently approved; "
          "two proposals, one return, one effective approval after six concurrent approvals and three restarts; "
          "all 26 financial fingerprints unchanged")
