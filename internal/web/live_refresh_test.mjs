import {boundedCache} from './static/interactions.js';
import {measureUI,performanceSample,uiNow} from './static/performance.js';
const createContext=values=>vm.createContext(Object.assign(values,{boundedCache,measureUI,performanceSample,uiNow,AbortController,workerCapacity:{refresh(){},reset(){},foreground:read=>read()},delivery:values.delivery||{close(){}},scopedNotice(){return (...args)=>values.notice?.(...args)}}));
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import {mergePreview,projectEvent,isPreviewEvent} from './static/live.js';
import {createSettingsLoader} from './static/settings-loader.js';

const app=fs.readFileSync(new URL('./static/app.js',import.meta.url),'utf8');
const code=app.slice(app.indexOf('const dirtyRuns='),app.indexOf('async function refresh(){'));
const tick=()=>new Promise(resolve=>setImmediate(resolve));
const snapshot=(id,sequence)=>({run:{id,event_seq:sequence},events:[],requests:[]});
function harness(){
  const calls=[],state={selected:'a',snap:snapshot('a',0)},context=createContext({state,revision:0,mergePreview,AbortController,conversationHistory:{refresh:next=>next,cancel(){}},render(){},api(path,method,body,options){return new Promise((resolve,reject)=>calls.push({path,options,resolve,reject}))}});
  vm.runInContext(code,context);
  return {context,state,calls,refresh:id=>vm.runInContext(`refreshRun('${id}')`,context),cancel:()=>vm.runInContext('cancelRunReads()',context)};
}
{
  const h=harness(),first=h.refresh('a');
  h.state.snap.events=[{sequence:2,type:'model_stream',data:{delta:{kind:'text',text:'last'}}}];h.state.snap.run.event_seq=2;
  const dirty=h.refresh('a');h.calls[0].resolve(snapshot('a',1));await tick();
  assert.equal(h.calls.length,2,'dirty signal during in-flight read schedules a second read');
  assert.equal(h.state.snap.events[0].data.delta.text,'last','the in-flight snapshot retains newer committed delta');
  h.calls[1].resolve(snapshot('a',2));await Promise.all([first,dirty]);
}
{
  const h=harness(),oldA=h.refresh('a');
  h.cancel();h.context.revision=1;h.state.selected='b';h.state.snap=null;const b=h.refresh('b');
  h.cancel();h.context.revision=2;h.state.selected='a';const newA=h.refresh('a');
  assert.equal(h.calls.length,3,'new A starts before the cancelled A fetch settles');
  assert.equal(h.calls[0].options.signal.aborted,true);assert.equal(h.calls[1].options.signal.aborted,true);
  h.calls[1].resolve(snapshot('b',80));await b;
  assert.equal(h.state.snap,null,'old B response cannot populate the newly selected A');
  h.calls[0].reject(Error('stale A failure'));await oldA;
  assert.equal(h.calls.length,3,'a stale failure must not consume the latest A refresh');
  assert.equal(vm.runInContext("runLoads.has('a')",h.context),true,'old finally retains the new A flight');
  assert.equal(vm.runInContext("runControllers.get('a').signal.aborted",h.context),false,'old cleanup retains the new A controller');
  h.calls[2].resolve(snapshot('a',90));await Promise.all([oldA,newA]);
  assert.equal(h.state.snap.run.id,'a');assert.equal(h.state.snap.run.event_seq,90);assert.equal(h.state.runError,'');
}
console.log('Live refresh: in-flight dirty signals, final frame reconciliation, A/B/A navigation and stale failure isolation passed.');
// A tab switch cancels the obsolete server projection and still drains the
// dirty pass for the newly selected tab inside the same single flight.
{
  const h=harness();h.state.taskTab='activity';
  const first=h.refresh('a');assert.match(h.calls[0].path,/tab=activity/);
  h.state.taskTab='planner';vm.runInContext("runControllers.get('a').abort()",h.context);
  const next=h.refresh('a');h.calls[0].reject(Error('aborted old activity'));await tick();
  assert.equal(h.calls.length,2);assert.match(h.calls[1].path,/tab=planner$/);
  assert.equal(h.state.snap.run.event_seq,0,'cancelled activity never paints the Planner');
  h.calls[1].resolve({...snapshot('a',12),projection:'planner'});await Promise.all([first,next]);
  assert.equal(h.state.snap.projection,'planner');assert.equal(h.state.snap.run.event_seq,12);
  assert.equal(h.state.runError,'');assert.equal(vm.runInContext('dirtyRuns.size',h.context),0);
}
// Re-selecting the mounted tab is a normal dirty refresh, not cancellation.
{
  const h=harness();h.state.taskTab='planner';
  Object.assign(h.context,{history:{replaceState(){}},runLink:()=>'',loadCatalog:async()=>{},loadEventHistory:async()=>{}});
  const branch=app.slice(app.indexOf("  if(action==='task-tab')"),app.indexOf('\n',app.indexOf("  if(action==='task-tab')")));
  vm.runInContext('async function selectTab(el){const action="task-tab";'+branch+'}',h.context);
  const first=h.refresh('a'),again=vm.runInContext('selectTab({dataset:{tab:"planner"}})',h.context);
  assert.equal(h.calls[0].options.signal.aborted,false,'same-tab refresh keeps the committed in-flight read');
  h.calls[0].resolve({...snapshot('a',4),projection:'planner'});await tick();assert.equal(h.calls.length,2);
  h.calls[1].resolve({...snapshot('a',5),projection:'planner'});await Promise.all([first,again]);
  assert.equal(h.state.snap.run.event_seq,5);assert.equal(vm.runInContext('dirtyRuns.size',h.context),0);
}
// A mutation during an earlier list read must trigger one fresh read, not accept
// the stale list as the mutation's refresh. Concurrent signals share the follow-up.
{
  const calls=[],state={runs:[],historyDetail:false},ctx=createContext({state,render(){},connection(){},api:path=>new Promise((resolve,reject)=>calls.push({path,resolve,reject}))});
  vm.runInContext(code,ctx);
  const first=vm.runInContext('refreshList()',ctx),second=vm.runInContext('refreshList(true)',ctx),third=vm.runInContext('refreshList(true)',ctx);
  assert.equal(calls.length,1);calls[0].resolve([{id:'old'}]);await tick();assert.equal(calls.length,2);assert.equal(state.runs.length,0,'stale pre-mutation list is not painted');
  calls[1].resolve([{id:'new'}]);await Promise.all([first,second,third]);assert.equal(state.runs[0].id,'new');
  const failed=vm.runInContext('refreshList()',ctx);calls[2].reject(Error('list offline'));await assert.rejects(failed,/list offline/);assert.equal(state.listError,'list offline');
  const recovery=vm.runInContext('refreshList()',ctx);calls[3].resolve([{id:'recovered'}]);await recovery;assert.equal(state.listError,'');assert.equal(state.runs[0].id,'recovered');
}
// The production wrapper reads only the active page and shares in-flight work.
{
  const calls=[],state={page:'overview'},listeners=new Map(),ctx=createContext({state,document:{addEventListener(type,listener){listeners.set(type,listener)}},render(){},connection(){},api:path=>new Promise(resolve=>calls.push({path,resolve}))});
  ctx.settingsLoader=createSettingsLoader({state,api:ctx.api});
  vm.runInContext(code,ctx);
  vm.runInContext(app.slice(app.indexOf('function loadSettings('),app.indexOf("document.addEventListener('click'")),ctx);
  assert.equal(typeof listeners.get('toggle'),'function','production lazy disclosure listener is registered');listeners.get('toggle')({target:{matches:()=>false}});
  const reads=[vm.runInContext('refreshList()',ctx),vm.runInContext('refreshList()',ctx),vm.runInContext('loadSettings()',ctx),vm.runInContext('loadSettings()',ctx)];
  assert.equal(calls.length,2);for(const call of calls)call.resolve(call.path==='/config'?{provider:{}}:[]);await Promise.all(reads);assert.equal(calls.length,2);
}
// Closed EventSource callbacks must not update a new subscription to the same run.
{
  const streams=[],connections=[],projected=[];
  class Stream{constructor(){streams.push(this)}close(){}addEventListener(type,listener){this.listener=listener}}
  const state={selected:'a',historyDetail:true,snap:snapshot('a',0),workspaceReadAt:123},ctx=createContext({state,source:null,refreshTimer:null,clearTimeout(){},EventSource:Stream,connection:(...args)=>connections.push(args),projectEvent:(_,item)=>projected.push(item),paintSoon(){},isPreviewEvent:()=>true});
  vm.runInContext(app.slice(app.indexOf('function subscribe(){'),app.indexOf('const runViewFields=')),ctx);
  vm.runInContext('subscribe()',ctx);vm.runInContext('subscribe()',ctx);
  streams[0].onopen();streams[0].onerror();streams[0].listener({data:JSON.stringify({sequence:90})});
  assert.equal(connections.length,0);assert.equal(projected.length,0);assert.equal(state.lastSeq,0);assert.equal(state.workspaceReadAt,123,'old subscription error cannot clear the replacement view freshness');
  streams[1].onopen();streams[1].listener({data:JSON.stringify({sequence:1})});assert.equal(connections.length,1);assert.equal(projected.length,1);
  state.selected='b';state.snap=snapshot('b',2);state.workspaceReadAt=456;vm.runInContext('subscribe()',ctx);
  streams[1].onerror();assert.equal(state.workspaceReadAt,456,'old run subscription error cannot change the new run freshness');
  streams[2].onerror();assert.equal(state.workspaceReadAt,undefined,'current subscription error enables recovery at the next fallback tick');
}
// Worker list metadata changes the shell counter, not the mounted transcript.
{
  let pages=0,counters=0;const calls=[],state={page:'history',historyDetail:true};
  const ctx=createContext({state,render:()=>pages++,renderRunCount:()=>counters++,api:async()=>{calls.push(true);return []}});
  vm.runInContext(code,ctx);await vm.runInContext('refreshList()',ctx);
  assert.equal(counters,1);assert.equal(pages,0,'list completion does not rebuild settled Markdown or exploration tree');
  state.historyDetail=false;ctx.connection=()=>{};await vm.runInContext('refreshList()',ctx);
  assert.equal(pages,1,'archive still renders the updated list');
}

