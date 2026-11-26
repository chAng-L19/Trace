import assert from 'node:assert/strict';
import {webcrypto} from 'node:crypto';
import {readCreationOptions,parseRunRoute,runLink,pendingMessages,moveQueuedMessage,createRunManagement,requirementsForm} from './static/run-management.js';
import {projectHistory,replayEvents,mountGraphPan,detailSnapshot,historyDetail} from './static/history-projection.js';
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
// Worker lifecycle is reduced only from parent-local events with visit identity.
const lifecycle={run:{id:'lifecycle',status:'completed'},workers:[{id:'w',visit:99,status:'completed',cleanup_status:'completed'}],child_reports:[{source_hash:'future-hash',summary:'future-report-secret'}],result:{summary:'future-result-secret'},plan:{tasks:[{id:'t',status:'verified'}]},events:[
  {sequence:1,type:'run_created'},
  {sequence:2,type:'plan_updated',data:{tasks:[{id:'t',goal:'原方向',status:'running',worker_id:'w',child_run_id:'child'}]}},
  {sequence:3,type:'worker_started',created_at:'2026-11-26T01:00:00Z',data:{id:'w',task_id:'t',run_id:'child',visit:1,status:'running',cleanup_status:'pending'}},
  {sequence:4,type:'worker_result_ready',data:{worker_id:'w',run_id:'child',task_id:'t',visit:'1',source_hash:'early'}},
  {sequence:5,type:'worker_cancel_requested',data:{worker_id:'w',task_id:'t',visit:1}},
  {sequence:6,type:'worker_finished',created_at:'2026-11-26T01:01:00Z',data:{worker_id:'w',run_id:'child',task_id:'t',visit:'1',source_hash:'final-1',status:'paused',cleanup_status:'completed'}},
  {sequence:7,type:'task_completed',data:{id:'t',goal:'原方向',status:'paused',worker_id:'w',child_run_id:'child'}},
  {sequence:8,type:'run_paused'},
  {sequence:9,type:'run_resumed'},
  {sequence:10,type:'worker_resumed',created_at:'2026-11-26T02:00:00Z',data:{id:'w',task_id:'t',run_id:'child',visit:2,status:'running',cleanup_status:'pending',started_at:'2026-11-26T01:00:00Z'}},
  {sequence:11,type:'worker_cleanup_reconciled',data:{worker_id:'w',run_id:'child',task_id:'t',visit:'1',source_hash:'final-1',status:'completed',cleanup_status:'completed'}},
  {sequence:12,type:'worker_finished',data:{worker_id:'w',run_id:'child',status:'completed',cleanup_status:'completed'}},
  {sequence:13,type:'worker_result_ready',data:{worker_id:'w',run_id:'child',task_id:'t',visit:'2',source_hash:'early-2'}},
  {sequence:14,type:'worker_finished',created_at:'2026-11-26T02:01:00Z',data:{worker_id:'w',run_id:'child',task_id:'t',visit:'2',source_hash:'final-2',status:'completed',cleanup_status:'completed'}},
  {sequence:15,type:'task_completed',data:{id:'t',goal:'原方向',status:'verified',worker_id:'w',child_run_id:'child',evidence_run_id:'child',evidence_ids:['proof']}}
]};
const lifecycleBefore=JSON.stringify(lifecycle),at=sequence=>projectHistory(lifecycle,sequence);
assert.equal(at(2).workers.length,0);assert.equal(at(4).workers[0].status,'running');assert.equal(at(4).workers[0].result_ready,true);
assert.equal(at(4).child_reports[0].source_hash,'early');assert.match(at(4).child_reports[0].summary,/历史信息不完整/);
assert.equal(at(5).workers[0].status,'cancelling');assert.equal(at(5).workers[0].cleanup_status,'pending');assert.equal(at(5).workers[0].finished_at,undefined,'cancel request is not an exit receipt');
assert.equal(at(6).plan.tasks[0].status,'running');assert.equal(at(6).plan.tasks[0].history_incomplete,true);
assert.equal(at(7).plan.tasks[0].status,'paused');assert.equal(at(7).plan.tasks[0].history_incomplete,undefined);
assert.equal(at(9).run.status,'running');assert.equal(at(10).workers[0].started_at,'2026-11-26T02:00:00Z');
assert.equal(at(11).workers[0].visit,2);assert.equal(at(11).workers[0].status,'running');assert.equal(at(11).workers[0].cleanup_status,'pending','late visit 1 reconciliation does not settle visit 2');
assert.equal(at(12).workers[0].status,'running');assert.equal(at(12).workers[0].history_incomplete,true,'legacy settlement does not import current state');
assert.equal(at(14).plan.tasks[0].history_incomplete,true);assert.equal(at(15).plan.tasks[0].status,'verified');assert.equal(at(15).plan.tasks[0].history_incomplete,undefined);
for(let cutoff=1;cutoff<=15;cutoff++){const past=at(cutoff);assert.ok(past.events.every(e=>e.sequence<=cutoff));assert.doesNotMatch(JSON.stringify(past),/future-report-secret|future-result-secret|future-hash/)}
assert.equal(JSON.stringify(lifecycle),lifecycleBefore);
for(const status of ['pending','failed','uncertain','running']){
  const reserved={id:'preparing',goal:'研究方向',status:'running',worker_id:'reserved-worker',child_run_id:'reserved-child',dispatch_phase:'preparing'};
  const settled={...reserved,status,dispatch_phase:'',...(status==='pending'||status==='failed'?{worker_id:'',child_run_id:''}:{})};
  const preparation={run:{id:'r'},events:[{sequence:1,type:'task_claimed',data:reserved},{sequence:2,type:'task_dispatch_settled',data:settled}]};
  assert.equal(projectHistory(preparation,1).plan.tasks[0].dispatch_phase,'preparing');
  assert.deepEqual(projectHistory(preparation,2).plan.tasks[0],settled,'dispatch settlement uses its committed task snapshot');
  assert.equal(projectHistory(preparation,2).workers.length,0,'preparation settlement does not invent an executing worker');
}
const cancelledWorker={id:'w',task_id:'t',run_id:'child',visit:1,status:'cancelled',cleanup_status:'completed',settled_visit:1,started_at:'2026-11-26T01:00:00Z',finished_at:'2026-11-26T01:01:00Z'};
const cancelledTask={id:'t',goal:'原方向',status:'cancelled',worker_id:'w',child_run_id:'child',lease_until:zero};
const cancelSettlement={sequence:8,type:'worker_cancel_requested',data:{worker_id:'w',task_id:'t',child_run_id:'child',visit:1,worker:cancelledWorker,task:cancelledTask}};
const settledCancellation={...lifecycle,events:[...lifecycle.events.filter(e=>e.sequence<=7),cancelSettlement]};
const cancellationBefore=JSON.stringify(settledCancellation),cancelledHistory=projectHistory(settledCancellation,8);
assert.equal(projectHistory(settledCancellation,7).workers[0].status,'paused');
assert.equal(cancelledHistory.workers[0].status,'cancelled','already-cleaned cancellation is a committed outcome');
assert.equal(cancelledHistory.workers[0].cleanup_status,'completed');assert.equal(cancelledHistory.workers[0].settled_visit,1);
assert.equal(cancelledHistory.workers[0].visit_started_sequence,3);
assert.equal(cancelledHistory.workers[0].finished_at,cancelledWorker.finished_at);
assert.deepEqual(cancelledHistory.plan.tasks[0],cancelledTask);
assert.equal(JSON.stringify(settledCancellation),cancellationBefore,'cancellation projection leaves saved snapshots intact');
const lateCancellation={...settledCancellation,events:[...settledCancellation.events,
  {sequence:9,type:'task_claimed',data:{...cancelledTask,status:'running'}},
  {sequence:10,type:'worker_resumed',created_at:'2026-11-26T02:00:00Z',data:{...cancelledWorker,visit:2,status:'running',cleanup_status:'pending'}},
  {...cancelSettlement,sequence:11}
]};
const beforeLateCancellation=projectHistory(lateCancellation,10),afterLateCancellation=projectHistory(lateCancellation,11);
assert.deepEqual(afterLateCancellation.workers,beforeLateCancellation.workers,'late cancelled visit 1 cannot change active visit 2');
assert.deepEqual(afterLateCancellation.plan.tasks,beforeLateCancellation.plan.tasks,'late cancellation cannot restore the prior task outcome');
const wrongSourceCancellation={...settledCancellation,events:[...settledCancellation.events.slice(0,-1),{...cancelSettlement,data:{...cancelSettlement.data,child_run_id:'other-child'}}]};
assert.equal(projectHistory(wrongSourceCancellation,8).workers[0].status,'paused','cancellation source Run must match the visit');
const blocked={run:{id:'blocked'},events:[{sequence:1,type:'plan_updated',data:{tasks:[{id:'t',status:'pending'}]}},{sequence:2,type:'task_blocked_capability',data:{task_id:'t',missing_tool_ids:['probe']}},{sequence:3,type:'task_capability_restored',data:{task_id:'t'}}]};
assert.equal(projectHistory(blocked,2).plan.tasks[0].status,'blocked_capability');assert.equal(projectHistory(blocked,3).plan.tasks[0].status,'pending');
const cutoffElement={closest:selector=>selector==='[data-history-sequence]'?{dataset:{historyRun:'lifecycle',historySequence:'5'}}:null};
const pinned=detailSnapshot({snap:{run:{id:'lifecycle'},result:{summary:'background-refresh-secret'}},replaySnapshot:lifecycle,replaySequence:15},cutoffElement);
assert.equal(pinned.history_sequence,5);assert.equal(pinned.workers[0].status,'cancelling');assert.match(historyDetail(pinned,'saved-detail'),/data-history-sequence="5"/);
assert.doesNotMatch(JSON.stringify(pinned),/background-refresh-secret/);
assert.equal(detailSnapshot({snap:lifecycle},cutoffElement).workers.length,0,'a discarded replay source cannot fall through to current state');
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
  const historicalMedia={run:{id:'media-history'},actions:sharedMedia.actions.map(action=>({...action,name:'media-fixture',arguments:'{}'})),events:sharedMedia.actions.map((action,index)=>({sequence:index*2+1,type:'tool_result',data:{evidence_id:'media-'+index}})),evidence:sharedMedia.actions.map((action,index)=>({id:'media-'+index,call_id:action.call_id,request_id:action.request_id,source_event_sequence:index*2+1,content:JSON.stringify(action.result)}))};
  const historicalMediaBefore=JSON.stringify(historicalMedia),mediaState={snap:historicalMedia,replaySnapshot:historicalMedia,replaySequence:3};
  const cutoffMediaElement=()=>({dataset:{hash},closest:selector=>selector==='[data-history-sequence]'?{dataset:{historyRun:'media-history',historySequence:'1'}}:null});
  let clickedMedia=cutoffMediaElement(),mediaFetches=0;
  globalThis.fetch=async path=>{mediaFetches++;assert.equal(path,'/api/artifacts/'+hash+'/download');return {ok:true,blob:async()=>new Blob(['fixture'])}};
  const historyDelivery=createDelivery({state:mediaState,showDetail:(title,html)=>{mediaViews.push({title,html});clickedMedia.closest=()=>null},notice(){},token:()=>({}),isCurrent:()=>true});
  const historicalViewStart=mediaViews.length;
  assert.equal(await historyDelivery.dispatch('artifact',clickedMedia),true);
  for(const view of mediaViews.slice(historicalViewStart))assert.match(view.html,/data-history-sequence="1"/,'loading and loaded views retain the clicked cutoff after their DOM is replaced');
  assert.ok(mediaViews.at(-1).html.includes('请求 q1 / 调用 same-call'));
  assert.doesNotMatch(mediaViews.at(-1).html,/请求 q2|second/,'same-hash future registrations do not fill historical source metadata');
  assert.match(mediaViews.at(-1).html,/历史截止点已保存的关联/);
  assert.equal(mediaState.replaySequence,3,'the detail cutoff is independent of the current replay slider');
  clickedMedia=cutoffMediaElement();globalThis.fetch=async()=>{mediaFetches++;throw new Error('fixture media unavailable')};
  assert.equal(await historyDelivery.dispatch('artifact',clickedMedia),true);
  assert.equal(mediaViews.at(-1).title,'产物读取失败');assert.match(mediaViews.at(-1).html,/data-history-sequence="1"/);
  const fetchesBeforeMissingHistory=mediaFetches;mediaState.replaySnapshot=null;clickedMedia=cutoffMediaElement();
  assert.equal(await historyDelivery.dispatch('artifact',clickedMedia),true);
  assert.match(mediaViews.at(-1).html,/data-history-sequence="1"/);assert.match(mediaViews.at(-1).html,/历史信息不完整/);
  assert.equal(mediaFetches,fetchesBeforeMissingHistory,'missing historical associations never fetch using the current snapshot');
  mediaState.replaySnapshot=historicalMedia;
  globalThis.fetch=async()=>({ok:true,blob:async()=>new Blob(['fixture'])});
  assert.equal(await historyDelivery.dispatch('artifact',{dataset:{hash}}),true);
  assert.ok(mediaViews.at(-1).html.includes('请求 q1 / 调用 same-call'));assert.ok(mediaViews.at(-1).html.includes('请求 q2 / 调用 same-call'));
  assert.doesNotMatch(mediaViews.at(-1).html,/data-history-sequence=/,'live previews retain all current registrations');
  assert.equal(JSON.stringify(historicalMedia),historicalMediaBefore);
  historyDelivery.close();
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
