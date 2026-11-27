import assert from 'node:assert/strict';
import {occupiedWorkerCount,esc,btn,label,json} from './static/ui.js';
import fs from 'node:fs';
import vm from 'node:vm';
import {workerGroups,pendingDispatch,workerSummary,workerPanel} from './static/workers.js';
import {taskGroups,renderPage} from './static/views.js';
import {explorationData,renderExplorationDetail,resolveEvidenceReference} from './static/exploration.js';
import {captureScroll,restoreScroll} from './static/interactions.js';
import {creationOptions,readCreationOptions,parseRunRoute,runLink} from './static/run-management.js';
import {taskForm,readTask} from './static/workforms.js';
import {explorationPreview} from './static/graphs.js';
import {detailSnapshot,historyDetail} from './static/history-projection.js';
import {evidenceDetail} from './static/evidence.js';

const tasks=[{id:'wait',goal:'等待依赖',status:'pending',dependencies:['active']},{id:'blocked',status:'blocked_capability'},{id:'active',status:'running',worker_id:'w1',child_run_id:'child1'},{id:'claim',status:'running'},{id:'failed',status:'failed'},{id:'done',status:'completed'}];
const workers=[{id:'w1',task_id:'active',run_id:'child1',status:'running'},{id:'w2',status:'starting'},{id:'w3',status:'cancelling'},{id:'w4',status:'paused'},{id:'w5',status:'uncertain'},{id:'w6',status:'completed',cleanup_status:'unknown'},{id:'w7',status:'completed',cleanup_status:'completed'}];
const snap={run:{id:'root',goal:'test',status:'running'},plan:{tasks,capacity:5},workers,messages:[],actions:[]};
assert.equal(occupiedWorkerCount(snap),7,'cleanup-unknown and unregistered claim retain actual slots');
assert.equal(occupiedWorkerCount({...snap,totals:{workers_occupying:9}}),9,'server occupancy overrides trimmed projection');
assert.deepEqual(workerGroups(snap).active.map(w=>w.id),['w1','w2','w3']);
assert.deepEqual(workerGroups(snap).attention.map(w=>w.id),['w4','w5','w6']);
assert.deepEqual(workerGroups(snap).history.map(w=>w.id),['w7']);
assert.deepEqual(pendingDispatch(snap).map(t=>t.id),['claim']);
assert.deepEqual(pendingDispatch({...snap,workers:[{...workers[0],status:'paused'}]}).map(t=>t.id),['claim'],'registered paused workers stay in attention, not dispatch');
assert.deepEqual(taskGroups(tasks).pending.map(t=>t.id),['wait','blocked']);
assert.match(workerSummary(snap),/派发中/);
assert.doesNotMatch(workerSummary(snap),/data-id="w7"/);
assert.match(workerPanel(snap),/<details id="worker-history"><summary>历史 Worker/);
assert.equal(tasks[0].status,'pending','projection does not mutate persisted statuses');

const state={page:'history',selected:'root',historyDetail:true,taskTab:'activity',snap,runs:[snap.run],tools:[]};
const html=renderPage(state);
assert.doesNotMatch(html,/class="exploration-view"/,'full graph remains lazy');
assert.match(html,/class="exploration-preview"/,'workspace directly renders the overview');
assert.match(html,/data-scroll="workspace-lineage"/);
assert.match(html,/workspace-composer/);assert.match(html,/workspace-secondary/);
assert.match(renderPage({...state,runLoading:true}),/aria-busy="true"/);
assert.match(renderPage({...state,runLoading:true}),/data-action="latest"[^>]* disabled>[^]*?正在读取会话/);