// A semantic workspace refresh can overtake frames still queued by EventSource.
// Replayed frames update the subscription cursor without another GET or paint.
{
  const streams=[],timers=[],painted=[],lists=[],refreshes=[];
  class Stream{constructor(){streams.push(this)}close(){}addEventListener(type,listener){this.listener=listener}}
  const state={selected:'a',historyDetail:true,snap:snapshot('a',10)},ctx=createContext({state,source:null,refreshTimer:null,dirtyRuns:new Set(),clearTimeout(){},setTimeout(fn,ms){timers.push({fn,ms});return timers.length},EventSource:Stream,connection(){},projectEvent,isPreviewEvent,paintSoon:()=>painted.push(true),refreshList:async()=>lists.push(true),refreshRun:async()=>refreshes.push(true)});
  vm.runInContext(app.slice(app.indexOf('function subscribe(){'),app.indexOf('const runViewFields=')),ctx);
  vm.runInContext('subscribe()',ctx);state.snap=snapshot('a',20);
  for(const sequence of [11,12,20])streams[0].listener({data:JSON.stringify({sequence,type:'worker_finished'})});
  assert.equal(state.lastSeq,20);assert.equal(timers.length,0);assert.equal(lists.length,0);assert.equal(painted.length,0);assert.equal(ctx.dirtyRuns.size,0);
  streams[0].listener({data:JSON.stringify({sequence:21,type:'worker_finished'})});
  assert.equal(timers.length,1);assert.equal(lists.length,1);assert.equal(painted.length,1);assert.equal(ctx.dirtyRuns.has('a'),true);
  ctx.dirtyRuns.clear();timers[0].fn();assert.equal(refreshes.length,0,'already-consumed dirty signal does not trigger a redundant GET');
}

