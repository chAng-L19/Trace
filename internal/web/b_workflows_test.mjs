import assert from 'node:assert/strict';
import {webcrypto} from 'node:crypto';
import {readCreationOptions,parseRunRoute,runLink,pendingMessages,moveQueuedMessage,createRunManagement,requirementsForm} from './static/run-management.js';
import {projectHistory,replayEvents,mountGraphPan} from './static/history-projection.js';
import {artifactReference,mediaKind,mediaMarkup,usagePanel,createDelivery} from './static/delivery.js';
import {markdown} from './static/presentation.js';
import {conversation} from './static/conversation.js';
import {settings,readConfigForm} from './static/settings.js';
import fs from 'node:fs';
import vm from 'node:vm';

const zero='0001-01-01T00:00:00Z';
const queue={mailbox:[{id:'a',kind:'steer',cancelled_at:zero,consumed_at:zero},{id:'b',kind:'follow_up'},{id:'c',kind:'steer'},{id:'d',kind:'steer',message_id:'delivered'},{id:'e',kind:'steer',cancelled_at:'2026-10-23T00:00:00Z'}]};
assert.deepEqual(pendingMessages(queue).map(m=>m.id),['a','b','c']);
assert.deepEqual(moveQueuedMessage(queue,'a',1),['c','b','a']);
assert.equal(moveQueuedMessage(queue,'b',1),null);
assert.deepEqual(queue.mailbox.map(m=>m.id),['a','b','c','d','e']);
const form={elements:Object.fromEntries(['max_model_requests','max_actions','max_seconds'].map(k=>[k,{value:'0'}])),querySelector:q=>({value:q==='#new-title'?'title':'marker',checked:q==='#new-budget-enabled'})};
assert.deepEqual(readCreationOptions(form).budget,{max_model_requests:0,max_actions:0,max_seconds:0});
const link=runLink('run:a',{tab:'events',message:'m?1',request:'r:2',call:'c/3',event:19}),route=parseRunRoute(link.slice(1));
assert.equal(route.runID,'run:a');assert.equal(route.deepLink.message,'m?1');assert.equal(route.deepLink.call,'c/3');assert.equal(route.deepLink.tab,'events');
assert.equal(parseRunRoute('history/%wrong').historyDetail,false);
assert.match(requirementsForm({run:{status:'running'},requests:[]}),/disabled/);

