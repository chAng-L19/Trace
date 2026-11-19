import assert from 'node:assert/strict';
import {occupiedWorkerCount} from './static/ui.js';
import fs from 'node:fs';
import vm from 'node:vm';
import {workerGroups,pendingDispatch,workerSummary,workerPanel} from './static/workers.js';
import {taskGroups,renderPage} from './static/views.js';
import {explorationData,renderExplorationDetail} from './static/exploration.js';
import {captureScroll,restoreScroll} from './static/interactions.js';
import {creationOptions,readCreationOptions,parseRunRoute,runLink} from './static/run-management.js';
import {taskForm,readTask} from './static/workforms.js';
import {explorationPreview} from './static/graphs.js';

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

const report={source_run_id:'child1',parent_run_id:'root',task_id:'active',worker_id:'w1',visit:1,source_hash:'abc',run_status:'completed',result_status:'incomplete',cleanup_status:'unknown',source_status:'valid',current:true,summary:'child conclusion',report_path:'/api/runs/child1/reports/abc',findings:[],unresolved:['pending']};
const graph=explorationData({...snap,child_reports:[report,report,{...report,visit:2,source_hash:'def'}]});
assert.equal(graph.nodes.filter(n=>n.kind==='child_report').length,2);
assert.equal(graph.nodes.filter(n=>n.kind==='source').length,2);
assert.equal(graph.nodes.filter(n=>n.kind==='observation').length,0,'child refs are not parent execution proof');
assert.ok(graph.edges.some(e=>e.from==='task:active'&&e.relation==='子结算报告'));
assert.match(renderExplorationDetail({...snap,child_reports:[report]},graph.nodes.find(n=>n.kind==='child_report').id),/读取固定版本源报告/);
const preview=explorationPreview({...snap,child_reports:[report,report,{...report,visit:2,source_hash:'def'}]});
assert.equal((preview.match(/class="child_report"/g)||[]).length,2,'distinct visits expand distinct report nodes');
assert.equal((preview.match(/class="source"/g)||[]).length,2,'fixed source versions stay visible');
assert.match(preview,/<title>子结算报告<\/title>/);
assert.match(preview,/<title>固定来源指针<\/title>/);
assert.doesNotMatch(preview,/class="observation"/,'child receipts never become parent execution proof');
assert.doesNotMatch(explorationPreview({run:snap.run}),/class="intent"|class="child_report"/,'empty plans do not invent work');
const large=explorationPreview({...snap,plan:{tasks:Array.from({length:100},(_,i)=>({id:'t'+i,status:'pending',goal:'<script>'+i}))},child_reports:Array.from({length:30},(_,i)=>({...report,task_id:'t'+i,source_hash:'hash'+i}))});
assert.ok((large.match(/data-node-search=/g)||[]).length<=22,'preview DOM stays bounded');
assert.match(large,/task:t29/,'recent report tasks retain their real ancestors');
assert.doesNotMatch(large,/<script>/,'untrusted node titles are escaped');

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
