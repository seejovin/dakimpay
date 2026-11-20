"use strict";
const el = id => document.getElementById(id);
const form = el("sale"), refundForm = el("refund");
let token = "", program = null, activeID = "", changing = false, running = false, refundID = "";
const id = () => crypto.randomUUID();
const fmt = (v, c) => new Intl.NumberFormat(undefined, {style:"currency",currency:c}).format(v / (10 ** digits(c)));
function digits(c) { return new Intl.NumberFormat("en", {style:"currency",currency:c}).resolvedOptions().maximumFractionDigits; }
function minor(s,c) {
    const places = digits(c);
    const m = /^(\d+)(?:\.(\d+))?$/.exec(s.trim());
    if (!m || (m[2] || "").length > places) throw Error("Enter a positive amount using the currency’s decimal places.");
    const n = Number(m[1] + (m[2] || "").padEnd(places,"0"));
    if (!Number.isSafeInteger(n) || n < 1 || n > program.max_amount) throw Error("Amount is outside this merchant’s limit.");
    return n;
}
function message(v="") {el("notice").textContent=v;}
function item(k) {return "dakimpay-v2:" + program.storage_scope + ":" + k;}
function load(k) {const s=localStorage.getItem(item(k));return s?JSON.parse(s):null;}
function save(k,v) {if(v===null)localStorage.removeItem(item(k));else localStorage.setItem(item(k),JSON.stringify(v));}
async function api(path, data, key) {
    const h = {Authorization:"Bearer "+token};
    if(key)h["Idempotency-Key"]=key;
    if(data)h["Content-Type"]="application/json";
    const res=await fetch(path,{headers:h,method:data?"POST":"GET",body:data?JSON.stringify(data):undefined,cache:"no-store",referrerPolicy:"no-referrer"});
    if(!res.ok){let msg;try{msg=(await res.json()).error}catch{};const e=Error(msg||("Request failed ("+res.status+")."));e.http=res.status;throw e;}
    return res.json();
}
function options(select, values) {select.replaceChildren(...values.map(v=>new Option(v,v)));}
const methodNames={card:"Card · any supported brand",visa:"Visa",mastercard:"Mastercard",amex:"American Express",wechat_pay:"WeChat Pay",alipay:"Alipay"};
let methodRequest=0;
function fillMethods(list){
    const sel=form.elements.method, current=sel.value;
    sel.replaceChildren(...list.map(v=>new Option(methodNames[v]||v,v)));
    if(list.includes(current))sel.value=current;
    el("method-label").hidden=!program.aggregate;
}
async function refreshMethods(){
    const version=++methodRequest;
    const channel=form.elements.channel.value;
    const full=(program.methods||{})[channel]||[];
    fillMethods(full);
    let n;
    try{n=minor(form.elements.amount.value,program.currency);}catch{return;}
    if(!program.aggregate)return;
    try{
        const page=await api("/v2/payment-methods?channel="+encodeURIComponent(channel)+"&amount_minor="+n);
        if(version!==methodRequest)return;
        fillMethods(page.methods);
        el("submit-sale").disabled=changing||!page.methods.length||(channel==="terminal"&&!form.elements.terminal.value);
    }catch(e){if(version===methodRequest)message(e.message);}
}
function updateReader(){
    const use=form.elements.channel.value==="terminal";
    el("device-label").hidden=!use;
    options(form.elements.terminal,Object.keys(program.terminals||{}).filter(k=>program.terminals[k]===form.elements.outlet.value));
    el("submit-sale").disabled=changing||(use&&!form.elements.terminal.value);
}
function newSale(){form.elements.order.value="order-"+id().slice(0,12);form.elements.amount.value="";}
function restoreDraft(){
    const pending=load("sale");
    if(!pending){newSale();return;}
    const x=pending.body;
    form.elements.order.value=x.order_id;form.elements.amount.value=(x.amount_minor/10**digits(x.currency)).toFixed(digits(x.currency));
    form.elements.channel.value=x.channel;form.elements.outlet.value=x.outlet_id;fillMethods((program.methods||{})[x.channel]||[]);if(x.payment_method)form.elements.method.value=x.payment_method;updateReader();if(x.terminal_id)form.elements.terminal.value=x.terminal_id;
    message("The last request has no confirmed reply. Submit the same order to recover it.");
}
function display(p) {
    if(p.id!==activeID)return;
    el("empty").hidden=true;el("detail").hidden=false;el("status").textContent=p.status;
    el("total").textContent=fmt(p.amount_minor,p.currency);el("order").textContent=p.order_id;
    el("payment-id").textContent=p.id;el("reference").textContent=p.reference||"Pending";
    el("refunded").textContent=fmt(p.refunded_minor,p.currency)+" / "+fmt(p.reserved_refund_minor,p.currency);
    const labels={queued:"The instruction is recorded. Waiting for dispatch.",pending:p.channel==="terminal"?"Present this payment on the assigned virtual reader.":"Open the buyer checkout to complete this order.",unknown:"Checking the original instruction. Keep this tender open.",captured:"The processor confirmed capture.",declined:"The processor declined this payment.",review:"The payment requires investigation."};
    el("instruction").textContent=labels[p.status]||p.status;
    for(const k of ["method-title","method-value","route-title","route-value"])el(k).hidden=!p.route_id;
    el("method-value").textContent=[methodNames[p.payment_method]||p.payment_method,p.card_brand&&p.card_brand!==p.payment_method?methodNames[p.card_brand]||p.card_brand:null].filter(Boolean).join(" / ");
    el("route-value").textContent=p.route_id||"";
    const a=el("checkout");a.removeAttribute("href");a.hidden=true;
    if(p.status==="pending"&&p.checkout_url){
        const target=new URL(p.checkout_url);
        const policy=program.checkout_origins[p.connector_id||""];
        const local=target.origin===policy&&target.protocol==="http:"&&target.hostname==="127.0.0.1"&&target.pathname.startsWith("/checkout/");
        const stripe=policy===target.origin&&target.origin==="https://checkout.stripe.com"&&target.pathname.startsWith("/c/pay/cs_test_");
        const antom=policy==="antom_test"&&(["https://render.alipay.com","https://global.alipay.com"].includes(target.origin)||(target.origin==="https://ac.alipay.com"&&target.pathname==="/page/sandbox/stone-infostaging.html"));
        if(!target.username&&!target.password&&(local||stripe||antom)){a.href=target.href;a.hidden=false;}
    }
    refundForm.hidden=p.status!=="captured"||p.amount_minor-p.reserved_refund_minor-p.refunded_minor<=0;
}
async function selectPayment(pid){const p=await api("/v2/payment-intents/"+encodeURIComponent(pid));activeID=p.id;save("selected",activeID);display(p);}
let cursor="0", feed=[];
async function events(){
    const page=await api("/v2/events?limit=100&after="+cursor);
    const rows=page.events||[];
    for(const e of rows)feed.unshift(e);
    cursor=page.next_cursor;
    feed=feed.slice(0,15);
    const host=el("events");host.replaceChildren();
    if(!feed.length){host.textContent="No payment activity yet.";return;}
    for(const e of feed){
        const row=document.createElement("div");row.className="event";
        for(const [n,txt] of [e.kind,e.status,e.operation_id].entries()){const c=document.createElement("span");c.textContent=txt;if(n===2)c.className="opid";row.append(c);}
        const b=document.createElement("button");b.className="quiet";b.textContent="Open";b.onclick=()=>selectPayment(e.parent_id||e.operation_id).catch(x=>message(x.message));row.append(b);host.append(row);
    }
}
function clearSelection(){
    activeID="";save("selected",null);el("empty").hidden=false;el("detail").hidden=true;
}
async function refresh(){
    if(running||!program||changing)return;running=true;
    try{
        if(activeID){
            try{display(await api("/v2/payment-intents/"+encodeURIComponent(activeID)));}
            catch(e){if(e.http===404)clearSelection();else message("Payment status: "+e.message);}
        }
        if(refundID){
            try{
                const r=await api("/v2/refunds/"+encodeURIComponent(refundID));
                if(["refunded","declined","review"].includes(r.status)){message("Refund "+r.id+" · "+r.status);refundID="";save("refund-watch",null);}
            }catch(e){if(e.http===404){refundID="";save("refund-watch",null);}else message("Refund status: "+e.message);}
        }
        await events();
    }catch(e){message("Activity: "+e.message);}finally{running=false;}
}
async function recoverSale(){
    const pending=load("sale");
    if(!pending)return;
    try{
        const original=await api(pending.path,pending.body,pending.key);
        activeID=original.id;save("selected",activeID);save("sale",null);display(original);newSale();
        message("Recovered payment "+original.id+" · "+original.status);
    }catch(e){
        if([400,403,404,409,422].includes(e.http))save("sale",null);
        message("Original payment recovery: "+e.message);
    }
}
async function recoverRefund(){
    const pending=load("refund");
    if(!pending)return;
    // Replay the persisted original command even when the payment's remaining balance is now zero.
    try{
        const r=await api(pending.path,pending.body,pending.key);
        refundID=r.id;save("refund-watch",r.id);save("refund",null);
        message("Recovered refund "+r.id+" · "+r.status);
    }catch(e){
        if([400,403,404,409,422].includes(e.http))save("refund",null);
        message("Original refund recovery: "+e.message);
    }
}
async function connect(raw){
    token=raw;program=await api("/ui/profile");el("token").value="";
    el("login").hidden=true;el("register").hidden=false;el("disconnect").hidden=false;
    el("merchant").textContent=program.merchant;el("currency").textContent=program.currency;el("units").textContent="("+program.currency+")";
    options(form.elements.outlet,Object.keys(program.outlets));updateReader();activeID=load("selected")||"";refundID=load("refund-watch")||"";fillMethods((program.methods||{})[form.elements.channel.value]||[]);restoreDraft();await recoverSale();await recoverRefund();await refresh();
}
async function command(slot, path, body){
    // A lost POST response is always retried with the original key and financial terms.
    const prior=load(slot);
    if(prior&&(prior.path!==path||JSON.stringify(prior.body)!==JSON.stringify(body)))throw Error("Resolve the previous "+slot+" request with its original terms before creating another.");
    const request=prior||{path,body,key:id()};save(slot,request);
    try{
        const v=await api(path,body,request.key);return v;
    }catch(e){
        if([400,403,404,409,422].includes(e.http))save(slot,null);
        throw e;
    }
}
form.onsubmit=async e=>{
    e.preventDefault();if(changing)return;changing=true;updateReader();message();
    try{
        const f=form.elements;
        const body={order_id:f.order.value.trim(),allocation_id:"primary",outlet_id:f.outlet.value,channel:f.channel.value,amount_minor:minor(f.amount.value,program.currency),currency:program.currency};
        if(program.aggregate){
            if(!f.method.value)throw Error("No payment method is enrolled for these terms.");
            body.payment_method=f.method.value;
        }
        if(body.channel==="terminal")body.terminal_id=f.terminal.value;
        const p=await command("sale","/v2/payment-intents",body);
        activeID=p.id;save("selected",p.id);save("sale",null);display(p);newSale();
    }catch(err){message(err.message);}finally{changing=false;updateReader();await refresh();}
};
refundForm.onsubmit=async e=>{
    e.preventDefault();if(changing)return;changing=true;refundForm.querySelector("button").disabled=true;
    try{
        const prior=load("refund");
        const body={merchant_refund_id:prior?prior.body.merchant_refund_id:"rf-"+id(),amount_minor:minor(refundForm.elements.amount.value,program.currency)};
        const r=await command("refund","/v2/payment-intents/"+encodeURIComponent(activeID)+"/refunds",body);
        refundID=r.id;save("refund-watch",refundID);save("refund",null);refundForm.reset();message("Refund recorded · "+r.id);
    }catch(err){message(err.message);}finally{changing=false;refundForm.querySelector("button").disabled=false;await refresh();}
};
el("lookup").onsubmit=e=>{e.preventDefault();selectPayment(e.target.elements.id.value.trim()).catch(e=>message(e.message));};
el("login-form").onsubmit=e=>{e.preventDefault();connect(el("token").value.trim()).catch(e=>message(e.message));};
form.elements.channel.onchange=()=>{updateReader();refreshMethods();};
form.elements.amount.onchange=refreshMethods;form.elements.outlet.onchange=updateReader;el("refresh").onclick=refresh;
el("disconnect").onclick=()=>location.reload();
const fragment=new URLSearchParams(location.hash.slice(1));const seeded=fragment.get("token");
if(location.hash)history.replaceState(null,"",location.pathname);
if(seeded)connect(seeded).catch(e=>message(e.message));
setInterval(refresh,2000);
