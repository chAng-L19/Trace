import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import {runWorkspace} from './static/workspace.js';
import {redraw} from './static/interactions.js';
import {workerSummary} from './static/workers.js';
import {conversation} from './static/conversation.js';
import {pendingTools,recordDetail,resourceLimitReason} from './static/workforms.js';
import {settings} from './static/settings.js';
import {renderPage,resolveRoute} from './static/views.js';
import {createChainEvents} from './static/evidence-chain-events.js';

const run={id:'r',goal:'Fixture',status:'paused',usage:{},pause_reason:'tool outcome uncertain; inspect side effects and submit its result manually'};
const action={call_id:'c',request_id:'q',name:'exec',status:'uncertain',result:{error:'thread error',worker_resource_limit:'pids.events:max 1'}};
const snap={run,messages:[],requests:[{id:'q'}],actions:[action],plan:{tasks:[{id:'short-title',goal:'A very long worker objective that should stay in its tooltip'}]},workers:[{id:'w',task_id:'short-title',run_id:'child',status:'uncertain',cleanup_status:'completed'}]};
const state={snap,tools:[],config:{provider:{}},settingTab:'tools',runs:[run]};
const html=runWorkspace(state);
assert.match(html,/<aside class="task-inspector"[^>]*><section class="workspace-composer"[^>]*><form id="message-form"/);
assert.ok(html.indexOf('id="message-form"')<html.indexOf('class="workspace-secondary"'));
assert.ok(html.indexOf('id="run-mailbox"')<html.indexOf('class="workspace-secondary"'));
assert.ok(html.indexOf('id="workspace-notice"')<html.indexOf('class="conversation"'));
assert.ok(html.indexOf('id="workspace-notice"')<html.indexOf('class="workspace-secondary"'));
// Repaint and navigation keep the actual live-region node, its message and timer target.
{
  const oldDocument=globalThis.document;
  const notice={hidden:false,textContent:'操作失败',className:'error'};
  const home={append(node){node.parentElement=this}};
  let host;
  const page={querySelectorAll:()=>[],set innerHTML(value){
    assert.equal(notice.parentElement,home,'notice must leave the subtree before replacement');
    host=value.includes('id="workspace-notice"')?{append(node){node.parentElement=this}}:null;
  }};
  const workspace={scrollTop:0,scrollTo(){}};
  globalThis.document={activeElement:null,getElementById:id=>({'page':page,'workspace':workspace,'notice':notice,'notice-home':home,'workspace-notice':host})[id]||null,querySelectorAll:()=>[],querySelector:()=>null};
  try{
    redraw({page:'history',selected:'notice-fixture',taskTab:'activity'},html,true);
    assert.equal(notice.parentElement,host);
    redraw({page:'history',selected:'notice-fixture',taskTab:'activity'},html+' ',true);
    assert.equal(notice.parentElement,host);
    assert.equal(notice.textContent,'操作失败');assert.equal(notice.className,'error');assert.equal(notice.hidden,false);
    redraw({page:'overview'},'<section>任务中枢</section>',true);
    assert.equal(notice.parentElement,home);
  }finally{globalThis.document=oldDocument}
}
assert.match(workerSummary(snap),/>short-title<\/span>/);
assert.match(workerSummary(snap),/title="A very long worker objective/);
const workerTitle=workerSummary(snap).match(/<span\b[^>]*\bdata-worker-title\b[^>]*>/)?.[0];
assert.ok(workerTitle,'Worker titles have a stable styling hook');
assert.match(workerSummary(snap),/<details id="workspace-workers" data-worker-activity class="run-live-status">/);
assert.doesNotMatch(workerSummary(snap),/<details[^>]*\bopen\b/,'Worker details start closed');
const longTask='调查'.repeat(100),compact=workerSummary({...snap,workers:[{...snap.workers[0],task_id:longTask}]});
assert.doesNotMatch(compact,new RegExp('>'+longTask+'</span>'),'long task titles do not fill the row');
assert.match(compact,/data-id="child"/,'short labels preserve the real navigation identity');
const longMessage={id:'process-long',role:'assistant',content:'过程记录 '.repeat(200)+'<script>NO_EXEC</script>'};
const transcript=conversation({...snap,messages:[longMessage]});
assert.match(transcript,/<details id="process-message-process-long" class="process-message">/);
assert.match(transcript,/&lt;script&gt;NO_EXEC&lt;\/script&gt;/,'folded content remains escaped and readable');
const finalTranscript=conversation({...snap,run:{...run,status:'completed'},messages:[longMessage]});
assert.doesNotMatch(finalTranscript,/id="process-message-process-long"/,'final conclusion stays visible');
const css=fs.readFileSync(new URL('./static/style-unified.css',import.meta.url),'utf8');
assert.match(html,/class="workspace-resize-hint"/);
assert.match(css,/\.task-workbench \.conversation\{[^}]*56dvh[^}]*resize:vertical/);
assert.doesNotMatch(css,/\.workspace-dialogue-head\{[^}]*position:sticky/);
assert.match(css,/\[data-worker-title\]\{[^}]*text-overflow:ellipsis/);
assert.match(pendingTools(snap),/<details class="pending-tools" open>/);
assert.doesNotMatch(html,/需要核对的调用|class="pending-tools"/,'workspace suppresses the duplicate panel without deleting the ledger');
assert.match(recordDetail('action-detail',action,snap),/thread error/,'the original uncertain call remains inspectable');
assert.match(pendingTools(snap),/Python 线程池/);
const unresolved=pendingTools({...snap,actions:[{...action,result:{...action.result,worker_cleanup_error:'owned process still live'}}]});
assert.match(unresolved,/执行状态或部分操作结果尚待核对/);
assert.match(unresolved,/不会自动重放，Agent 会继续处理/);
assert.match(unresolved,/补充观测是可选项/);
assert.doesNotMatch(unresolved,/提交观测后再继续|等待人工核对/);
assert.doesNotMatch(unresolved,/执行已结束/,'uncertain does not establish process exit or completed cleanup');
assert.match(resourceLimitReason('output_bytes'),/输出达到/);
assert.doesNotMatch(recordDetail('action-detail',action,snap),/id="tool-observation-form"/,'consumed historical calls do not offer a failing observation form');
const pendingSnap={...snap,messages:[{role:'assistant',request_id:'q',tool_calls:[{id:'c',name:'exec',arguments:'{}'}]}]};
assert.match(recordDetail('action-detail',action,pendingSnap),/id="tool-observation-form"/);
assert.doesNotMatch(recordDetail('action-detail',{...action,status:'completed'},snap),/id="tool-observation-form"/);
assert.match(settings(state),/MCP 管理：/);
assert.match(settings({...state,settingTab:'local'}),/浏览器扩展/);
assert.doesNotMatch(settings({...state,settingTab:'local'}),/href="#local-tools"/,'legacy route remains readable without a visible navigation link');
assert.equal(resolveRoute('#mcp').settingTab,'tools');
assert.equal(resolveRoute('#local-tools').settingTab,'local');
const loading=renderPage({...state,page:'history',historyDetail:true,runLoading:true,snap:null});
assert.match(loading,/<div class="workspace-dialogue-head"><div id="workspace-notice"><\/div><header class="workspace-heading">[^]*?<\/header><\/div><div class="conversation">/);
assert.doesNotMatch(loading,/<form|<textarea/,'loading exposes the reader, not an editor for the previous run');
const index=fs.readFileSync(new URL('./static/index.html',import.meta.url),'utf8');
assert.equal((index.match(/href="#local-tools"/g)||[]).length,0);
assert.match(index,/MCP 管理/);