const snap={run:{id:'r',goal:'goal',status:'completed'},result:{summary:'future conclusion'},findings:[{id:'future',title:'future secret'}],plan:{tasks:[{id:'future-task'}]},events:[
  {sequence:1,type:'run_created'},
  {sequence:2,type:'plan_updated',data:{tasks:[{id:'t',status:'pending',goal:'original'}]}},
  {sequence:3,type:'task_claimed',data:{id:'t',status:'running',goal:'original'}},
  {sequence:4,type:'model_stream',data:{delta:{text:'hidden'}}},
  {sequence:5,type:'task_completed',data:{id:'t',status:'verified',goal:'original'}},
  {sequence:6,type:'finding_submitted',data:{id:'f',evidence:[{id:'e'}]}},
  {sequence:7,type:'run_completed'}
]};
const before=JSON.stringify(snap),projection=projectHistory(snap,3);
assert.equal(projection.plan.tasks[0].status,'running');assert.equal(projection.findings.length,0);assert.equal(projection.result,undefined);
assert.equal(JSON.stringify(snap),before);assert.equal(replayEvents(snap).length,6);
assert.equal(projectHistory(snap,6).findings[0].title,'已登记发现 f');
assert.equal(projectHistory(snap,7).run.status,'completed');
const disposed={...snap,events:[...snap.events,{sequence:8,type:'task_disposed',data:{id:'t',status:'abandoned',disposition_reason:'no viable path'}},{sequence:9,type:'plan_failed'}]};
assert.equal(projectHistory(disposed,9).plan.tasks[0].status,'abandoned');
assert.equal(projectHistory(disposed,9).run.status,'failed');
const handlers={},canvas={dataset:{},clientWidth:600,clientHeight:440,getBoundingClientRect:()=>({left:0,top:0}),scrollLeft:100,scrollTop:200,addEventListener:(name,fn)=>handlers[name]=fn,setPointerCapture(){},classList:{add(){},remove(){}}};
mountGraphPan(canvas);handlers.pointerdown({button:0,pointerId:1,clientX:50,clientY:50,target:{closest:()=>null},preventDefault(){}});handlers.pointermove({pointerId:1,clientX:20,clientY:10});
assert.equal(canvas.scrollLeft,130);assert.equal(canvas.scrollTop,240);handlers.pointercancel();handlers.pointermove({pointerId:1,clientX:0,clientY:0});assert.equal(canvas.scrollLeft,130);
assert.match(markdown('```js\nconst a = "<img>";\n```'),/data-action="copy-code"/);
assert.match(markdown('```js\n<img>\n```'),/&lt;img&gt;/);
assert.match(conversation({run:{id:'r'},messages:[{id:'m',role:'user',content:'hello'}]}),/data-action="copy-message"/);
const hash='a'.repeat(64),ref={hash,mime_type:'image/svg+xml',name:'<x>',bytes:20};
assert.equal(artifactReference({actions:[{request_id:'q',call_id:'c',result:{artifacts:[ref]}}]},hash).request_id,'q');
assert.equal(artifactReference({actions:[]},hash),undefined);
const sharedMedia={actions:[
  {request_id:'q1',call_id:'same-call',result:{artifacts:[ref,{...ref,hash:'b'.repeat(64)}]}},
  {request_id:'q2',call_id:'same-call',result:{artifacts:[{...ref,name:'<second>'}]}}
]};
const mediaBefore=JSON.stringify(sharedMedia),sharedRef=artifactReference(sharedMedia,hash);
assert.deepEqual(sharedRef.sources.map(source=>[source.request_id,source.call_id,source.name]),[['q1','same-call','<x>'],['q2','same-call','<second>']]);
assert.equal(JSON.stringify(sharedMedia),mediaBefore);
const fetchBefore=globalThis.fetch,createBefore=URL.createObjectURL,revokeBefore=URL.revokeObjectURL,mediaViews=[],revoked=[];
try{
  globalThis.fetch=async()=>({ok:true,blob:async()=>new Blob(['fixture'])});
  URL.createObjectURL=()=> 'blob:fixture';URL.revokeObjectURL=value=>revoked.push(value);
  const delivery=createDelivery({state:{snap:sharedMedia},showDetail:(title,html)=>mediaViews.push({title,html}),notice(){},token:()=>({}),isCurrent:()=>true});
  assert.equal(await delivery.dispatch('artifact',{dataset:{hash}}),true);
  assert.equal(mediaViews.at(-1).title,'同字节媒体产物');
  assert.ok(mediaViews.at(-1).html.includes('请求 q1 / 调用 same-call'));
  assert.ok(mediaViews.at(-1).html.includes('请求 q2 / 调用 same-call'));
  assert.match(mediaViews.at(-1).html,/&lt;second&gt;/);
  assert.doesNotMatch(mediaViews.at(-1).html,/<second>/);
  delivery.close();assert.deepEqual(revoked,['blob:fixture']);
}finally{globalThis.fetch=fetchBefore;URL.createObjectURL=createBefore;URL.revokeObjectURL=revokeBefore}
assert.equal(mediaKind('text/html'),'');assert.equal(mediaKind('application/pdf'),'pdf');
assert.match(mediaMarkup(ref,'blob:fixture'),/alt="&lt;x&gt;"/);
assert.match(usagePanel({usageSummary:{run_ids:['r'],cost:{complete:false,priced_responses:0},usage:{},rates:[]}}),/未知 \/ 尚无已定价响应/);
const cacheUsage=usagePanel({usageSummary:{usage:{prompt_cache:{read_ratio:0.8,known_read_tokens:160,observed_input_tokens:200,observed_responses:2,missing_responses:1,invalid_responses:0,known_write_tokens:0}}}});
assert.match(cacheUsage,/80\.0% · 160 \/ 200 输入 token/);
assert.match(cacheUsage,/2 已知 \/ 1 缺失 \/ 0 异常响应/);
assert.match(cacheUsage,/legacy_inclusive_input 按全部输入计价/);
assert.match(cacheUsage,/disjoint_cache_v1 分开未缓存输入、缓存读写与输出，缺明细保持未知/);
assert.match(usagePanel({usageSummary:{usage:{prompt_cache:{read_ratio:null}}}}),/缓存读取命中<\/dt><dd>未知/);
assert.match(usagePanel({usageSummary:{usage:{prompt_cache:{read_ratio:0,known_read_tokens:0,observed_input_tokens:100}}}}),/0\.0% · 0 \/ 100/);
assert.match(usagePanel({usageSummary:{run_ids:['r'],cost:{complete:true,priced_responses:1,known_amount:'0.123456789123456789',currency:'CNY'},usage:{},rates:[]}}),/0\.123456789123456789/);
const price={model:'fixture',input_per_million:'0.123456789123456789',output_per_million:'1',currency:'CNY',source:'fixture rate',effective_at:'2026-10-23T00:00:00Z'};
const config={provider:{},context:{},costs:{currency:'CNY',rates:[price]}};
assert.match(settings({config,tools:[],runs:[],skills:[],settingTab:'provider'}),/id="cost-rates"/);
const fields={'cost-rates':JSON.stringify([price]),'cost-currency':'CNY','cost-pool':'20.000000000000000001','provider-url':'','provider-model':'fixture','provider-key':'','prompt-path':'','context-window':'1000','model-window':'1000','context-reserve':'100','compaction-mode':'semantic'};
globalThis.document={getElementById:id=>id in fields?{value:fields[id]}:null};
assert.equal(readConfigForm(config,'provider').costs.rates[0].input_per_million,price.input_per_million);
assert.equal(readConfigForm(config,'provider').costs.pool_limit,'20.000000000000000001');
delete globalThis.document;
// Search hits carry both message and request IDs; message targeting wins.
const appSource=fs.readFileSync(new URL('./static/app.js',import.meta.url),'utf8');
const revealSource=appSource.slice(appSource.indexOf('async function revealDeepLink('),appSource.indexOf('async function mutate('));
let focusedMessage=false;
const target={dataset:{messageId:'hit'},scrollIntoView(){},setAttribute(){},focus(){focusedMessage=true}};
const revealContext=vm.createContext({state:{selected:'r',deepLink:{message:'hit',request:'request'},snap:{messages:[{id:'hit',role:'assistant',content:'body',request_id:'request'}]}},revision:0,navigation:0,detailRevision:0,document:{querySelectorAll:()=>[target]},render(){throw Error('message deep link incorrectly selected request trajectory')},notice(){}});
vm.runInContext(revealSource,revealContext);await vm.runInContext('revealDeepLink()',revealContext);assert.equal(focusedMessage,true);
const selectSource=appSource.slice(appSource.indexOf('async function selectRun('),appSource.indexOf('async function loadEventHistory('));
const selectContext=vm.createContext({state:{selected:'old',page:'history'},source:null,refreshTimer:null,cancelRunReads(){},clearTimeout(){},runViewFields:[],runViews:new Map([['r',{taskTab:'artifacts'}]]),revision:0,navigation:0,sessionStorage:{setItem(){}},runLink,location:{hash:''},history:{pushState(){}},render(){},refreshRun:async()=>{},subscribe(){},revealDeepLink:async()=>{},runManagement:{reconcile:async()=>null}});
vm.runInContext(selectSource,selectContext);await vm.runInContext("selectRun('r','history',{deepLink:{message:'hit',request:'request'}})",selectContext);
assert.equal(selectContext.state.taskTab,'activity');assert.equal(selectContext.state.workspaceTab,undefined);