const report={source_run_id:'child1',parent_run_id:'root',task_id:'active',worker_id:'w1',visit:1,source_hash:'abc',run_status:'completed',result_status:'incomplete',cleanup_status:'unknown',source_status:'valid',current:true,summary:'child conclusion',report_path:'/api/runs/child1/export?format=report&report_id=archive-abc&language=zh-CN',findings:[],unresolved:['pending']};
const graph=explorationData({...snap,child_reports:[report,report,{...report,visit:2,source_hash:'def'}]});
assert.equal(graph.nodes.filter(n=>n.kind==='child_report').length,2);
assert.equal(graph.nodes.filter(n=>n.kind==='source').length,2);
assert.equal(graph.nodes.filter(n=>n.kind==='observation').length,0,'child refs are not parent execution proof');
assert.ok(graph.edges.some(e=>e.from==='task:active'&&e.relation==='子结算报告'));
assert.match(renderExplorationDetail({...snap,child_reports:[report]},graph.nodes.find(n=>n.kind==='child_report').id),/读取固定版本源报告/);
const preview=explorationPreview({...snap,child_reports:[report,report,{...report,visit:2,source_hash:'def'}]});
assert.equal((preview.match(/class="child_report"/g)||[]).length,2,'distinct visits expand distinct report nodes');
assert.equal((preview.match(/class="source"/g)||[]).length,0,'fixed sources remain in receipt details instead of the default mainline');
assert.match(preview,/<title>子结算报告<\/title>/);
assert.doesNotMatch(preview,/<title>固定来源指针<\/title>/);
assert.doesNotMatch(preview,/class="observation"/,'child receipts never become parent execution proof');
assert.doesNotMatch(explorationPreview({run:snap.run}),/class="intent"|class="child_report"/,'empty plans do not invent work');
const large=explorationPreview({...snap,plan:{tasks:Array.from({length:100},(_,i)=>({id:'t'+i,status:'pending',goal:'<script>'+i}))},child_reports:Array.from({length:30},(_,i)=>({...report,task_id:'t'+i,source_hash:'hash'+i}))});
assert.ok((large.match(/data-node-search=/g)||[]).length<=22,'preview DOM stays bounded');
assert.match(large,/task:t29/,'recent report tasks retain their real ancestors');
assert.doesNotMatch(large,/<script>/,'untrusted node titles are escaped');

{
// Same-visit revisions fold by registration, while explicit old hashes stay exact.
const early={...report,summary:'旧回执正文',source_hash:'early',created_at:'2030-01-01T00:00:00Z',report_path:'/api/runs/child1',settlement_pending:true};
const final={...report,summary:'新回执正文',source_hash:'final',created_at:'2020-01-01T00:00:00Z'};
const revisions={...snap,child_reports:[early,final,{...report,visit:2,source_hash:'second-visit'}],totals:{child_reports:40}};
const revisedGraph=explorationData(revisions),representative=revisedGraph.nodes.find(n=>n.kind==='child_report'&&n.record.visit===1);
assert.equal(revisedGraph.nodes.filter(n=>n.kind==='child_report').length,2);
assert.equal(representative.record.source_hash,'final','last registration wins even if CreatedAt was rewritten');
const earlyID='child-report:'+JSON.stringify(['child1','active','w1',1,'early']),earlyDetail=renderExplorationDetail(revisions,earlyID);
assert.match(earlyDetail,/旧回执正文/);assert.doesNotMatch(earlyDetail,/新回执正文/);
assert.match(earlyDetail,/本组历史回执/);assert.match(earlyDetail,/未登记固定 report_id/);
assert.doesNotMatch(earlyDetail,/读取固定版本源报告/);
assert.match(renderExplorationDetail({...revisions,child_reports:[final]},earlyID),/引用版本未载入/);
assert.doesNotMatch(renderExplorationDetail({...revisions,child_reports:[final]},earlyID),/新回执正文/);
assert.match(renderExplorationDetail(revisions,representative.id),/此 visit 的回执版本 · 2/);
assert.match(explorationPreview(revisions),/累计回执 40 条 · 载入回执 3 条 · 载入访问组 2 组 · 未载入 37 条/);
assert.match(explorationPreview({...revisions,totals:{child_reports:0}}),/累计回执 0 条/,'authoritative zero is not replaced with a window length');
const busy={...snap,plan:{tasks:[...Array.from({length:12},(_,i)=>({id:'old-'+i,goal:'最近完成方向 '+i,status:'completed'})),{id:'running-now',goal:'当前活动方向',status:'running'},{id:'blocked-now',goal:'能力阻塞方向',status:'blocked_capability'}]},child_reports:Array.from({length:12},(_,i)=>({...report,task_id:'old-'+i,source_hash:'old-'+i}))};
const busyPreview=explorationPreview(busy);
assert.match(busyPreview,/task:running-now/);assert.match(busyPreview,/task:blocked-now/);
assert.match(busyPreview,/省略 6 个方向/);
}
let top=100,offset=10,writes=0;
const node={dataset:{messageId:'m1'},getBoundingClientRect:()=>({top:offset,bottom:offset+60})};
const pane={dataset:{scroll:'conversation'},scrollHeight:1000,clientHeight:200,scrollLeft:0,get scrollTop(){return top},set scrollTop(v){writes++;top=v},getBoundingClientRect:()=>({top:0}),querySelectorAll:()=>[node]};
const saved=captureScroll(pane);offset=60;restoreScroll(pane,saved);
assert.equal(top,150,'above-anchor growth is compensated, not followed to bottom');assert.equal(writes,1);
top=790;const bottom=captureScroll(pane);pane.scrollHeight=1100;restoreScroll(pane,bottom);assert.equal(top,1100);

