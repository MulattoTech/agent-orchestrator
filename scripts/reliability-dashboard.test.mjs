// Offline DOM regression: every fetch is a local fixture; no browser/provider/network process.
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { pathToFileURL } from 'node:url';
const require=createRequire(new URL('../frontend/package.json',import.meta.url));
const { JSDOM }=await import(pathToFileURL(require.resolve('jsdom')).href);
const html=await readFile(new URL('../backend/internal/httpd/reliability.html',import.meta.url),'utf8');
function fixture(){
 return {observedAt:'2026-11-16T00:00:00Z',allowanceCheckEnabled:false,
 admission:{limits:{registeredSlots:40,maxActiveWorkers:8,defaultProjectWorkers:4,projectWorkers:{red:4},queueEnabled:true},
 activeWorkerSessions:8,provisioningWorkers:2,runningToolProcesses:null,queuedRequests:32,resourceWorkerCeiling:8},
 inference:{quota:'UNKNOWN',active:1,uncertain:0,inference_cap:2,project_cap:1,reviewer_cap:1,
 conservative_turns_remaining:3,reserve_turns:1,telemetry_confidence:'Native receipts; true allowance UNKNOWN',
 models:[{model:'sol',effort:'medium',turns:17,failed:2,receipts:20,input_tokens:1000,cached_input_tokens:800,output_tokens:50,reasoning_output_tokens:20}]},
 controllerHealth:{status:'DEGRADED'},mergedPRs:4,acceptedRequirements:null,idleWaste:null,retryAttribution:'Unknown',
 queue:[{id:'q1',projectId:'red',state:'queued',attempts:0}]};
}
const tick=()=>new Promise(resolve=>setImmediate(resolve));
async function mount(t,data=fixture()){
 const requests=[];
 const dom=new JSDOM(html,{url:'http://127.0.0.1:19421/internal/reliability/dashboard',runScripts:'dangerously',
 beforeParse(w){
  w.setInterval=()=>0;
  w.fetch=async(path,options={})=>{
   assert.ok(String(path).startsWith('/internal/reliability'),'unexpected network access');
   const body=options.body?JSON.parse(options.body):undefined;
   requests.push({path,method:options.method,body});
   if(body&&path.endsWith('/inference')){
    if(body.pause!==undefined)data.inference.paused=body.pause;
    if(body.emergency_stop!==undefined)data.inference.emergency_stop=body.emergency_stop;
   }
   if(body&&path.endsWith('/workers'))data.admission.limits=body;
   return {ok:true,status:body?204:200,json:async()=>structuredClone(data),text:async()=>''};
  };
 }});
 t.after(()=>dom.window.close());
 await tick();await tick();
 return {window:dom.window,document:dom.window.document,requests};
}
test('shows separate slots, worker occupancy and active inference; unknown never becomes zero',async t=>{
 const {document:d,requests}=await mount(t);
 const metrics=Object.fromEntries([...d.querySelectorAll('.metric')].map(m=>[m.querySelector('small').textContent,m.querySelector('.value').textContent]));
 assert.equal(metrics['Registered slots'],'40');assert.equal(metrics['Active worker sessions'],'8');
 assert.equal(metrics['Active inference'],'1');assert.equal(metrics['Running tool processes'],'Unknown');
 assert.match(d.querySelector('#quality').textContent,/DEGRADED/);
 assert.match(d.querySelector('#outcomes').textContent,/Idle inference waste: Unknown/);
 assert.ok(d.querySelector('#verifyAllowance').disabled);
 assert.equal(requests.length,1);
});
test('worker and inference actions write distinct controls and preserve project settings',async t=>{
 const {window:w,document:d,requests}=await mount(t);
 d.querySelector('#workers').value='4';
 d.querySelector('#workerForm').dispatchEvent(new w.Event('submit',{bubbles:true,cancelable:true}));
 await tick();await tick();
 const workers=requests.find(r=>r.path.endsWith('/workers'));
 assert.equal(workers.body.maxActiveWorkers,4);assert.equal(workers.body.registeredSlots,40);
 assert.deepEqual(workers.body.projectWorkers,{red:4});assert.equal(workers.body.maximum_inference,undefined);
 d.querySelector('#emergency').click();await tick();await tick();
 const stop=requests.find(r=>r.body?.emergency_stop===true);
 assert.equal(stop.path,'/internal/reliability/inference');
 assert.equal(d.querySelector('#status').textContent,'Emergency stop');
 assert.ok(!requests.some(r=>r.path.includes('/allowance/verify')));
});
test('native model labels are plain text and policy alarms outrank quota status',async t=>{
 const data=fixture();data.inference.models[0].model='<img src=x onerror="window.pwned=true">';
 data.inference.policy_stop=true;data.inference.quota='POLICY_VIOLATION';
 const {window:w,document:d}=await mount(t,data);
 assert.equal(d.querySelectorAll('img').length,0);assert.equal(w.pwned,undefined);
 assert.equal(d.querySelector('#status').textContent,'Policy violation');
 assert.match(d.querySelector('#models').textContent,/800/);
});