// Exercise the production submit listener: one inline form error, zero toast.
const app=fs.readFileSync(new URL('./static/app.js',import.meta.url),'utf8');
const handlers={},feedback=[],notices=[];
const modal={open:true,remove(){}};
const form={id:'run-delete-form',dataset:{},isConnected:true,closest:()=>modal,querySelectorAll:()=>[]};
const ctx=vm.createContext({document:{addEventListener:(event,fn)=>handlers[event]=fn},operationKey:()=> 'delete',pending:new Set(),scopedNotice:()=> (...args)=>notices.push(args),state:{},detailRevision:1,navigation:1,applyPending(){},runManagement:{submit:async()=>{throw new Error('blocked')}},formFeedback:(...args)=>feedback.push(args),$:()=>modal,render(){}});
const start=app.indexOf("document.addEventListener('submit',");
vm.runInContext(app.slice(start,app.indexOf("document.addEventListener('change',",start)),ctx);
await handlers.submit({target:form,preventDefault(){}});
assert.equal(feedback.length,1);
assert.equal(notices.length,0);

// Execute the actual catalog-delete submit branch, including per-item failures
// that never reach the outer submit catch. UI feedback belongs to its dialog.
const submitStart=app.indexOf('async function submit(form)');
const submitSource=app.slice(submitStart,app.indexOf('async function runIntegration(',submitStart));
async function catalogDeletion({fail=false,warning=false,stale='',keys=['r/e1']}={}){
  const result={details:[],notices:[],calls:[],closed:0};
  let context;
  context=vm.createContext({
    scopedNotice:()=> (...args)=>result.notices.push(args),state:{selected:'r'},navigation:1,detailRevision:1,
    api:async(path,method)=>{result.calls.push({path,method});if(fail)throw new Error('referenced by another run');return warning?{cleanup_warning:'cleanup remains pending'}:{}},
    refresh:async()=>{if(stale==='navigation')context.navigation++;if(stale==='dialog')context.detailRevision++},
    showDetail:(title,html)=>{context.detailRevision++;result.details.push({title,html})},
    esc:value=>String(value),$:()=>({close(){result.closed++;context.detailRevision++}})
  });
  vm.runInContext(submitSource,context);
  await context.submit({id:'catalog-delete-form',dataset:{keys:JSON.stringify(keys)}});
  return result;
}
const denied=await catalogDeletion({fail:true,keys:['r/e1','r/e2']});
assert.equal(denied.calls.length,2);
assert.equal(denied.details.length,1);
assert.match(denied.details[0].html,/已删除 0 \/ 2 条证据/);
assert.match(denied.details[0].html,/referenced by another run/);
assert.equal(denied.notices.length,0,'failed catalog deletion has one error surface');
const warned=await catalogDeletion({warning:true});
assert.equal(warned.details.length,1);
assert.match(warned.details[0].html,/已删除 1 \/ 1 条证据/);
assert.match(warned.details[0].html,/cleanup remains pending/);
assert.equal(warned.notices.length,0);
const deleted=await catalogDeletion();
assert.equal(deleted.closed,1);
assert.equal(deleted.notices.length,1);
assert.equal(deleted.details.length,0);
for(const stale of ['navigation','dialog']){
  const late=await catalogDeletion({fail:true,stale});
  assert.equal(late.calls.length,1);
  assert.equal(late.details.length,0,'late deletion must not replace another dialog');
  assert.equal(late.notices.length,0,'late deletion must not leave an unrelated error toast');
  assert.equal(late.closed,0);
}

