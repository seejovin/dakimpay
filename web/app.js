'use strict';

const $ = (selector, root = document) => root.querySelector(selector);
const view = $('#view');
const dialog = $('#modal');
const moneyFormatter = new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD' });
const dateFormatter = new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
const pages = { overview: 'Merchant overview', payments: 'Payments', deposits: 'Bank deposits', activity: 'Activity' };
const brands = { visa: 'Visa', mastercard: 'Mastercard', amex: 'American Express' };
const stateLabels = { unknown: 'Needs recovery', awaiting_customer: 'Awaiting customer', captured: 'Captured',
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
    ['in_transit','awaiting_customer','pending'].includes(status) ? 'wait' : '';
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
      html(date(r.created_at)) + '</small></div><div class="right">' + html(money(r.amount)) + '<small class="sub">' +
      html(stateLabels[r.status] || r.status) + '</small>' + (r.status === 'unknown' ? button('Recover refund','recover-refund',r.id) : '') + '</div></li>';
  }).join('') + '</ul>';
}
function events(rows) {
  if (!rows.length) return empty('The timeline is clear', 'Payment, recovery and settlement events will appear here as they are recorded.');
  return '<ol class="event-list">' + rows.map((e) => '<li><i class="event-dot" aria-hidden="true"></i><div><strong>' +
    html(e.message || e.kind) + '</strong><small>' + html([date(e.created_at),shortRef(e.subject)].filter(Boolean).join(' · ')) +
    '</small></div></li>').join('') + '</ol>';
}
function payoutLines(lines) {
  return '<ul class="flow-list">' + list(lines).map((line) => '<li><div><strong>' +
    html(({capture:'Capture',refund:'Refund',fee:'Processing fee'})[line.kind] || line.kind) + '</strong><small class="sub">' +
    html(line.reference) + '</small></div><span class="amount">' + html(money(line.amount)) + '</span></li>').join('') + '</ul>';
}
function payouts(records) {
  if (!records.length) return empty('No payouts recorded', 'Create a payout after a sale. Post its bank entry and reconcile the deposit to match it.');
  return records.map((p) => '<details class="payout-item"><summary>' + html(shortRef(p.id)) + ' <span class="amount">' + html(money(p.net)) +
    '</span> ' + badge(p.status) + '</summary><div class="payout-detail"><p class="support">Bank trace ' + html(p.bank_trace) + '</p>' +
    payoutLines(p.lines) + (p.status === 'matched' ? '<a class="link" href="/v1/payouts/' + encode(p.id) + '/statement" download="mimi-statement-' + html(p.id) + '.json">Download Mimi statement</a>' : '') + '</div></details>' +
    (p.status === 'in_transit' ? '<div class="payout-controls">' + button('Post bank entry','bank',p.bank_trace) + '</div>' : '')).join('');
}
function overview() {
  const t = snapshot.totals;
  const records = orderRecords(snapshot.payments), deposits = orderRecords(snapshot.payouts), history = orderRecords(snapshot.events);
  const matched = deposits.filter((p) => p.status === 'matched').length;
  const hasJourney = records.some((p) => p.order_id === 'MIMI-GUIDED-TERMINAL');
  return '<div class="hero"><section class="account"><div><p class="overline">Matched bank deposits</p><p class="account-total">' +
    html(money(t.deposited)) + '</p></div><div class="account-foot"><span>Mimi settlement account</span><span>' +
    matched + (matched === 1 ? ' deposit' : ' deposits') + '</span></div></section>' +
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
  if (page === 'deposits') view.innerHTML = '<section class="panel">' + sectionHead('Payout statements','Match acquirer lines to the independently posted bank entry.',
    button('Create payout','settle') + button('Reconcile deposits','reconcile','',true)) + payouts(orderRecords(snapshot.payouts)) + '</section>';
  if (page === 'activity') view.innerHTML = '<section class="panel">' + sectionHead('Transaction timeline','Events from payment collection through to bank reconciliation.') +
    events(orderRecords(snapshot.events)) + '</section>';
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
function showPayment(id, openRefund = false) {
  const p = snapshot?.payments?.find((item) => item.id === id);
  if (!p) return;
  const remaining = p.amount - (p.refunded || 0);
  const checkout = p.status === 'awaiting_customer' && safeHostedLink(p.checkout_url);
  const action = (checkout ? '<a class="btn primary" href="' + html(checkout) + '" target="_blank" rel="noopener noreferrer">Open hosted checkout</a>' : '') +
    (['unknown','awaiting_customer'].includes(p.status) ? button(p.status === 'unknown' ? 'Recover payment' : 'Check payment','recover',p.id,p.status === 'unknown') : '') +
    (p.status === 'captured' && remaining > 0 && !openRefund ? button('Refund payment','refund-form',p.id) : '');
  const note = p.status === 'unknown' ? 'The terminal reply was lost. Recover the original payment before collecting again.' :
    p.status === 'awaiting_customer' ? 'Complete the simulated checkout in its own tab, then check this payment.' : '';
  show(head(p.order_id,'Payment details') + badge(p.status) + detailsList([['Sale amount', money(p.amount)],['Channel',channelLabel(p.channel)],['Network',brands[p.brand] || p.brand],
    ...(p.terminal_id ? [['Terminal',p.terminal_id]] : []),['Payment ID',p.id],['Acquirer reference',p.reference || 'Awaiting confirmation'],['Refunded',money(p.refunded || 0)]]) +
    (note ? '<p class="support">' + html(note) + '</p>' : '') + '<div class="actions detail-actions">' + action + '</div>' +
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
      return data.status === 'unknown' ? 'The terminal response was lost. Recover this payment to inspect the original charge.' :
        data.status === 'awaiting_customer' ? 'Online payment created. Continue to hosted checkout.' : 'Terminal payment captured.';
    case 'recover':
      if (dialog.open) showPayment(req.context);
      return data.status === 'captured' ? 'Original payment verified as captured.' : 'Payment status checked.';
    case 'recover-refund': return data.status === 'succeeded' ? 'Original refund verified.' : 'Refund is awaiting confirmation.';
    case 'refund':
      if (dialog.open) showPayment(req.context);
      return data.status === 'unknown' ? 'Refund outcome pending. Open Payments and recover its original refund.' : 'Refund recorded against the original payment.';
    case 'demo':
      return 'Journey complete. Both captures, the refund and the bank match are recorded below.';
    case 'settle':
      return 'Payout statement created. Post its bank entry to simulate arrival at Mimi’s bank.';
    case 'bank':
      return 'Bank posting submitted. Reconcile deposits to compare the independent entry with the payout.';
    case 'reconcile':
      return Number(data.matched) > 0 ? 'Matched ' + data.matched + (data.matched === 1 ? ' bank deposit.' : ' bank deposits.') :
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
      if (!p || amount>p.amount-(p.refunded||0)) throw Error('Refund amount exceeds the remaining captured amount.');
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
