"use strict";

// Never store credentials or CSRF tokens in browser storage.
let settings = null;
const form = document.getElementById("setup-form");
const message = document.getElementById("setup-message");
const fields = [
 ["elasticsearch_url", "Elasticsearch URL", "url", "standalone"],
 ["username", "Setup username", "text", "standalone"],
 ["password", "Setup password", "password", "standalone"],
 ["api_key", "API key (alternative to username and password)", "password", "standalone"],
 ["fleet_url", "Fleet Server URL", "url", "fleet"],
 ["enrollment_token", "Enrollment token", "password", "fleet"],
 ["ca_certificate", "CA certificate (PEM or file in /ssl)", "textarea"],
 ["tags", "Tags (one per line)", "textarea"],
 ["collect_core", "Collect Home Assistant Core logs", "checkbox", "standalone"],
 ["collect_supervisor", "Collect Supervisor logs", "checkbox", "standalone"],
 ["collect_apps", "Collect app logs", "checkbox", "standalone"],
 ["collect_host", "Collect host logs", "checkbox", "standalone"],
 ["read_existing_logs", "Read existing logs on first collection", "checkbox", "standalone"],
 ["insecure", "Disable certificate verification (testing only)", "checkbox"],
];
function setupMode() {
 const mode = document.getElementById("setup-mode").value;
 for (const row of form.querySelectorAll("[data-mode]")) row.hidden = row.dataset.mode !== mode;
}
async function loadSettings() {
 try {
  const response = await fetch("api/config", {cache:"no-store"});
  if (!response.ok) throw new Error(await response.text());
  settings = await response.json();
  document.getElementById("setup-fields").replaceChildren();
  for (const [key,label,type,mode] of fields) {
   const row = document.createElement("div"); row.className="setup-field";
   if (mode) row.dataset.mode=mode;
   const caption=document.createElement("label"); caption.htmlFor="setting-"+key; caption.textContent=label;
   const input=document.createElement(type==="textarea"?"textarea":"input");
   input.id="setting-"+key; input.name=key;
   if (type!=="textarea") input.type=type==="url"?"text":type;
   if (type==="checkbox") input.checked=settings.options[key] ?? key.startsWith("collect_");
   else if (type==="password") {
    input.autocomplete="new-password";
    input.placeholder=settings.configured_secrets[key]?"Configured — leave blank to keep":"Not configured";
   } else input.value=key==="tags"?(settings.options[key]||[]).join("\n"):(settings.options[key]||"");
   if (type==="url" && settings.configured_urls[key] && !input.value) input.placeholder="Configured — leave blank to keep, or enter a replacement";
   row.append(caption,input);
   if (type==="password" || type==="url") {
    const clearLabel=document.createElement("label"); clearLabel.className="clear-secret";
    const clear=document.createElement("input"); clear.type="checkbox"; clear.id="clear-"+key;
    clear.addEventListener("change",()=>{input.disabled=clear.checked;if(clear.checked)input.value="";});
    clearLabel.append(clear,document.createTextNode(type==="url"?"Clear saved URL":"Clear saved credential")); row.append(clearLabel);
   }
   document.getElementById("setup-fields").append(row);
  }
  document.getElementById("setup-mode").value=settings.mode;
  setupMode(); form.hidden=false; message.textContent="";
 } catch(err) {form.hidden=true;message.textContent=err.message || "Could not load settings.";}
}
document.getElementById("setup-mode").addEventListener("change",setupMode);
document.getElementById("reload-settings").addEventListener("click",loadSettings);
form.addEventListener("submit",async(event)=>{
 event.preventDefault(); if(!settings)return;
 const button=document.getElementById("save-settings");button.disabled=true;
 const options={};const mode=document.getElementById("setup-mode").value;
 for(const [key,,type,fieldMode] of fields){
  const input=document.getElementById("setting-"+key);
  if(fieldMode && fieldMode!==mode){
   if(["elasticsearch_url","username","password","api_key","fleet_url","enrollment_token"].includes(key)) options[key]="";
   continue;
  }
  if(type==="url"){
   if(document.getElementById("clear-"+key).checked)options[key]="";
   else if(input.value.trim() !== (settings.options[key]||"")) options[key]=input.value.trim();
  } else if(type==="password"){
   if(document.getElementById("clear-"+key).checked)options[key]="";
   else if(input.value)options[key]=input.value;
  } else if(type==="checkbox")options[key]=input.checked;
  else options[key]=key==="tags"?input.value.split("\n").map(t=>t.trim()).filter(Boolean):input.value.trim();
 }
 const headers={"Content-Type":"application/json","X-CSRF-Token":settings.csrf_token};
 try{
  message.textContent="Saving settings…";
  const response=await fetch("api/config",{method:"POST",headers,body:JSON.stringify({revision:settings.revision,options})});
  if(!response.ok)throw new Error(await response.text());
  // Clear typed secrets as soon as persistence is confirmed.
  for(const input of form.querySelectorAll('input[type="password"]'))input.value="";
  form.hidden=true;
  message.textContent="Settings saved. Requesting restart…";
  try{
   const restart=await fetch("api/restart",{method:"POST",headers,body:"{}"});
   if(!restart.ok)throw new Error("Restart not confirmed");
   message.textContent="Settings saved. Restart requested. Reload this page once the app is available.";
  }catch{
   message.textContent="Settings saved. The restart connection closed or could not be confirmed. Check status; if needed, restart from Home Assistant. Then reload this page.";
  }
  settings=null;
 }catch(err){message.textContent=err.message || "Could not save settings.";}
 finally{button.disabled=false;}
});
loadSettings();
