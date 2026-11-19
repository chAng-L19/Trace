import {boundedCache} from './static/interactions.js';
const createContext=values=>vm.createContext(Object.assign(values,{boundedCache,scopedNotice(){return (...args)=>values.notice?.(...args)}}));
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import {inspectionCalls,messageCallRecords,conversation,toolCard,callLinks} from './static/conversation.js';
import {redraw} from './static/interactions.js';
import {evidenceCard,evidenceDetail} from './static/evidence.js';
import {pendingTools,recordDetail} from './static/workforms.js';

const run={id:'conversation-fixture',status:'running'};
const read={call_id:'read-1',name:'read',arguments:'{"path":"first.txt"}',status:'completed',result:{stdout:'READ_OUTPUT_MARKER',exit_code:0}};
const edit={call_id:'edit-2',name:'write',arguments:'{"path":"second.txt","content":"WRITE_INPUT_MARKER"}',status:'completed',result:{stdout:'WRITE_OUTPUT_MARKER',exit_code:0}};
const exec={call_id:'exec-3',name:'exec',arguments:'{"command":"printf COMMAND_INPUT_MARKER"}',status:'started'};
const user={id:'user',role:'user',content:'检查这两个文件。'};
const assistant={id:'assistant',role:'assistant',content:'先读取文件，再检查结果。',thinking:'THINKING_MARKER',tool_calls:[{id:read.call_id,name:read.name,arguments:read.arguments}]};
const single={run,messages:[user,assistant],actions:[read],events:[]};
const grouped={...single,messages:[...single.messages,{id:'tool-return',role:'tool',tool_call_id:read.call_id,content:'duplicate-output'},{id:'second-call',role:'assistant',tool_calls:[{id:edit.call_id,name:edit.name,arguments:edit.arguments},{id:exec.call_id,name:exec.name,arguments:exec.arguments}]}],actions:[read,edit,exec],events:[{type:'tool_output',data:{call_id:exec.call_id,stream:'stderr',content:'STDERR_PROGRESS_MARKER'}},{type:'tool_progress',data:{call_id:exec.call_id,message:'检查中',progress:1,total:3}}]};
const html=conversation(grouped),firstSummary=html.match(/class="tool-disclosure tool-group"><summary>(.*?)<\/summary>/s)?.[1];
assert.ok(firstSummary);
assert.match(firstSummary,/读取了文件 · 编辑了文件 · 运行命令/);
assert.match(firstSummary,/检查中 · 1 \/ 3/);
assert.doesNotMatch(firstSummary,/OUTPUT_MARKER|INPUT_MARKER|STDERR_PROGRESS_MARKER|耗时|秒/);
assert.doesNotMatch(html,/<header>|<time>|Trace|duplicate-output/);
assert.match(html,/class="message user"/);
assert.match(html,/data-action="branch" data-message="assistant"/);
assert.match(html,/<summary>思考<\/summary>/);
for(const text of ['READ_OUTPUT_MARKER','WRITE_INPUT_MARKER','WRITE_OUTPUT_MARKER','COMMAND_INPUT_MARKER','STDERR_PROGRESS_MARKER','THINKING_MARKER'])assert.ok(html.includes(text),text);
assert.doesNotMatch(html,/<details[^>]*class="tool-disclosure[^"]*"[^>]*\sopen(?:\s|>)/,'tool input/output remain opt-in');
assert.match(html,/<details[^>]*class="reasoning-disclosure" data-reasoning-phase="done">/,'archived reasoning starts collapsed');
assert.match(html,/id="chat-call-read-1"/,'group retains single-call disclosure identity');
assert.match(html,/id="chat-item-call-exec-3"/,'each grouped call has an independent disclosure');
assert.equal((html.match(/id="chat-call-read-1"/g)||[]).length,1);
const interrupted=toolCard({...exec,status:'completed',result:{exit_code:1,stderr:'ACTUAL_FAILURE'}},grouped);
assert.match(interrupted.match(/<summary>(.*?)<\/summary>/s)[1],/status failed/);
assert.match(interrupted,/ACTUAL_FAILURE/);
assert.doesNotMatch(interrupted.match(/<summary>(.*?)<\/summary>/s)[1],/ACTUAL_FAILURE/);
assert.match(conversation({run,requests:[{id:'failed-request'}],responses:[{request_id:'failed-request',error:'EMPTY_RESPONSE_ERROR'}]}),/EMPTY_RESPONSE_ERROR/,'failure before first token remains visible');
const separated=conversation({...single,messages:[...single.messages,{id:'interlude',role:'assistant',content:'这一段正文隔开工具操作。'},{id:'next',role:'assistant',tool_calls:[{id:edit.call_id,name:edit.name,arguments:edit.arguments}]}],actions:[read,edit]});
assert.doesNotMatch(separated,/tool-group/,'do not group across assistant prose');
const reused={run,messages:[
  {id:'m-first',role:'assistant',content:'FIRST_PROSE',tool_calls:[{id:'same',name:'exec',arguments:'{"command":"first"}'}]},
  {id:'t-first',role:'tool',tool_call_id:'same',content:'{"stdout":"FIRST_REPLY"}'},
  {id:'m-second',role:'assistant',content:'SECOND_PROSE',tool_calls:[{id:'same',name:'exec',arguments:'{"command":"second"}'}]},
  {id:'t-second',role:'tool',tool_call_id:'same',content:'{"stdout":"SECOND_REPLY"}'},
],actions:[{...exec,call_id:'same',request_id:'r-first',arguments:'{"command":"first"}',status:'completed',result:{stdout:'FIRST_ACTION'}},{...exec,call_id:'same',request_id:'r-second',arguments:'{"command":"second"}'}],events:[
  {type:'model_request',data:{request_id:'r-first'}},
  {type:'action_started',data:{request_id:'r-first',call_id:'same'}},
  {type:'tool_output',data:{call_id:'same',stream:'stderr',content:'STALE_STDERR'}},
  {type:'model_request',data:{request_id:'r-second'}},
  {type:'action_started',data:{request_id:'r-second',call_id:'same'}},
  {type:'tool_output',data:{call_id:'same',stream:'stderr',content:'CURRENT_STDERR'}},
]};
const reusedHTML=conversation(reused);
assert.match(reusedHTML,/id="chat-call-r-first:same"/);
assert.match(reusedHTML,/id="chat-call-r-second:same"/);
const secondCall=reusedHTML.slice(reusedHTML.indexOf('id="chat-call-r-second:same"'));
assert.match(secondCall,/CURRENT_STDERR/);
assert.doesNotMatch(secondCall,/STALE_STDERR|FIRST_ACTION|FIRST_REPLY/);
const manualReuse=conversation({...reused,actions:[],events:[]});
assert.match(manualReuse,/FIRST_REPLY/);
assert.match(manualReuse,/SECOND_REPLY/);
assert.match(manualReuse,/id="chat-call-m-first:same"/);
assert.match(manualReuse,/id="chat-call-m-second:same"/);
const overview=conversation({...single,run:{...run,elapsed_millis:12345},totals:{actions:64,requests:2}},{lazy:true});
assert.match(overview,/<details id="conversation-work-summary"[^>]*><summary>用时 12.3 秒/);
assert.match(overview,/2 次模型请求/);
assert.doesNotMatch(overview,/次工具动作/);
assert.doesNotMatch(conversation(single),/用时 [\d.]+ 秒/,'missing timing is not fabricated');
const evidence=[{id:'ev-first',call_id:'same',request_id:'r-first',content:'FIRST_EVIDENCE'},{id:'ev-second',call_id:'same',request_id:'r-second',content:'SECOND_EVIDENCE'}];
const linked={...reused,evidence},links=callLinks(reused.actions[1],linked);

