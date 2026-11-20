"use strict";
const el = id => document.getElementById(id);
let token = "", command = null, generation = 0, busy = false, timer;
const fragment = new URLSearchParams(location.hash.slice(1));
const initial = fragment.get("token");
if (location.hash) history.replaceState(null, "", location.pathname + location.search);
function money(f) {
  const format = new Intl.NumberFormat(undefined, {style: "currency", currency: f.currency, currencyDisplay: "code"});
  return format.format(f.amount_minor / 10 ** format.resolvedOptions().maximumFractionDigits);
}
async function request(path, decision) {
  const options = {cache:"no-store", credentials:"omit", redirect:"error", headers: {Authorization:"Bearer "+token}};
  if (decision) {
    const input={decision};
    if(decision==="approve"&&command.payment_method)input.card_brand=el("instrument").value;
    options.method="POST";options.headers["Content-Type"]="application/json";options.body=JSON.stringify(input);
  }
  const result=await fetch(path,options);
  const value=await result.json();
  if (!result.ok) throw new Error(value.error || "Reader unavailable.");
  return value;
}
function clear(){
  generation++; clearTimeout(timer); token=""; command=null; busy=false;
  el("reader").hidden=true; el("sign-in").hidden=false; el("receipt").hidden=true;
  el("error").textContent=""; el("credential").value=""; el("location").textContent="";
}
async function poll(g) {
  if (g!==generation || busy || !token) return;
  try {
    const data=await request("/device/v1/command");
    if(g!==generation || busy) return;
    const previous=command; command=data.command; el("error").textContent=""; el("sign-in").hidden=true; el("reader").hidden=false;
    el("location").textContent=data.merchant_id+" / "+data.outlet_id+" / "+data.terminal_id;
    el("idle").hidden=!!command; el("pending").hidden=!command;
    if(command){
      el("amount").textContent=money(command);el("order").textContent=command.order_id;el("op").textContent=command.operation_id;
      el("card-choice").hidden=!command.payment_method;
      if(!previous || previous.operation_id!==command.operation_id)el("instrument").value=command.payment_method==="card"?"visa":command.payment_method;
    }
  } catch(e) {
    if(g!==generation)return;
    // Conditions must be observed again before a decision is enabled.
    command=null; el("pending").hidden=true; el("idle").hidden=true;
    el("error").textContent=e.message;
    if(el("reader").hidden){token="";return;}
  }
  if(g===generation && !busy && token)timer=setTimeout(()=>poll(g),1400);
}
function connect(value){clear();token=value;poll(generation);}
async function decide(decision){
  if(!command||busy)return;
  const id=command.operation_id;
  clearTimeout(timer); busy=true; const g=++generation; el("approve").disabled=el("decline").disabled=true;
  try{
    const result=await request("/device/v1/commands/"+encodeURIComponent(id)+"/decision",decision);
    if(g!==generation)return;
    el("outcome").textContent=money(result)+" · "+(result.state==="captured"?"Approved":"Declined")+" · "+result.order_id;
    el("receipt").hidden=false; el("error").textContent="";
  }catch(e){if(g===generation)el("error").textContent=e.message;}
  finally{
    if(g===generation){busy=false;command=null;el("approve").disabled=el("decline").disabled=false;await poll(g);}
  }
}
el("connect").addEventListener("submit",e=>{e.preventDefault();connect(el("credential").value.trim());});
el("disconnect").addEventListener("click",()=>{clear();el("approve").disabled=el("decline").disabled=false;});
el("approve").addEventListener("click",()=>decide("approve"));
el("decline").addEventListener("click",()=>decide("decline"));
if(initial)connect(initial);