// Read-only panels must reject A/B/A navigation and must not reopen closed dialogs.
for(const action of ['usage-refresh','replay-open','child-preview','snapshot']){
  const calls=[],state={selected:'a',snap:{run:{id:'a'},childPreviews:{}}},shown=[];
  const ctx=createContext({state,revision:1,navigation:1,detailRevision:0,render(){},showDetail:(...args)=>shown.push(args),api:path=>new Promise(resolve=>calls.push({path,resolve})),replayEvents:snap=>snap.events||[],esc:String});
  vm.runInContext(app.slice(app.indexOf('async function dispatch('),app.indexOf('async function submit(')),ctx);
  const pending=vm.runInContext(`dispatch('${action}',{dataset:{id:'child'}})`,ctx);
  ctx.revision=3;ctx.navigation=3;state.snap={run:{id:'a'},childPreviews:{}};state.replayLoading=false;
  calls[0].resolve({run:{id:'child',parent_run_id:'a'},events:[{sequence:8}]});await pending;
  assert.equal(state.usageSummary,undefined);assert.equal(state.replaySnapshot,undefined);assert.equal(Object.keys(state.snap.childPreviews).length,0);assert.equal(shown.length,0,action+' did not reopen stale dialog');
}
console.log('Frontend review: single-flight reads, explicit invalidation, stale subscriptions and read-only navigation guards passed.');
// One summary query supplies both views; archived hidden records are retained.
{
  const paths=[],state={},ctx=createContext({state,render(){},connection(){},api:async path=>{paths.push(path);return [{id:'visible'},{id:'archived',archived:true},{id:'hidden',archived:true,deleted:true}]}});
  vm.runInContext(code,ctx);await vm.runInContext('refreshList()',ctx);
  assert.deepEqual(paths,['/runs?include_archived=true&include_deleted=true']);
  assert.deepEqual(Array.from(state.runs,r=>r.id),['visible','archived']);
  assert.deepEqual(Array.from(state.hiddenRuns,r=>r.id),['hidden']);
}
{
  const calls=[],state={selected:'a',snap:{requests:[{id:'r'}]}},ctx=createContext({state,revision:1,navigation:1,detailRevision:0,showDetail(){},rawDetails:()=>'',requestDetails:new Map(),requestLoads:new Map(),render(){},api:path=>new Promise((resolve,reject)=>calls.push({path,resolve,reject}))});
  vm.runInContext(app.slice(app.indexOf('async function dispatch('),app.indexOf('async function submit(')),ctx);
  const schema=vm.runInContext("dispatch('request-detail',{dataset:{request:'r'}})",ctx),raw=vm.runInContext("dispatch('request-detail',{dataset:{request:'r'}})",ctx);
  assert.equal(calls.length,1,'schema/raw share one request detail read');calls[0].resolve({request:{id:'r',body:'persisted request'}});await Promise.all([schema,raw]);
  await vm.runInContext("dispatch('request-detail',{dataset:{request:'r'}})",ctx);assert.equal(calls.length,1,'reopening a loaded request uses its immutable record');
  ctx.requestDetails.clear();const failed=vm.runInContext("dispatch('request-detail',{dataset:{request:'r'}})",ctx);calls[1].reject(Error('detail offline'));await assert.rejects(failed,/detail offline/);
  const retry=vm.runInContext("dispatch('request-detail',{dataset:{request:'r'}})",ctx);assert.equal(calls.length,3);calls[2].resolve({id:'r',body:'recovered'});await retry;assert.equal(ctx.requestDetails.get('a:r').body,'recovered');
}
{
  const state={selected:'a',page:'history',historyDetail:true,snap:snapshot('a',4)},ctx=createContext({state});
  vm.runInContext(app.slice(app.indexOf('async function selectRun('),app.indexOf('async function loadEventHistory(')),ctx);
  await vm.runInContext("selectRun('a','history')",ctx);assert.equal(state.snap.run.event_seq,4,'selecting current loaded run needs no reload or view reset');
}