assert.match(links,/data-action="action-detail" data-id="same" data-request="r-second"/);
assert.match(links,/data-id="ev-second"/);
assert.doesNotMatch(links,/ev-first/);
for(const renderer of [evidenceCard,evidenceDetail])assert.match(renderer(evidence[1],linked),/data-id="same" data-request="r-second"/);
const messageLinks=callLinks(linked.actions.at(-1),linked);
assert.match(messageLinks,/data-id="same" data-request="r-second"/);
assert.doesNotMatch(messageLinks,/ev-first/);
const actionReading=recordDetail('action-detail',linked.actions[0],linked);
assert.match(actionReading,/ev-first/);
assert.doesNotMatch(actionReading,/ev-second/);
const pending=pendingTools({...linked,run:{...run,status:'paused'},messages:linked.messages.slice(0,-1),actions:[linked.actions[0],{...linked.actions[1],status:'uncertain'}]});
assert.match(pending,/data-id="same" data-request="r-second"/);
const legacy={run,actions:[{...read,call_id:'old'}],messages:[],events:[]};
assert.match(callLinks(legacy.actions[0],legacy),/data-id="old" data-request=""/);
const app=fs.readFileSync(new URL('./static/app.js',import.meta.url),'utf8');
let inspected;
const dispatchContext=createContext({chainEvents:{action:async()=>false},state:{snap:linked},inspectionCalls,messageCallRecords,showDetail(title,body){inspected=body},recordDetail(kind,item,snapshot){return {item,evidence:snapshot.evidence}}});
vm.runInContext(app.slice(app.indexOf('async function dispatch('),app.indexOf('async function submit(')),dispatchContext);
await vm.runInContext("dispatch('action-detail',{dataset:{id:'same',request:'r-second'}})",dispatchContext);
assert.equal(inspected.item.request_id,'r-second');
assert.deepEqual(inspected.evidence.map(item=>item.id),['ev-second']);
await assert.rejects(vm.runInContext("dispatch('action-detail',{dataset:{id:'same'}})",dispatchContext),/多个请求中复用/,'ambiguous historical links never open a random action');
dispatchContext.state.snap=legacy;
await vm.runInContext("dispatch('action-detail',{dataset:{id:'old'}})",dispatchContext);
assert.equal(inspected.item.call_id,'old');