assert.ok(creationOptions().indexOf('new-completion')<creationOptions().indexOf('creation-advanced'));
assert.ok(taskForm([]).indexOf('task-completion')<taskForm([]).indexOf('task-advanced'));
const fields={'task-id':'id','task-goal':'goal','task-completion':'write a report','task-mode':'explore','task-priority':'0','task-predicate':''};
const form={querySelector:key=>({value:fields[key.slice(1)]}),querySelectorAll:()=>[]};
const task=readTask(form,[])[0];assert.equal(task.completion_criteria,'write a report');assert.equal(task.success_contains,'');
const rootForm={elements:Object.fromEntries(['max_model_requests','max_actions','max_seconds'].map(k=>[k,{value:'0'}])),querySelector:key=>({value:key==='#new-completion'?'natural language':'',checked:false})};
assert.equal(readCreationOptions(rootForm).completion_criteria,'natural language');assert.equal(readCreationOptions(rootForm).success_contains,'');
const app=fs.readFileSync(new URL('./static/app.js',import.meta.url),'utf8');
const detailAction=app.split('\n').find(line=>line.includes("if(action==='exploration-detail')"));
for(const taskTab of ['activity','assets']){
 const current={run:{id:'current'}},history={run:{id:'history'}};
 const ctx=vm.createContext({state:{snap:current,taskTab},action:'exploration-detail',el:{dataset:{id:'node'}},selectExplorationChain:selected=>assert.equal(selected.dataset.id,'node'),graphSnapshot:()=>history,renderExplorationDetail:s=>s,showDetail:(_,s)=>{assert.equal(s,taskTab==='activity'?current:history)}});
 vm.runInContext('(function(){'+detailAction+'})()',ctx);
}
{
// Execute the real detail dispatch after the current snapshot has advanced.
const pastSource={run:{id:'root'},plan:{tasks:[]},events:[{sequence:1,type:'plan_updated',data:{tasks:[{id:'old-task',goal:'原任务',status:'running',evidence_run_id:'child'}]}},{sequence:2,type:'tool_result',data:{evidence_id:'old-evidence'}}],evidence:[{id:'old-evidence',call_id:'call',request_id:'request',type:'tool_execution',verification:'verified',source_event_sequence:2,content:JSON.stringify({stdout:'截止前原文',exit_code:0}),report_links:[{title:'未来关联正文'}]}],actions:[{call_id:'call',request_id:'request',name:'probe',adapter:'local',status:'completed'}]};
const futureSource={run:{id:'root'},plan:{tasks:[{id:'old-task',goal:'未来任务正文',status:'verified'}]},evidence:[{id:'old-evidence',content:'未来证据正文',verification:'verified'}]};
const evidenceHandler=app.slice(app.indexOf("  if(action==='evidence-detail'){"),app.indexOf("  if(['asset-detail','action-detail','task-detail'].includes(action))"));
for(const reference of ['old-evidence','child/old-evidence']){
  let shown='';const context=vm.createContext({state:{snap:futureSource,replaySnapshot:pastSource,replaySequence:20},el:{dataset:{id:reference},closest:selector=>selector==='[data-history-sequence]'?{dataset:{historyRun:'root',historySequence:'2'}}:null},action:'evidence-detail',detailSnapshot,resolveEvidenceReference,historyDetail,evidenceDetail,esc,btn,label,json,api:async()=>{throw new Error('historical detail fetched current data')},showDetail:(_,html)=>shown=html});
  await vm.runInContext('(async()=>{'+evidenceHandler+'})()',context);
  assert.match(shown,/data-history-sequence="2"/);assert.doesNotMatch(shown,/未来证据正文|未来关联正文/);
  assert.match(shown,reference.includes('/')?/历史信息不完整/:/截止前原文/);
}
const recordHandler=app.split('\n').find(line=>line.includes("if(['asset-detail','action-detail','task-detail'].includes(action))"));
let taskShown='';
const taskContext=vm.createContext({state:{snap:futureSource,replaySnapshot:pastSource,replaySequence:20},el:{dataset:{id:'old-task'},closest:()=>({dataset:{historyRun:'root',historySequence:'2'}})},action:'task-detail',detailSnapshot,historyDetail,esc,recordDetail:(_,record)=>JSON.stringify(record),showDetail:(_,html)=>taskShown=html,api:async()=>{throw new Error('historical task fetched current data')}});
await vm.runInContext('(async()=>{'+recordHandler+'})()',taskContext);
assert.match(taskShown,/原任务/);assert.match(taskShown,/running/);assert.match(taskShown,/data-evidence-run="child"/);assert.doesNotMatch(taskShown,/未来任务正文|verified/);
// Preserve the visit stamped when this particular cancellation button was rendered.
for(const observedVisit of [0,1]){
const cancelButton={tagName:'BUTTON',disabled:false,dataset:{action:'worker-cancel',worker:'w1'},setAttribute(){},removeAttribute(){}};
const cancelBodies=[],cancelState={snap:{workers:[{id:'w1',visit:observedVisit}]}},cancelContext=vm.createContext({state:cancelState,document:{querySelectorAll:()=>[cancelButton]},pending:new Set(),operationKey:()=> 'cancel',action:'worker-cancel',el:cancelButton,id:'root',mutate:async(path,body)=>cancelBodies.push({path,body}),notice(){}});
vm.runInContext(app.slice(app.indexOf('function applyPending(){'),app.indexOf('async function saveConfig(')),cancelContext);
vm.runInContext('applyPending()',cancelContext);cancelState.snap.workers[0].visit=observedVisit+1;vm.runInContext('applyPending()',cancelContext);
const cancelHandler=app.split('\n').find(line=>line.includes("if(action==='worker-cancel')"));
await vm.runInContext('(async()=>{'+cancelHandler+'})()',cancelContext);
assert.equal(cancelBodies[0].path,'/runs/root/workers/w1/cancel');assert.equal(cancelBodies[0].body.expected_visit,observedVisit,'zero and positive visits both fence a stale DOM button');
}
}
const select=app.slice(app.indexOf('async function selectRun('),app.indexOf('async function loadEventHistory('));
assert.ok(select.indexOf('subscribe();')<select.indexOf('await conversationHistory.load('));
// Exercise the exact parsed browser route, not an undefined-link stand-in.
for(const query of ['', '?message=m17', '?request=req', '?event=19', '?tab=planner']){
 const route=parseRunRoute('history/target'+query),order=[],savedCursor={around:'m60'};
 const selected={selected:'other',page:'history',historyDetail:true,snap:{run:{id:'other'}},taskTab:'activity'};
 const context=vm.createContext({state:selected,source:null,refreshTimer:null,revision:0,navigation:0,runViewFields:['taskTab','messageCursor'],runViews:new Map([['target',{taskTab:'activity',messageCursor:savedCursor}]]),cancelRunReads(){},clearTimeout(){},sessionStorage:{setItem(){}},location:{hash:'#history/other'},history:{pushState(){},replaceState(){}},runLink,render(){},refreshRun:async id=>{order.push('workspace');selected.snap={run:{id}};selected.runLoading=false},subscribe(){order.push('sse')},conversationHistory:{load:async cursor=>{order.push('history');assert.equal(cursor,savedCursor)}},loadEventHistory:async()=>order.push('event-history'),loadCatalog:async()=>{},revealDeepLink:async()=>order.push('reveal'),runManagement:{reconcile:async()=>null}});
 vm.runInContext(select,context);context.route=route;
 await vm.runInContext('selectRun(route.runID,"history",{replace:true,deepLink:route.deepLink})',context);
 assert.equal(selected.runError,'','route selection failed');
 if(!query){assert.deepEqual(order.slice(0,3),['workspace','sse','history']);assert.equal(selected.deepLink,undefined)}
 else {assert.ok(!order.includes('history'),'real locator must take precedence over saved cursor');assert.deepEqual(selected.deepLink,route.deepLink)}
 // Plain same-run hash navigation must reconnect: hashchange closed SSE first.
 if(!query){order.length=0;await vm.runInContext('selectRun(route.runID,"history",{replace:true,deepLink:route.deepLink})',context);assert.deepEqual(order.slice(0,3),['workspace','sse','history'])}
}
assert.match(app,/activity=true/);assert.match(app,/tab!==\(state.taskTab\|\|'activity'\)/);
console.log('P0 workspace grouping, lazy graph, source reports, anchors and completion forms PASS');