// Cancelling a same-run read also detaches its single-flight ownership. Even a
// transport that ignores abort cannot paint or erase the replacement read.
{
  const h=harness(),old=h.refresh('a');h.cancel();const current=h.refresh('a');
  assert.equal(h.calls.length,2);
  h.calls[0].resolve(snapshot('a',80));await old;
  assert.equal(h.state.snap.run.event_seq,0);
  assert.equal(vm.runInContext("runLoads.has('a')",h.context),true);
  h.calls[1].resolve(snapshot('a',90));await current;
  assert.equal(h.state.snap.run.event_seq,90);assert.equal(h.state.runError,'');
}
// The first accepted workspace opens SSE while a dirty follow-up GET is still
// pending. Later committed snapshots do not reset the stream or its cursor.
{
  const h=harness(),streams=[],timers=[];
  class Stream{constructor(url){this.url=url;streams.push(this)}close(){}addEventListener(type,listener){this.listener=listener}}
  Object.assign(h.context,{source:null,refreshTimer:null,EventSource:Stream,clearTimeout(){},setTimeout(fn,ms){timers.push({fn,ms});return timers.length},connection(){},projectEvent,isPreviewEvent});
  h.state.historyDetail=true;
  vm.runInContext(app.slice(app.indexOf('function subscribe(){'),app.indexOf('const runViewFields=')),h.context);
  const first=h.refresh('a'),dirty=h.refresh('a');h.calls[0].resolve(snapshot('a',10));await tick();
  assert.equal(h.calls.length,2);assert.equal(streams.length,1);assert.match(streams[0].url,/after=10$/);
  streams[0].listener({data:JSON.stringify({sequence:11,type:'model_stream',data:{request_id:'r',delta:{kind:'text',text:'live'}}})});
  assert.equal(h.state.snap.run.event_seq,11,'committed frames arrive before follow-up GET completes');
  h.calls[1].resolve(snapshot('a',20));await Promise.all([first,dirty]);
  assert.equal(streams.length,1);assert.equal(h.state.lastSeq,11,'workspace refresh does not reset the subscription cursor');
  streams[0].listener({data:JSON.stringify({sequence:12,type:'worker_finished'})});
  assert.equal(h.state.lastSeq,12);assert.equal(h.state.snap.run.event_seq,20,'already committed SSE replay cannot roll back the workspace');
  streams[0].listener({data:JSON.stringify({sequence:21,type:'model_stream',data:{delta:{kind:'text',text:'next'}}})});
  assert.equal(h.state.snap.run.event_seq,21,'the next committed frame is still applied');
  assert.equal(h.calls.length,2,'replayed semantic frames do not trigger redundant workspace reads');
}
// Event locators read the requested persisted page directly, without first
// fetching the latest page. Invalid locators retain the normal latest view.
for(const event of ['42','invalid','']){
  const calls=[],target={open:false,scrollIntoView(){},setAttribute(){},focus(){}},state={selected:'old',page:'history'},ctx=createContext({state,source:null,refreshTimer:null,revision:0,navigation:0,detailRevision:0,runViews:new Map(),runViewFields:[],URLSearchParams,cancelRunReads(){},clearTimeout(){},sessionStorage:{setItem(){}},location:{hash:''},history:{pushState(){}},runLink:id=>'#history/'+id,render(){},refreshRun:async id=>{state.snap=snapshot(id,100);state.runLoading=false},subscribe(){ctx.source={}},runManagement:{reconcile:async()=>null},api:async path=>{calls.push(path);return {events:[{sequence:42}]}},document:{getElementById:()=>target},notice(){}});
  vm.runInContext(app.slice(app.indexOf('async function selectRun('),app.indexOf('function showDetail(')),ctx);
  vm.runInContext(app.slice(app.indexOf('async function revealDeepLink('),app.indexOf('async function mutate(')),ctx);
  ctx.link={tab:'events',event};await vm.runInContext("selectRun('a','history',{deepLink:link})",ctx);
  assert.equal(state.runError,'');assert.equal(calls.length,1,event+' uses one event page read');
  if(event==='42'){assert.match(calls[0],/before=43/);assert.doesNotMatch(calls[0],/summary=true/);assert.equal(target.open,true)}
  else {assert.doesNotMatch(calls[0],/before=/);assert.match(calls[0],/summary=true/)}
}
// Cold deep links render the requested run once. A slow config read updates
// model/provider labels without serializing the settled transcript again.
for(const failure of [false,true]){
  let pages=0,labels=0,models=0,resolve,reject;
  const state={},ctx=createContext({state,location:{hash:'#history/a'},resolveRoute:()=>({page:'history',historyDetail:true,runID:'a'}),render:()=>pages++,modelConnection:()=>models++,renderProviderLabel:()=>labels++,providerProbe:{sync(){}},loadSettings:()=>new Promise((yes,no)=>{resolve=yes;reject=no}),refreshList:async()=>{},selectRun:()=>{state.snap=snapshot('a',1);pages++},connection(){}});
  vm.runInContext(app.slice(app.indexOf('function settingsLoaded('),app.indexOf("document.addEventListener('toggle'")),ctx);
  vm.runInContext(app.slice(app.indexOf('async function boot(){'),app.indexOf('}boot();')+1),ctx);
  await vm.runInContext('boot()',ctx);assert.equal(pages,1,'cold locator skips the transient archive render');
  if(failure)reject(Error('config offline'));else resolve();await tick();
  assert.equal(pages,1);assert.equal(labels,1);assert.equal(models,1);
  if(failure)assert.equal(state.settingsError,'config offline');
  state.page='settings';vm.runInContext('settingsLoaded()',ctx);assert.equal(pages,2,'configuration pages still render newly loaded content');
  state.page='history';vm.runInContext("settingsLoaded('tools')",ctx);assert.equal(pages,3,'late tool catalog loads still update pending-call availability after navigation');
}
// Only completed navigation actions bypass the final full render. Actions
// that mutate state after an earlier render still need their final paint.
for(const action of ['select','run-retry','catalog-open-run','usage-refresh','replay-open']){
  let handler,pages=0,pendingPaints=0;const state={},pending=new Set(),el={dataset:{action},matches:()=>false};
  const ctx=createContext({state,pending,detailRevision:0,navigation:0,document:{addEventListener:(type,fn)=>handler=fn},operationKey:()=>action,applyPending:()=>pendingPaints++,render:()=>pages++,runManagement:{dispatch:async()=>false},delivery:{dispatch:async()=>false},dispatch:async()=>{if(action!=='usage-refresh')pages++;state.result='loaded'}});
  vm.runInContext(app.slice(app.indexOf("document.addEventListener('click'"),app.indexOf("document.addEventListener('submit'")),ctx);
  await handler({target:{closest:selector=>selector==='[data-action]'?el:null},preventDefault(){}});
  assert.equal(state.result,'loaded');assert.equal(pending.size,0);
  assert.equal(pages,action==='replay-open'?2:1,action+' paints all changed state without an extra navigation render');
  assert.equal(pendingPaints,['select','run-retry','catalog-open-run'].includes(action)?2:1);
}
console.log('Entry loading: immediate accepted-workspace SSE, detached aborted reads, direct event locators and exact navigation/config paints passed.');