// Use the real chain handler: beginDetail advances its token before DELETE,
// but an error still belongs to the same mounted form. Replaced/closed forms
// and navigation must receive neither a stale inline error nor a toast.
async function chainDeletionFailure(stale=''){
  const result={feedback:[],notices:[],calls:[],beginCount:0},listeners={};
  const dialog={open:true},banner={hidden:false},inline={removed:false,remove(){this.removed=true}};
  const form={id:'chain-delete-form',dataset:{run:'r',chain:'chain-1'},isConnected:true,querySelectorAll:()=>[],closest:selector=>form.isConnected?dialog:null};
  const context=vm.createContext({
    document:{addEventListener:(event,fn)=>listeners[event]=fn},operationKey:()=> 'chain-delete',pending:new Set(),
    scopedNotice:()=> (...args)=>result.notices.push(args),state:{page:'evidence',selected:'r'},detailRevision:1,navigation:1,
    applyPending(){},runManagement:{submit:async()=>false},formFeedback:(...args)=>result.feedback.push(args),
    $:selector=>selector==='#detail-dialog'?dialog:selector==='#notice'?banner:inline,render(){}
  });
  context.chainEvents=createChainEvents({
    state:context.state,notice(){},render(){},refresh:async()=>{},showDetail(){throw new Error('unexpected detail replacement')},
    beginDetail:()=>{result.beginCount++;return {nav:context.navigation,detail:++context.detailRevision,run:'r'}},
    isCurrent:token=>token.nav===context.navigation&&token.detail===context.detailRevision,
    api:async(path,method)=>{
      result.calls.push({path,method});
      if(stale==='navigation')context.navigation++;
      if(stale==='dialog'){form.isConnected=false;context.detailRevision++}
      if(stale==='closed'){dialog.open=false;context.detailRevision++}
      throw new Error('chain is referenced by another run');
    }
  });
  vm.runInContext(submitSource,context);
  vm.runInContext(app.slice(start,app.indexOf("document.addEventListener('change',",start)),context);
  await listeners.submit({target:form,preventDefault(){}});
  assert.equal(result.beginCount,1,'exercise the real beginDetail token change');
  assert.deepEqual(result.calls,[{path:'/evidence-chains/r/chain-1',method:'DELETE'}]);
  assert.equal(result.notices.length,0);
  assert.equal(result.feedback.length,stale?0:1);
  assert.equal(banner.hidden,!stale);
  assert.equal(inline.removed,!stale);
  if(!stale)assert.equal(result.feedback[0][3],true);
}
for(const stale of ['','navigation','dialog','closed'])await chainDeletionFailure(stale);
console.log('UI/runtime: composer, graph order, unified tools, short titles, uncertainty and single error surface passed');

const plannerHTML=renderPage({...state,page:'history',historyDetail:true,taskTab:'planner',runs:[run]});
assert.match(plannerHTML,/<details id="run-goal"/);
assert.equal((plannerHTML.match(/id="task-goal"/g)||[]).length,1,'global task-goal focus resolves only to its textarea');
assert.match(plannerHTML,/<textarea id="task-goal"/);

const projectedPlan=renderPage({...state,page:'history',historyDetail:true,taskTab:'planner',snap:{...snap,projection:'planner',plan:{tasks:[{id:'old',goal:'Historical task',status:'verified'}],frontier:[]}}});
assert.match(projectedPlan,/已记录 · 待核验/);
assert.match(projectedPlan,/计划显示已记录状态/);
assert.doesNotMatch(projectedPlan,/class="status verified"/,'display planner does not promote recorded task state to a fresh source check');

const projectedTaskDetail=recordDetail('task-detail',{id:'old',goal:'Historical task',status:'verified'},{...snap,projection:'planner'});
assert.match(projectedTaskDetail,/已记录 · 待核验/);
assert.doesNotMatch(projectedTaskDetail,/class="status verified"/);
assert.match(recordDetail('task-detail',{id:'checked',status:'verified'},snap),/class="status verified"/);