const values=new Map();globalThis.sessionStorage={getItem:k=>values.get(k)||null,setItem:(k,v)=>values.set(k,v),removeItem:k=>values.delete(k)};
if(!globalThis.crypto)globalThis.crypto=webcrypto;
let posts=0,lookups=0,operation;
const management=createRunManagement({state:{selected:'r'},api:async(path,method,body)=>{
  if(method==='POST'){posts++;operation=body.operation_id;throw new Error('response lost')}
  lookups++;return {accepted:true,operation_id:operation,status:'queued'};
},showDetail(){},refresh(){},refreshRun(){},render(){},notice(){}});
assert.equal((await management.send('r','steer',{content:'only once',attachments:[]})).status,'queued');
assert.equal(posts,1);assert.equal(lookups,1);assert.equal(values.size,0);
let failures=2;const sent=[];
const retry=createRunManagement({state:{selected:'r'},api:async(path,method,body)=>{
  if(method==='POST'){sent.push(body.operation_id);if(failures-->0)throw new Error('network down');return {status:'queued'}}
  throw Object.assign(new Error('not found'),{status:404});
},showDetail(){},refresh(){},refreshRun(){},render(){},notice(){}});
await assert.rejects(retry.send('r','steer',{content:'one logical send'}));
await assert.rejects(retry.send('r','steer',{content:'different content'}),/恢复原文/);
await assert.rejects(retry.send('r','steer',{content:'one logical send'}));
await retry.send('r','steer',{content:'one logical send'});
assert.equal(new Set(sent).size,1);assert.equal(sent.length,3);
assert.equal(values.size,0);
// Refresh retains only the operation identity, not secret message text.
values.set('trace.operation:reload','retained-op');
let resumedOperation;
const reloaded=createRunManagement({state:{selected:'reload'},api:async(path,method,body)=>{
  if(method==='POST'){resumedOperation=body.operation_id;return {status:'queued'}}
  throw Object.assign(new Error('not yet accepted'),{status:404});
},showDetail(){},refresh(){},refreshRun(){},render(){},notice(){}});
await reloaded.reconcile('reload');
await reloaded.send('reload','steer',{content:'reentered original'});
assert.equal(resumedOperation,'retained-op');
// Reconciliation while navigating must not resend the still-visible old draft.
let accepted=false,logicalPosts=0;
const navigated=createRunManagement({state:{selected:'nav'},api:async(path,method)=>{
  if(method==='POST'){logicalPosts++;throw new Error('lost response')}
  if(accepted)return {status:'queued'};
  throw Object.assign(new Error('not visible yet'),{status:404});
},showDetail(){},refresh(){},refreshRun(){},render(){},notice(){}});
await assert.rejects(navigated.send('nav','steer',{content:'same draft'}));accepted=true;
await navigated.reconcile('nav');await navigated.send('nav','steer',{content:'same draft'});
assert.equal(logicalPosts,1);
values.set('trace.operation:conflict','old-op');
let conflictReads=0;
const conflicted=createRunManagement({state:{selected:'conflict'},api:async(path,method)=>{
  if(method==='POST')throw Object.assign(new Error('payload conflict'),{status:409});
  if(++conflictReads>1)return {status:'queued'};
  throw Object.assign(new Error('not found'),{status:404});
},showDetail(){},refresh(){},refreshRun(){},render(){},notice(){}});
await assert.rejects(conflicted.send('conflict','steer',{content:'different reentered text'}),/payload conflict/);
assert.equal(conflictReads,1,'a 409 must not be converted into success for a different draft');
console.log('B workflows: metadata links, queue concurrency identity, CAS media, historical projection and reported cost passed');