// Closed child details do not enter the initial workspace critical path. An
// open during that read joins it and requests details only in its next pass.
{
  const h=harness(),listeners=new Map();
  Object.assign(h.state,{historyDetail:true,taskTab:'activity',workerActivityExpanded:false});
  h.state.snap.projection='activity';
  Object.assign(h.context,{source:{},document:{addEventListener(type,listener){listeners.set(type,listener)}}});
  vm.runInContext(app.slice(app.indexOf("document.addEventListener('toggle'"),app.indexOf("document.addEventListener('click'")),h.context);
  const toggle=open=>listeners.get('toggle')({target:{matches:selector=>selector==='[data-worker-activity]',open,isConnected:true}});
  const entry=h.refresh('a');assert.match(h.calls[0].path,/activity=true&workers=false$/);
  toggle(true);toggle(true);
  assert.equal(h.calls.length,1,'expansion shares the pending workspace read');
  h.calls[0].resolve({...snapshot('a',10),projection:'activity'});await tick();
  assert.equal(h.calls.length,2);assert.match(h.calls[1].path,/activity=true$/);
  toggle(true);assert.equal(vm.runInContext('dirtyRuns.size',h.context),0,'restoring open disclosure does not create another refresh');
  projectEvent(h.state.snap,{sequence:11,type:'model_stream',data:{request_id:'r',delta:{kind:'text',text:'committed'}}});
  h.calls[1].resolve({...snapshot('a',10),projection:'activity',worker_activity:[{worker_id:'w',phase:'paused'}]});await entry;
  assert.equal(h.state.snap.run.event_seq,11,'expanded details never roll back the committed SSE watermark');
  assert.equal(h.state.snap.worker_activity[0].phase,'paused');
  toggle(false);assert.equal(h.calls.length,2,'collapsing starts no request');
  const refresh=h.refresh('a');assert.match(h.calls[2].path,/workers=false$/);
  h.calls[2].resolve({...snapshot('a',12),projection:'activity'});await refresh;
  assert.equal(h.state.snap.worker_activity,undefined,'a closed refresh retains no stale child details');
  listeners.get('toggle')({target:{matches:()=>true,open:true,isConnected:false}});
  assert.equal(h.state.workerActivityExpanded,false,'detached prior-run disclosure has no effect');
  h.state.historyDetail=false;toggle(true);assert.equal(h.calls.length,3,'off-page disclosure cannot fetch child details');
}