// Exercise production redraw with a DOM adapter for details/scroll state. A group
// growing during streaming must preserve the user's open and closed choices.
let details=[];
const page={querySelectorAll:selector=>selector==='details'?details:[],set innerHTML(value){details=[...value.matchAll(/<details\s+id="([^"]+)"[^>]*>/g)].map(match=>({id:match[1],open:false}))}};
const workspace={scrollTop:40,scrollTo({top}){this.scrollTop=top}};
const previous=globalThis.document;
globalThis.document={activeElement:null,getElementById:id=>id==='page'?page:id==='workspace'?workspace:null,querySelectorAll:()=>[],querySelector:()=>null};
try{
  const state={page:'history',selected:run.id,taskTab:'activity'};
  redraw(state,conversation(single));
  workspace.scrollTop=40; // User scroll after entering the new scope.
  const byId=id=>details.find(item=>item.id===id);
  byId('chat-call-read-1').open=true;
  byId('thinking-assistant').open=true;
  redraw(state,html);
  assert.equal(byId('chat-call-read-1').open,true,'open single tool stays open when more calls arrive');
  assert.equal(byId('thinking-assistant').open,true,'reasoning state survives repaint');
  byId('chat-item-call-exec-3').open=true;
  const next={...grouped,events:[...grouped.events,{type:'tool_output',data:{call_id:exec.call_id,stream:'stdout',content:'NEW_STREAM_OUTPUT'}}]};
  redraw(state,conversation(next));
  assert.equal(byId('chat-item-call-exec-3').open,true,'stream update preserves individual expansion');
  byId('chat-call-read-1').open=false;
  redraw(state,conversation({...next,actions:[read,edit,{...exec,status:'completed',result:{stdout:'FINAL_OUTPUT',exit_code:0}}]}));
  assert.equal(byId('chat-call-read-1').open,false,'completion does not reopen a user-collapsed group');
  assert.equal(workspace.scrollTop,40);
}finally{if(previous===undefined)delete globalThis.document;else globalThis.document=previous}
console.log('Conversation: quiet action rows, contiguous grouping, complete payloads, errors, and persistent disclosure state.');
