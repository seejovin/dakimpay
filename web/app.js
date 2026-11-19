'use strict';

const $ = (selector, root = document) => root.querySelector(selector);
const view = $('#view');
const dialog = $('#modal');
const moneyFormatter = new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD' });
const dateFormatter = new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
const pages = { overview: 'Merchant overview', payments: 'Payments', deposits: 'Bank deposits', activity: 'Activity' };
const brands = { visa: 'Visa', mastercard: 'Mastercard', amex: 'American Express' };
const stateLabels = { unknown: 'Needs recovery', awaiting_customer: 'Awaiting customer', authorized: 'Awaiting capture', captured: 'Captured', expired: 'Expired unpaid',
  succeeded: 'Refunded', matched: 'Matched', in_transit: 'In transit', pending: 'Pending', declined: 'Declined' };
const storeKey = 'dakim-pay.mimi.v1.pending';
let snapshot;
let busy = false;
let readVersion = 0;
let pending = null;
try { pending = JSON.parse(sessionStorage.getItem(storeKey)); } catch { /* Storage is optional. */ }

function escapeText(value) {
  return String(value ?? '').replace(/[&<>"']/g, (c) => ({ '&':'&amp;', '<':'&lt;', '>':'&gt;', '"':'&quot;', "'":'&#39;' })[c]);
}
const html = escapeText;
const money = (value) => moneyFormatter.format(Number(value) / 100);
const encode = encodeURIComponent;
const list = (value) => Array.isArray(value) ? value : [];
const uid = () => crypto.randomUUID();
const currentPage = () => Object.hasOwn(pages, location.hash.slice(1)) ? location.hash.slice(1) : 'overview';

function date(value) {
  const d = new Date(value);
  return Number.isNaN(d.getTime()) ? '' : dateFormatter.format(d);
}
function orderRecords(rows) {
  return list(rows).slice().sort((a,b) => (Date.parse(b.created_at) || 0) - (Date.parse(a.created_at) || 0));
}
function shortRef(value) {
  const s=String(value || '');
  return s.length > 23 ? s.slice(0,12) + '…' + s.slice(-6) : s;
}
function badge(status) {
  const tone = status === 'unknown' || status === 'declined' ? 'alert' :
    ['in_transit','awaiting_customer','authorized','pending'].includes(status) ? 'wait' : '';
  return '<span class="pill ' + tone + '">' + html(stateLabels[status] || status) + '</span>';
}
function button(label, action, id = '', primary = false) {
  return '<button type="button" class="btn compact' + (primary ? ' primary' : '') + '" data-action="' + html(action) +
    '" data-id="' + html(id) + '">' + html(label) + '</button>';
}
function empty(title, body) {
  return '<div class="empty"><strong>' + html(title) + '</strong><p>' + html(body) + '</p></div>';
}
function sectionHead(title, description = '', actions = '') {
  return '<header class="panel-head"><div><h2>' + html(title) + '</h2>' +
    (description ? '<p>' + html(description) + '</p>' : '') + '</div>' +
    (actions ? '<div class="actions">' + actions + '</div>' : '') + '</header>';
}
function remember(request) {
  pending = request;
  try {
    if (request) sessionStorage.setItem(storeKey, JSON.stringify(request));
    else sessionStorage.removeItem(storeKey);
  } catch { /* The same operation can still be retried in this tab. */ }
}
function flash(message, error = false) {
  const markup = '<div class="notice' + (error ? ' error' : '') + '">' + html(message) + '</div>';
  $('#notice').innerHTML = markup;
  const inside = $('#dialog-notice');
  if (dialog.open && inside) inside.innerHTML = markup;
}
function renderRecovery() {
  const holder = $('#recovery');
  if (!pending || busy) { holder.replaceChildren(); return; }
  const local = dialog.open && $('#dialog-notice');
  holder.innerHTML = '<div class="notice error"><strong>Unconfirmed request</strong><p>' +
    html(pending.label) + ' may have already reached the service. Retry the same request to retrieve its result.</p>' +
    '<div class="actions">' + button('Retry original request','retry') + '</div></div>';
  if (local) local.innerHTML = holder.innerHTML;
}
function protectButtons(disabled) {
  for (const el of document.querySelectorAll('button[data-action],button[type=submit]')) {
    if (!['close','detail','new','refund-form'].includes(el.dataset.action)) el.disabled = disabled;
  }
}
function amountFrom(field) {
  const value = field.value.trim();
  if (!/^(?:0|[1-9]\d{0,7})(?:\.\d{1,2})?$/.test(value)) throw Error('Enter a positive USD amount with up to two decimal places.');
  const [whole, fraction = ''] = value.split('.');
  const result = Number(whole) * 100 + Number(fraction.padEnd(2,'0'));
  if (result <= 0) throw Error('Enter an amount greater than zero.');
  return result;
}
function channelLabel(channel) { return channel === 'terminal' ? 'Terminal' : channel === 'online' ? 'Online' : String(channel || ''); }
function paymentTable(rows) {
  if (!rows.length) return empty('No payments yet', 'Create a sale or run the payment journey to see Mimi’s captured and pending payments.');
  return '<table class="data-table"><thead><tr><th>Order</th><th class="channel-col">Channel</th><th>Status</th><th class="amount">Amount</th></tr></thead><tbody>' +
    rows.map((p) => '<tr><td><button class="link order" type="button" data-action="detail" data-id="' + html(p.id) + '">' + html(p.order_id) +
    '</button><small class="sub">' + html(brands[p.brand] || p.brand) + '</small></td><td class="channel-col">' + html(channelLabel(p.channel)) +
    '</td><td>' + badge(p.status) + '</td><td class="amount">' + html(money(p.amount)) + '</td></tr>').join('') + '</tbody></table>';
}
function refundList(rows) {
  if (!rows.length) return empty('No refunds recorded', 'Open a captured payment to refund an amount to the original card.');
  return '<ul class="flow-list">' + rows.map((r) => {
    const p = snapshot.payments.find((p) => p.id === r.payment_id);
    return '<li><div><strong>' + html(p?.order_id || shortRef(r.payment_id)) + '</strong><small class="sub">' +
      html((r.source_operation ? 'Acquirer refund · ' : '') + date(r.created_at)) + '</small></div><div class="right">' + html(money(r.amount)) + '<small class="sub">' +
      html(stateLabels[r.status] || r.status) + '</small>' + (r.status === 'unknown' ? button('Recover refund','recover-refund',r.id) : '') + '</div></li>';
  }).join('') + '</ul>';
}
function events(rows) {
  if (!rows.length) return empty('The timeline is clear', 'Payment, recovery and settlement events will appear here as they are recorded.');
  return '<ol class="event-list">' + rows.map((e) => '<li><i class="event-dot" aria-hidden="true"></i><div><strong>' +
    html(e.message || e.kind) + '</strong><small>' + html([date(e.created_at),shortRef(e.subject)].filter(Boolean).join(' · ')) +
    '</small></div></li>').join('') + '</ol>';
}
function processorCallbacks(records) {
  if (!records.length) return empty('No processor notifications received', 'The next signed processor delivery will appear here.');
  return records.map((record) => {
    const conflicts = Number(record.conflicts) || 0;
    const label = conflicts ? 'Conflicting delivery' : ({ applied: 'Checked', pending: 'Awaiting verification', exception: 'Needs review' }[record.status] || record.status);
    const tone = conflicts || record.status === 'exception' ? 'alert' : record.status === 'pending' ? 'wait' : '';
    return '<details class="payout-item"><summary>' + html(record.type) + ' <span class="pill ' + tone + '">' +
      html(label) + '</span> <span class="support">' + html(date(record.received_at)) + '</span></summary>' +
      '<div class="payout-detail"><p>' + html(record.detail) + '</p><p class="support">Event ' + html(record.event_id) +
      '<br>Original operation ' + html(record.operation_id) + '<br>Verification attempts ' + html(record.attempts) +
      ' · Conflicting payloads ' + html(conflicts) + '</p></div></details>';
  }).join('');
}
function payoutLines(lines) {
  return '<ul class="flow-list">' + list(lines).map((line) => '<li><div><strong>' +
    html(({capture:'Capture',refund:'Refund',fee:'Processing fee'})[line.kind] || line.kind) + '</strong><small class="sub">' +
    html(line.reference) + '</small></div><span class="amount">' + html(money(line.amount)) + '</span></li>').join('') + '</ul>';
}
function payouts(records) {
  if (!records.length) return empty('No payouts recorded', 'Create a payout after a sale. Post its bank entry and reconcile the deposit to match it.');
  return records.map((p) => '<details class="payout-item"><summary>' + (p.net < 0 ? 'Bank debit · ' : '') + html(shortRef(p.id)) + ' <span class="amount">' + html(money(p.net)) +
    '</span> ' + badge(p.status) + '</summary><div class="payout-detail"><p class="support">Bank trace ' + html(p.bank_trace) + '</p>' +
    payoutLines(p.lines) + (p.status === 'matched' ? '<a class="link" href="/v1/payouts/' + encode(p.id) + '/statement" download="mimi-statement-' + html(p.id) + '.json">Download Mimi statement</a>' : '') + '</div></details>' +
    (p.status === 'in_transit' ? '<div class="payout-controls">' + button('Post bank entry','bank',p.bank_trace) + '</div>' : '')).join('');
}
const timingStatuses = {in_transit:'Payout in transit',bank_only:'Bank posting only',amount_mismatch:'Amounts differ',matched:'Source matched'};

function timingStamp(value) {
  if (typeof value !== 'string' || !/(Z|[+-]\d\d:\d\d)$/.test(value) || !Number.isFinite(Date.parse(value))) {
    throw Error('The timing report includes an invalid source timestamp.');
  }
  const utc = value.endsWith('Z') ? value : new Date(value).toISOString();
  return utc.replace('T',' ').replace(/(?:\.0+)?Z$/,' UTC');
}
function timingAfter(left, right) {
  const delta = Date.parse(left) - Date.parse(right);
  if (delta !== 0) return delta > 0;
  // Date.parse discards the microseconds present in retained cutoffs.
  const fraction = (stamp) => (stamp.match(/\.(\d+)/)?.[1] || '').padEnd(9,'0');
  return fraction(left) > fraction(right);
}
function timingDuration(seconds) {
  if (!Number.isSafeInteger(seconds) || seconds < 0) throw Error('The timing report includes an invalid age.');
  return [[86400,'d'],[3600,'h'],[60,'m'],[1,'s']].map(([unit,suffix]) => {
    const count = Math.floor(seconds / unit);
    seconds %= unit;
    return count ? count + suffix : '';
  }).filter(Boolean).join(' ') || '0s';
}
function timingRow(row, currency, matched) {
  if (!row || !Object.hasOwn(timingStatuses,row.status) || matched !== (row.status === 'matched') ||
      row.currency !== currency || !row.bank_trace) throw Error('The timing report includes an unrecognized item.');
  const amount = (value) => {
    if (!Number.isSafeInteger(value)) throw Error('A settlement amount is unavailable.');
    return money(value) + ' ' + currency;
  };
  const processor = row.status !== 'bank_only', bank = row.status !== 'in_transit';
  if (processor && (!row.payout_id || !row.operation_id)) throw Error('The processor payout reference is unavailable.');
  const risk = row.status === 'amount_mismatch' || row.status === 'bank_only';
  const fields = [];
  if (processor) fields.push(['Processor payout',row.payout_id],['Settlement operation',row.operation_id],
    ['Payout effective',timingStamp(row.payout_at)],['Payout first observed',timingStamp(row.payout_known_at)],['Processor amount',amount(row.processor_amount)]);
  if (bank) fields.push(['Bank posting effective',timingStamp(row.posted_at)],['Bank posting first observed',timingStamp(row.bank_known_at)],['Posted amount',amount(row.bank_amount)]);
  if (row.booked_match_at) fields.push(['Local ledger match',timingStamp(row.booked_match_at)],['Booking first observed',timingStamp(row.booking_known_at)]);
  else fields.push(['Local ledger match','No recorded match by cutoff']);
  const total = row.status === 'amount_mismatch' ? 'Processor ' + amount(row.processor_amount) + ' · Bank ' + amount(row.bank_amount) :
    amount(processor ? row.processor_amount : row.bank_amount);
  return '<li><details><summary><span class="timing-summary"><span class="pill ' + (risk ? 'alert' : matched ? '' : 'wait') + '">' +
    timingStatuses[row.status] + '</span><strong class="timing-amount">' + html(total) + '</strong><span class="timing-age">' +
    (matched ? 'Between records ' : 'Open for ') + timingDuration(row.age_seconds) + '</span></span><span class="sub timing-trace">' +
    'Bank trace · <span>' + html(row.bank_trace) + '</span></span></summary>' + detailsList(fields) + '</details></li>';
}
function settlementTiming(report, showMatched) {
  if (report?.merchant_id !== 'mimi' || report.currency !== 'USD' || !Array.isArray(report.open) || !Array.isArray(report.matched)) {
    throw Error('The service returned an incomplete timing report.');
  }
  const rows = (items,matched) => '<ul class="timing-items">' + items.map(row => timingRow(row,report.currency,matched)).join('') + '</ul>';
  const beyondCoverage = timingAfter(report.as_of,report.source_through);
  return detailsList([['Source effective cutoff',timingStamp(report.as_of)],['Knowledge cutoff',timingStamp(report.known_as_of)],
    ['Last completed observation',timingStamp(report.observed_at)],['Sources covered through',timingStamp(report.source_through)]]) +
    (beyondCoverage ? '<p class="support">The effective cutoff extends past the last source check. Later deliveries may be missing.</p>' : '') +
    '<h3>Open items · ' + report.open.length + '</h3>' +
    (report.open.length ? rows(report.open,false) : '<p class="support">No open items in the retained evidence.</p>') +
    '<details class="timing-matches"' + (showMatched ? ' open' : '') + '><summary>Source matched · ' + report.matched.length +
    '</summary>' + (report.matched.length ? rows(report.matched,true) : '<p class="support">No matches in the retained evidence.</p>') + '</details>';
}
function timingReplayControls(selection, report, expanded) {
  const known = selection.known || report?.known_as_of || '';
  const effective = selection.effective || (report && report.as_of !== report.known_as_of ? report.as_of : '');
  return '<details class="timing-replay"' + (expanded ? ' open' : '') + '><summary>Replay a prior cutoff</summary>' +
    '<form id="timing-replay-form"><label>Known by (UTC)<input name="known" required autocomplete="off" ' +
    'placeholder="YYYY-MM-DD HH:MM:SS" value="' + html(known) + '"></label>' +
    '<label>Source effective through (UTC, optional)<input name="effective" autocomplete="off" ' +
    'placeholder="Same as knowledge cutoff" value="' + html(effective) + '"></label>' +
    '<p class="support">Enter a UTC date and time. An earlier effective cutoff limits the movements included.</p>' +
    '<div class="actions"><button class="btn compact" type="submit">Replay report</button></div></form></details>';
}

function timingFilter(value) {
  const parts = /^(\d{4}-\d{2}-\d{2})[ T](\d{2}:\d{2})(:\d{2}(?:\.\d{1,6})?)?Z?$/.exec(value.trim());
  if (!parts) throw Error('Enter a UTC date and time, for example 2026-11-19 12:30:00.');
  // Preserve database microseconds when replaying the exact first observation.
  return parts[1] + 'T' + parts[2] + (parts[3] || ':00') + 'Z';
}

let timingObservationInFlight = false;
async function showTimingQueue(selection = {}, record = false) {
  if (record && timingObservationInFlight) return;
  if (record) timingObservationInFlight = true;
  const refreshing = dialog.open && Boolean($('#timing-queue',dialog));
  const showMatched = refreshing && Boolean($('#timing-queue .timing-matches[open]',dialog));
  const expanded = Boolean(selection.known);
  const params = new URLSearchParams();
  if (selection.known) params.set('known_as_of', selection.known);
  if (selection.effective) params.set('as_of', selection.effective);
  const status = record ? 'Recording a complete observation from both sources…' : 'Reading retained settlement history…';
  show(head('Settlement timing','Deposits') + '<section id="timing-queue" aria-live="polite" aria-busy="true">' + status + '</section>');
  const output = $('#timing-queue',dialog);
  let report,markup;
  try {
    const path = record ? '/v1/settlement-timing/observations' : '/v1/settlement-timing/history' + (params.size ? '?' + params : '');
    const response = await fetch(path,{method:record ? 'POST' : 'GET',credentials:'same-origin',cache:'no-store',signal:AbortSignal.timeout(15000)});
    const data = await response.json();
    if (!response.ok) {
      if (response.status === 409 && !record) throw Error(selection.known ?
        'No complete observation was available at this knowledge cutoff. Choose a later cutoff.' :
        'No complete observation is available yet. Record a current observation to start settlement history.');
      throw Error(typeof data?.error === 'string' ? data.error : 'The complete settlement report is unavailable.');
    }
    markup = settlementTiming(data,showMatched);
    report = data;
  } catch (err) {
    const reason = err instanceof TypeError || err instanceof SyntaxError || err.name === 'TimeoutError' ?
      record ? 'Observation could not be confirmed. Check the latest retained report.' : 'The retained settlement report could not be retrieved.' : err.message;
    markup = '<div class="notice error"><strong>Settlement history unavailable</strong><p>' + html(reason) + '</p></div>';
  } finally {
    if (record) timingObservationInFlight = false;
  }
  if (dialog.open && output.isConnected) {
    output.innerHTML = '<p class="support">Replay uses retained observations. Recording reads the current processor and bank records.</p>' +
      '<div class="actions timing-actions">' + button('Record current observation','timing-observe') +
      button('View latest retained report','timing') + '</div>' +
      timingReplayControls(selection,report,expanded) + markup;
    output.removeAttribute('aria-busy');
    if (refreshing) output.querySelector(expanded ? '.timing-replay summary' : '[data-action="timing"]')?.focus({preventScroll:true});
  }
}
const importLabels = {unbooked:'Awaiting import',in_transit:'In transit',matched:'Matched'};
function feeReports(reports) {
  if (!Array.isArray(reports)) return empty('Fee comparisons unavailable','Refresh the workspace to retrieve settlement requests.');
  if (!reports.length) return empty('No settlement requests','Fee comparisons will appear here when Mimi requests a payout.');
  return '<ul class="flow-list fee-reports">' + reports.map((r) => '<li><div><strong>' + html(r.payout_id || r.operation_id) +
    '</strong><small class="sub">' + html(importLabels[r.local_status] || r.local_status) + ' · ' + html(r.batch) + '</small></div>' +
    button('Compare fees','fee-report',r.operation_id) + '</li>').join('') + '</ul>';
}
function signedFee(value) {
  return (value > 0 ? '+' : '') + money(value);
}
function feeComparison(request, report) {
  const bankLabels = {matched:'Matches the statement',pending:'Awaiting posting',mismatch:'Differs from the statement',unavailable:'Evidence unavailable'};
  const invalid = 'The fee comparison contains incomplete or unexpected values.';
  if (report?.operation_id !== request.operation_id || (request.payout_id && report.payout_id !== request.payout_id) ||
      report.merchant_id !== 'mimi' || report.currency !== 'USD' || !Object.hasOwn(bankLabels,report.bank_status) ||
      !Object.hasOwn(importLabels,report.local_status) || !['aligned','variance','incomplete','no_fees'].includes(report.status) ||
      !Array.isArray(report.lines) || !Number.isSafeInteger(report.net) || !Number.isSafeInteger(report.actual_total) ||
      !Number.isSafeInteger(report.different_count) || !Number.isSafeInteger(report.missing_count)) throw Error(invalid);
  const value = (n,format=money) => {
    if (n === null) return 'Unavailable';
    if (!Number.isSafeInteger(n)) throw Error(invalid);
    return format(n);
  };
  const differences = report.different_count;
  const missing = report.missing_count;
  const summary = [differences ? differences + (differences === 1 ? ' fee differs' : ' fees differ') + ' from the recorded estimate.' : '',
    missing ? missing + (missing === 1 ? ' line has' : ' lines have') + ' no recorded estimate.' : '',
    differences && report.variance === 0 ? 'The differences offset in the total.' : ''].filter(Boolean).join(' ');
  const rows = report.lines.map((line) => '<li><details><summary><strong>' + html(line.order_id || line.reference) + '</strong> ' +
    '<span class="sub">' + (line.variance === null ? 'Estimate unavailable' : line.variance === 0 ? 'Fee agrees' : 'Difference ' + html(value(line.variance,signedFee))) +
    '</span></summary>' + detailsList([['Payment ID',line.payment_id],['Acquirer charge',line.reference],['Statement line',line.line_id],['Gross sale',value(line.gross)],
      ['Estimate policy',line.policy || 'Unavailable'],['Estimated rate',value(line.rate_bps,(n) => (n/100) + '%')],
      ['Estimate recorded',line.recorded_at || 'Unavailable']]) + '</details>' +
    '<dl class="fee-amounts"><div><dt>Estimated</dt><dd>' + html(value(line.expected)) + '</dd></div><div><dt>Statement</dt><dd>' +
    html(value(line.actual)) + '</dd></div><div><dt>Difference</dt><dd>' + html(value(line.variance,signedFee)) + '</dd></div></dl></li>');
  return (summary ? '<p class="notice error">' + html(summary) + '</p>' :
    report.status === 'aligned' ? '<p class="support">All itemized fees agree with their recorded estimates.</p>' : '') +
    detailsList([['Settlement request',report.operation_id],['Acquirer payout',report.payout_id],['Local payout status',importLabels[report.local_status]],
      ['Bank trace',report.bank_trace],['Statement net',value(report.net)],['Bank posting',bankLabels[report.bank_status]],
      ['Bank amount',value(report.bank_amount)],['Estimated fees',value(report.expected_total)],['Statement fees',value(report.actual_total)],
      ['Net fee difference',value(report.variance,signedFee)]]) +
    (rows.length ? '<h3>Itemized fees</h3><p class="support">A positive difference means the statement charged more than estimated. Open an order for its source references.</p>' +
      '<ul class="fee-lines">' + rows.join('') + '</ul>' : '<p class="support">This statement has no charge fees to compare.</p>');
}
async function showFeeReport(operation) {
  const request = snapshot?.fee_reports?.find((r) => r.operation_id === operation);
  if (!request) return;
  show(head('Processing fee comparison','Deposits') + '<section id="fee-comparison" aria-live="polite" aria-busy="true">Loading the processor estimate, statement and bank evidence…</section>');
  const output = $('#fee-comparison',dialog);
  let markup;
  try {
    const response = await fetch('/v1/settlements/' + encode(operation) + '/fee-report',
      {credentials:'same-origin',cache:'no-store',signal:AbortSignal.timeout(10000)});
    if (response.status === 409) throw Error('An itemized processor statement is not yet available for this request.');
    if (!response.ok) throw Error('The service could not retrieve the fee evidence.');
    markup = feeComparison(request,await response.json());
  } catch (err) {
    const reason = err instanceof TypeError || err instanceof SyntaxError || err.name === 'TimeoutError' ? 'The service could not retrieve the fee evidence.' : err.message;
    markup = '<div class="notice error"><strong>Fee comparison unavailable</strong><p>' + html(reason) + '</p></div>' + button('Retry comparison','fee-report',operation);
  }
  if (dialog.open && output.isConnected) {
    output.innerHTML = markup;
    output.removeAttribute('aria-busy');
  }
}
function overview() {
  const t = snapshot.totals;
  const records = orderRecords(snapshot.payments), deposits = orderRecords(snapshot.payouts), history = orderRecords(snapshot.events);
  const matched = deposits.filter((p) => p.status === 'matched').length;
  const hasJourney = records.some((p) => p.order_id === 'MIMI-GUIDED-TERMINAL');
  return '<div class="hero"><section class="account"><div><p class="overline">Net matched bank activity</p><p class="account-total">' +
    html(money(t.deposited)) + '</p></div><div class="account-foot"><span>Mimi settlement account</span><span>' +
    matched + (matched === 1 ? ' bank entry' : ' bank entries') + '</span></div></section>' +
    '<section class="journey"><p class="overline">Acquirer simulator</p><h2>Follow a payment</h2><p>Recover a $42 terminal sale, approve a $68 online checkout, then follow the $10 refund and bank deposit.</p>' +
    '<div class="actions"><button type="button" class="btn primary" data-action="demo">Run payment journey <span aria-hidden="true">→</span></button>' +
    (hasJourney ? '<button type="button" class="link" data-action="journey">View journey trace</button>' : '') + '</div></section></div>' +
    '<dl class="metrics">' + [['Captured sales',t.captured],['Refunded',t.refunded],['Processing fees',t.fees],['Deposits in transit',t.pending_deposit]].map(([label,v]) =>
    '<div class="metric"><dt>' + label + '</dt><dd>' + html(money(v)) + '</dd></div>').join('') + '</dl>' +
    '<section class="panel">' + sectionHead('Recent payments','', '<a class="link" href="#payments">All payments</a>') + paymentTable(records.slice(0,5)) + '</section>' +
    '<div class="panel-grid"><section class="panel">' + sectionHead('Bank deposits','', '<a class="link" href="#deposits">Manage deposits</a>') +
    payouts(deposits.slice(0,3)) + '</section><section class="panel">' + sectionHead('Recent activity','', '<a class="link" href="#activity">All activity</a>') +
    events(history.slice(0,4)) + '</section></div>';
}
function render() {
  const page = currentPage();
  const name = pages[page];
  document.title = name + ' · dakim pay';
  $('#title').textContent = name;
  $('#section-kicker').textContent = page === 'overview' ? 'Merchant workspace' : 'Mimi grocery';
  for (const a of document.querySelectorAll('nav a')) {
    if (a.hash === '#' + page) a.setAttribute('aria-current','page');
    else a.removeAttribute('aria-current');
  }
  if (!snapshot) return;
  for (const field of ['payments','payouts','refunds','events']) snapshot[field] = list(snapshot[field]);
  snapshot.totals ||= {};
  const open = [...view.querySelectorAll('details[open]')].map((el) => el.querySelector('summary')?.textContent);
  if (page === 'overview') view.innerHTML = overview();
  if (page === 'payments') view.innerHTML = '<section class="panel">' + sectionHead('Payment register','Select an order to inspect its original reference, check its status or issue a refund.') +
    paymentTable(orderRecords(snapshot.payments)) + '</section><section class="panel">' + sectionHead('Refunds') + refundList(orderRecords(snapshot.refunds)) + '</section>';
  if (page === 'deposits') view.innerHTML = '<section class="panel">' + sectionHead('Settlement timing',
    'Trace outstanding processor payouts and bank postings by age.',button('Open timing queue','timing')) + '</section>' +
    '<section class="panel">' + sectionHead('Payout statements','Match acquirer lines to the independently posted bank entry.',
    button('Create payout','settle') + button('Reconcile deposits','reconcile','',true)) + payouts(orderRecords(snapshot.payouts)) + '</section>' +
    '<section class="panel">' + sectionHead('Processing fee comparisons','Review estimated and statement fees for each charge, including payouts awaiting import.') +
    feeReports(snapshot.fee_reports) + '</section>';
  if (page === 'activity') view.innerHTML = '<section class="panel">' + sectionHead('Transaction timeline','Events from payment collection through to bank reconciliation.') +
    events(orderRecords(snapshot.events)) + '</section><section class="panel">' +
    sectionHead('Processor notifications','Review delivery conflicts and verification against the original transaction.') +
    processorCallbacks(list(snapshot.callbacks)) + '</section>';
  for (const item of view.querySelectorAll('details')) if (open.includes(item.querySelector('summary')?.textContent)) item.open = true;
  renderRecovery();
  protectButtons(busy);
}
async function read() {
  const version = ++readVersion;
  view.setAttribute('aria-busy','true');
  try {
    const response = await fetch('/v1/dashboard',{ credentials:'same-origin', cache:'no-store' });
    if (!response.ok) throw Error('Merchant records are unavailable (' + response.status + ').');
    const data = await response.json();
    if (version !== readVersion) return;
    snapshot = data;
    render();
  } finally {
    if (version === readVersion) view.setAttribute('aria-busy','false');
  }
}
function head(title, kicker) {
  return '<div class="dialog-head"><div><p class="overline">' + html(kicker) + '</p><h2 id="dialog-title">' + html(title) +
    '</h2></div><button class="close" type="button" data-action="close" aria-label="Close dialog">×</button></div><div id="dialog-notice" role="status" aria-live="polite"></div>';
}
function show(markup) {
  $('#dialog-content').innerHTML = markup;
  if (!dialog.open) dialog.showModal();
}
function newSale() {
  if (pending) { flash('Retrieve the original request before starting another operation.',true); return; }
  show(head('New sale','Simulated acceptance') + '<form id="sale-form"><label>Order reference<input name="order" required maxlength="80" value="MIMI-' +
    html(uid().slice(0,8).toUpperCase()) + '"></label><div class="payment-grid"><label>Amount (USD)<input name="amount" inputmode="decimal" required placeholder="42.00" value="42.00"></label>' +
    '<label>Channel<select name="channel"><option value="terminal">Terminal</option><option value="online">Online checkout</option></select></label></div>' +
    '<label>Simulated card network<select name="brand"><option value="visa">Visa</option><option value="mastercard">Mastercard</option><option value="amex">American Express</option></select></label>' +
    '<label class="check" id="loss-field"><input type="checkbox" name="loss"><span>Lose the terminal response after capture</span></label>' +
    '<p class="support" id="channel-help">Terminal MIMI-01 uses a simulated card. No card details are required.</p>' +
    '<div class="actions"><button type="submit" class="btn primary">Create sale</button></div></form>');
}
function safeHostedLink(value) {
  if (!value) return '';
  try {
    const u = new URL(value);
    return ['http:','https:'].includes(u.protocol) && !u.username && !u.password &&
      ['localhost','127.0.0.1','[::1]',location.hostname].includes(u.hostname) ? u.href : '';
  } catch { return ''; }
}
function detailsList(items) {
  return '<dl class="details">' + items.map(([label,value]) => '<div><dt>' + html(label) + '</dt><dd>' + html(value) + '</dd></div>').join('') + '</dl>';
}
function refundEvidence(payment, evidence) {
  const original = evidence?.payment;
  const observation = new Map([['none','No dispute reported'],['open','Open dispute'],['unknown','Unknown']]);
  const incomplete = 'The service could not verify the complete refund history for this payment.';
  if (evidence?.complete !== true || !Array.isArray(evidence.refunds) || !observation.has(evidence.dispute_status) ||
      !original || original.status !== 'captured' || original.id !== payment.id || !original.reference ||
      original.reference !== payment.reference || original.merchant_id !== payment.merchant_id ||
      original.order_id !== payment.order_id || original.currency !== payment.currency || original.amount !== payment.amount) {
    throw Error(incomplete);
  }
  let total = 0;
  const ids = new Set(), references = new Set();
  const rows = evidence.refunds.map((refund) => {
    if (!refund || typeof refund.id !== 'string' || !refund.id || ids.has(refund.id) ||
        typeof refund.reference !== 'string' || !refund.reference || references.has(refund.reference) ||
        refund.status !== 'succeeded' || refund.payment_id !== payment.id || refund.charge_reference !== payment.reference ||
        refund.merchant_id !== payment.merchant_id || refund.currency !== payment.currency ||
        !Number.isSafeInteger(refund.amount) || refund.amount <= 0) throw Error(incomplete);
    ids.add(refund.id);
    references.add(refund.reference);
    total += refund.amount;
    return '<li><strong>' + html(money(refund.amount)) + '</strong>' +
      detailsList([['Refund ID',refund.id],['Acquirer refund reference',refund.reference],
        ...(refund.source_operation ? [['Initiated by','Acquirer'],['Source operation',refund.source_operation]] : [])]) + '</li>';
  });
  if (!Number.isSafeInteger(total) || total !== original.refunded || total > original.amount) throw Error(incomplete);
  const status = total === 0 ? 'No refunds' : total === original.amount ? 'Fully refunded' : 'Partially refunded';
  const held = evidence.dispute_status !== 'none';
  return '<h3>Refund evidence</h3>' +
    (held ? '<p class="notice error">Hold for review. Verify the dispute with the acquirer before closing this case.</p>' : '') +
    detailsList([['Refund status',status],['Verified refunds',money(total)],['Unrefunded amount',money(original.amount-total)],
      ['Acquirer dispute observation',observation.get(evidence.dispute_status)]]) +
    (rows.length ? '<ol aria-label="Verified refunds">' + rows.join('') + '</ol>' : '');
}
async function checkRefundEvidence(id) {
  const output = $('#refund-evidence',dialog);
  const payment = snapshot?.payments.find((p) => p.id === id);
  if (!payment || !output || output.dataset.id !== id || output.getAttribute('aria-busy') === 'true') return;
  output.setAttribute('aria-busy','true');
  output.textContent = 'Checking the original payment and refunds…';
  let markup;
  try {
    const response = await fetch('/v1/payments/' + encode(id) + '/reversal-evidence',
      {credentials:'same-origin', cache:'no-store', signal:AbortSignal.timeout(10000)});
    const data = await response.json();
    if (!response.ok) throw Error(errorMessage(response,data));
    markup = refundEvidence(payment,data);
  } catch (err) {
    const reason = err instanceof TypeError || err instanceof SyntaxError || err.name === 'TimeoutError'
      ? 'The service could not supply current evidence.' : err.message;
    markup = '<div class="notice error"><strong>Refund evidence on hold</strong><p>' + html(reason) +
      '</p><p>Keep the case open and check again.</p></div>';
  }
  // Opening another order, the refund form or the journey replaces this element.
  if (dialog.open && output.isConnected) {
    output.innerHTML = markup;
    output.removeAttribute('aria-busy');
  }
}
function showPayment(id, openRefund = false) {
  const p = snapshot?.payments?.find((item) => item.id === id);
  if (!p) return;
  const remaining = Number.isSafeInteger(p.refundable) && !p.refund_balance_review ? p.refundable : 0;
  openRefund = openRefund && p.status === 'captured' && remaining > 0;
  const checkout = p.status === 'awaiting_customer' && safeHostedLink(p.checkout_url);
  const action = (checkout ? '<a class="btn primary" href="' + html(checkout) + '" target="_blank" rel="noopener noreferrer">Open hosted checkout</a>' : '') +
    (['unknown','awaiting_customer','authorized'].includes(p.status) ? button(p.status === 'unknown' ? 'Recover payment' : 'Check payment','recover',p.id,p.status === 'unknown') : '') +
    (p.status === 'captured' && remaining > 0 && !openRefund ? button('Refund payment','refund-form',p.id) : '') +
    (p.status === 'captured' && !openRefund ? button('Check refund evidence','evidence',p.id) : '');
  const note = p.status === 'unknown' ? 'The payment outcome is unknown. Check the original payment before collecting again.' :
    p.status === 'awaiting_customer' ? 'Complete the simulated checkout in its own tab, then check this payment.' :
    p.status === 'authorized' ? 'The acquirer authorized this amount. Wait for capture before releasing the order.' :
    p.status === 'expired' ? 'The acquirer confirmed this checkout closed unpaid.' : '';
  show(head(p.order_id,'Payment details') + badge(p.status) + detailsList([['Sale amount', money(p.amount)],['Channel',channelLabel(p.channel)],['Network',brands[p.brand] || p.brand],
    ...(p.terminal_id ? [['Terminal',p.terminal_id]] : []),['Payment ID',p.id],['Acquirer reference',p.reference || 'Awaiting confirmation'],['Refunded',money(p.refunded || 0)],
    ...(p.status === 'captured' ? [['Reserved for pending refunds',money(p.refund_reserved)],['Available to refund',money(remaining)]] : []),
    ...(p.refund_balance_review && p.refund_pending_requested !== p.refund_reserved ? [['Pending requests at face value',money(p.refund_pending_requested)]] : [])]) +
    (p.refund_balance_review ? '<p class="support">Refunds are under review. New refunds are paused while the original amounts are reconciled.</p>' : '') +
    (note ? '<p class="support">' + html(note) + '</p>' : '') + '<div class="actions detail-actions">' + action + '</div>' +
    (p.status === 'captured' && !openRefund ? '<section id="refund-evidence" data-id="' + html(p.id) + '" aria-live="polite"></section>' : '') +
    (openRefund ? '<form id="refund-form" data-id="' + html(p.id) + '"><label>Refund amount (USD)<input name="amount" inputmode="decimal" required placeholder="10.00"></label>' +
      '<p class="support">Available to refund ' + html(money(remaining)) + ' to the original card.</p><div class="actions"><button type="submit" class="btn primary">Issue refund</button></div></form>' : ''));
}

function request(action, path, label, body = {}, context = '') {
  return { action, path, label, body, context, key:uid() };
}
function errorMessage(response, data) {
  const msg = typeof data.error === 'string' ? data.error : data.error?.message || data.message;
  return msg || 'The service returned ' + response.status + '.';
}
function showJourney() {
  const terminal = snapshot.payments.find((p) => p.order_id === 'MIMI-GUIDED-TERMINAL');
  const online = snapshot.payments.find((p) => p.order_id === 'MIMI-GUIDED-ONLINE');
  const refunds = snapshot.refunds.filter((r) => r.payment_id === online?.id || r.payment_id === terminal?.id);
  const refs = new Set([terminal?.reference, online?.reference].filter(Boolean));
  const payouts = snapshot.payouts.filter((p) => p.lines?.some((l) => refs.has(l.reference)));
  const ids = new Set([terminal?.id, online?.id, ...refunds.map((r) => r.id), ...payouts.map((p) => p.id)].filter(Boolean));
  const journeyEvents = orderRecords(snapshot.events).reverse().filter((ev) => ids.has(ev.subject));
  show(head('Payment journey','Mimi grocery') + detailsList([
    ['Terminal sale',terminal ? money(terminal.amount) + ' · ' + (stateLabels[terminal.status] || terminal.status) : 'Awaiting sale'],
    ['Online sale',online ? money(online.amount) + ' · ' + (stateLabels[online.status] || online.status) : 'Awaiting sale'],
    ['Refunds confirmed',money(refunds.filter((r) => r.status === 'succeeded').reduce((total,r) => total+r.amount,0))],
    ['Matched at bank',money(payouts.filter((p) => p.status === 'matched').reduce((total,p) => total+p.net,0))]
  ]) + '<h3>Recorded events</h3>' + events(journeyEvents));
}
function success(req,data) {
  switch (req.action) {
    case 'sale':
      showPayment(data.id);
      return data.status === 'unknown' ? 'The response was lost. Check this payment to retrieve its original outcome.' :
        data.status === 'awaiting_customer' ? 'Online payment created. Continue to hosted checkout.' :
        data.status === 'authorized' ? 'Payment authorized. Waiting for capture.' :
        data.status === 'captured' ? 'Payment captured.' : 'Payment status recorded.';
    case 'recover':
      if (dialog.open) showPayment(req.context);
      return data.status === 'captured' ? 'Original payment verified as captured.' :
        data.status === 'expired' ? 'Original checkout verified as expired and unpaid.' : 'Payment status checked.';
    case 'recover-refund': return data.status === 'succeeded' ? 'Original refund verified.' :
      data.status === 'declined' ? 'The acquirer declined the original refund.' : 'Refund is awaiting confirmation.';
    case 'refund':
      if (dialog.open) showPayment(req.context);
      return data.status === 'unknown' ? 'Refund outcome pending. Open Payments and recover its original refund.' :
        data.status === 'declined' ? 'The acquirer declined this refund. Check the original payment’s refund history.' :
        'Refund recorded against the original payment.';
    case 'demo':
      return 'Journey complete. Both captures, the refund and the bank match are recorded below.';
    case 'settle':
      return 'Payout statement created. Post its bank entry to simulate the movement at Mimi’s bank.';
    case 'bank':
      return 'Bank posting submitted. Reconcile deposits to compare the independent entry with the payout.';
    case 'reconcile':
      return Number(data.matched) > 0 ? 'Matched ' + data.matched + (data.matched === 1 ? ' bank entry.' : ' bank entries.') :
        Number(data.pending) > 0 ? 'Still waiting for bank evidence on ' + data.pending + ' payout(s).' : 'Reconciliation complete. No new matches.';
    default: return 'Request completed.';
  }
}
async function execute(req, replay = false) {
  if (busy || !req) return;
  if (pending && !replay) { flash('Retry the unconfirmed request first.',true); return; }
  busy = true;
  remember(req);
  renderRecovery();
  protectButtons(true);
  const inside = $('#dialog-notice');
  if (inside) inside.textContent = '';
  $('#notice').textContent = '';
  try {
    let response;
    try {
      response = await fetch(req.path,{method:'POST',credentials:'same-origin',
        headers:{'Content-Type':'application/json','Idempotency-Key':req.key},body:JSON.stringify(req.body)});
    } catch {
      throw Error('The service reply was interrupted. Reuse the original request to retrieve its outcome.');
    }
    let data = {};
    try { data = await response.json(); } catch { /* An empty body is valid for an acknowledgement. */ }
    if (!response.ok) {
      if (response.status >= 400 && response.status < 500 && ![408,425,429].includes(response.status)) remember(null);
      if (req.action === 'settle' && response.status === 409) {
        // The processor may have frozen a statement which the local import held.
        try { await read(); } catch { throw Error(errorMessage(response,data) + ' Refresh Deposits to check the statement.'); }
      }
      throw Error(errorMessage(response,data));
    }
    remember(null);
    try { await read(); } catch { throw Error('The request was acknowledged. Refresh the workspace to retrieve its records.'); }
    flash(success(req,data));
  } catch (err) {
    flash(err.message || 'The request could not be completed.',true);
  } finally {
    busy = false;
    renderRecovery();
    protectButtons(false);
  }
}
document.addEventListener('click',(event) => {
  const el = event.target.closest('[data-action]');
  if (!el) return;
  const action = el.dataset.action, id=el.dataset.id || '';
  if (action === 'close') { dialog.close(); return; }
  if (busy) return;
  switch (action) {
    case 'journey': showJourney(); return;
    case 'new': newSale(); return;
    case 'detail': showPayment(id); return;
    case 'fee-report': void showFeeReport(id); return;
    case 'timing': void showTimingQueue(); return;
    case 'timing-observe': void showTimingQueue({},true); return;
    case 'evidence': void checkRefundEvidence(id); return;
    case 'refund-form': showPayment(id,true); return;
    case 'retry': void execute(pending,true); return;
    case 'refresh':
      void refreshWithFeedback(); return;
    case 'recover-refund': void execute(request(action,'/v1/refunds/'+encode(id)+'/recover','Original refund lookup',{},id)); return;
    case 'recover': void execute(request(action,'/v1/payments/'+encode(id)+'/recover','Payment status check',{},id)); return;
    case 'demo': void execute(request(action,'/app/demo','Sample payment journey')); return;
    case 'settle': void execute(request(action,'/v1/settle','New payout')); return;
    case 'bank': void execute(request(action,'/app/bank-post/'+encode(id),'Bank statement posting',{},id)); return;
    case 'reconcile': void execute(request(action,'/v1/reconcile','Bank reconciliation')); return;
  }
});
document.addEventListener('change',(event) => {
  if (event.target.name !== 'channel' || !event.target.closest('#sale-form')) return;
  const online = event.target.value === 'online';
  $('#loss-field').hidden = online;
  $('#loss-field input').disabled = online;
  $('#channel-help').textContent = online ? 'A hosted simulator page will open for customer approval.' :
    'Terminal MIMI-01 uses a simulated card. No card details are required.';
});
document.addEventListener('submit',(event) => {
  const f=event.target;
  if (f.id === 'timing-replay-form') {
    event.preventDefault();
    try {
      const known = timingFilter(f.elements.known.value);
      const effective = f.elements.effective.value.trim() ? timingFilter(f.elements.effective.value) : '';
      void showTimingQueue({known,effective});
    } catch (err) { flash(err.message,true); }
    return;
  }
  if (!['sale-form','refund-form'].includes(f.id)) return;
  event.preventDefault();
  if (busy) return;
  try {
    const amount=amountFrom(f.elements.namedItem('amount'));
    if (f.id === 'sale-form') {
      const channel=f.elements.channel.value;
      const body={order_id:f.elements.order.value.trim(),channel,amount,currency:'USD',brand:f.elements.brand.value,
        terminal_id:channel==='terminal'?'MIMI-01':'',lose_response:channel==='terminal' && f.elements.loss.checked};
      if (!body.order_id) throw Error('Enter an order reference.');
      void execute(request('sale','/v1/payments','Create sale for '+body.order_id,body));
    } else {
      const p=snapshot.payments.find((x) => x.id===f.dataset.id);
      if (!p || p.refund_balance_review || !Number.isSafeInteger(p.refundable) || amount > p.refundable) throw Error('Refund amount exceeds the amount available after pending reservations.');
      void execute(request('refund','/v1/payments/'+encode(p.id)+'/refunds','Refund payment '+p.order_id,{amount},p.id));
    }
  } catch(err) { flash(err.message,true); }
});
async function refreshWithFeedback() {
  if (busy) return;
  try { await read(); }
  catch(err) { flash(err.message,true); if (!snapshot) view.innerHTML='<div class="empty">' + button('Retry loading records','refresh') + '</div>'; }
}
window.addEventListener('hashchange',() => {
  if (dialog.open) dialog.close();
  render();
  window.scrollTo({top:0,behavior:'instant'});
});
window.addEventListener('focus',() => { if (!busy) void refreshWithFeedback(); });
render();
renderRecovery();
void refreshWithFeedback();