// The disconnected-SSE poll is a fallback, not a semantic invalidation. A
// slow entry or existing reconciliation read already supplies its next view.
{
  const h=harness();let poll,now=0;
  Object.assign(h.context,{uiNow:()=>now,source:null,document:{hidden:false},EventSource:{OPEN:1},setInterval(fn,ms){assert.equal(ms,5000);poll=fn},subscribe(){h.context.source={readyState:1}}});
  h.state.historyDetail=true;h.state.runLoading=true;
  vm.runInContext(app.slice(app.indexOf('setInterval(()=>{if(document.hidden)')),h.context);
  poll();assert.equal(h.calls.length,0,'initial loading does not enqueue a competing workspace read');
  const entry=h.refresh('a');poll();poll();
  assert.equal(h.calls.length,1);assert.equal(vm.runInContext('dirtyRuns.size',h.context),0,'polling does not dirty the in-flight entry');
  h.calls[0].resolve(snapshot('a',10));await entry;
  assert.equal(h.calls.length,1,'slow entry completes without a redundant follow-up GET');assert.equal(h.state.workspaceReadAt,0,'zero is a valid monotonic accepted-read timestamp');
  poll();assert.equal(h.calls.length,1,'open SSE still avoids polling');
  h.context.source.readyState=0;now=300;poll();assert.equal(h.calls.length,1,'global tick during initial SSE connection reuses the recent accepted read');
  now=4999;poll();assert.equal(h.calls.length,1,'fallback waits a full interval since acceptance');
  now=5000;poll();const recovery=vm.runInContext("runLoads.get('a')",h.context);
  assert.equal(h.calls.length,2,'SSE still connecting at the interval boundary resumes fallback reads');
  poll();assert.equal(vm.runInContext('dirtyRuns.size',h.context),0,'an existing fallback read is not invalidated by its next poll');
  h.calls[1].reject(Error('temporary offline'));await assert.rejects(recovery,/temporary offline/);assert.equal(h.state.workspaceReadAt,0,'failure never advances freshness');
  now=10000;poll();const retry=vm.runInContext("runLoads.get('a')",h.context);assert.equal(h.calls.length,3,'a failed recovery read is retried on the next poll');
  h.calls[2].resolve(snapshot('a',20));await retry;assert.equal(h.state.runError,'');assert.equal(h.state.workspaceReadAt,10000);
  now=10001;const semantic=h.refresh('a');assert.equal(h.calls.length,4,'a true semantic invalidation bypasses the fallback freshness gate');
  h.calls[3].resolve(snapshot('a',21));await semantic;
  now=15001;h.context.document.hidden=true;poll();assert.equal(h.calls.length,4,'hidden pages keep the existing poll policy');
  h.context.document.hidden=false;h.state.historyDetail=false;let lists=0;h.context.refreshList=async()=>lists++;
  poll();assert.equal(lists,1,'archive polling still refreshes the list');
}
// Freshness belongs only to an accepted current view. A cancelled transport
// cannot renew it after another run has accepted its own workspace.
{
  const h=harness();let now=100;h.context.uiNow=()=>now;
  const old=h.refresh('a');h.state.workspaceReadAt=50;h.cancel();assert.equal(h.state.workspaceReadAt,undefined);
  h.context.revision=1;h.state.selected='b';h.state.snap=null;const current=h.refresh('b');
  now=200;h.calls[1].resolve(snapshot('b',20));await current;assert.equal(h.state.workspaceReadAt,200);
  now=400;h.calls[0].resolve(snapshot('a',90));await old;assert.equal(h.state.workspaceReadAt,200,'late aborted A cannot postpone B recovery');
  h.cancel();assert.equal(h.state.workspaceReadAt,undefined,'the next navigation does not inherit the previous run freshness');
}
console.log('Entry fallback: initial/in-flight/recent accepted reads avoid duplicate GETs; SSE recovery, retry and immediate semantic reads remain active.');
