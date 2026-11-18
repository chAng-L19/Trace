import {boundedCache} from './static/interactions.js';
const createContext=values=>vm.createContext(Object.assign(values,{boundedCache,scopedNotice(){return (...args)=>values.notice?.(...args)}}));
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import {eventLabel} from './static/event-labels.js';
import {renderPage} from './static/views.js';
import {viewScope} from './static/interactions.js';
import {esc} from './static/ui.js';
import {resultPanel,recordDetail} from './static/workforms.js';
import {conversation,toolCard,toolOutput,messageCallRecords,callStatus,inspectionCalls} from './static/conversation.js';
import {markdown,messageBody,streamingBody,contentSummary} from './static/presentation.js';

const run={id:'run',goal:'研究目标 <fixture>',status:'running',usage:{}};
const events=[
  {sequence:1,type:'model_stream',data:{request_id:'r1',delta:{kind:'thinking',text:'先检查'}}},
  {sequence:2,type:'model_stream',data:{request_id:'r1',delta:{kind:'thinking',text:'配置。'}}},
  {sequence:3,type:'model_stream',data:{request_id:'r1',delta:{kind:'text',text:'过时增量'}}},
  {sequence:4,type:'tool_output',data:{call_id:'c1',stream:'stdout',content:'历史预览'}},
  {sequence:5,type:'model_stream',data:{request_id:'r3',delta:{kind:'text',text:'实时'}}},
  {sequence:6,type:'model_stream',data:{request_id:'r3',delta:{kind:'text',text:'回复'}}},
  {sequence:7,type:'model_stream',data:{request_id:'r3',delta:{kind:'tool_call',index:0,name:'read',arguments:'{"path":'}}},
  {sequence:8,type:'model_stream',data:{request_id:'r3',delta:{kind:'tool_call',index:0,arguments:'"<file>"}'}}},
];
const snap={run,requests:[{id:'r1'},{id:'r2'},{id:'r3'}],responses:[{request_id:'r1',response:{content:'最终回复',thinking:'已归档思考'}},{request_id:'r2',response:{},error:'Provider 请求失败'}],actions:[{request_id:'r1',call_id:'c1',name:'read',arguments:'{"path":"<file>"}',status:'completed',result:{stdout:'实际完整输出',stderr:'',exit_code:0,artifacts:[{kind:'stdout',hash:'abcd',bytes:10}]}}],evidence:[{id:'ev',call_id:'c1',verification:'verified'}],messages:[],assets:[],events,plan:{tasks:[]}};
const state={page:'history',selected:'run',runs:[run],historyFilter:'',tools:[],snap};
assert.equal(eventLabel('model_request'),'提交模型请求');
assert.equal(eventLabel('vendor_event'),'vendor_event');

const task=renderPage(state);
assert.match(task,/aria-label="当前任务内容"/);
assert.doesNotMatch(task,/aria-label="研究目标、探索方向、观测与发现关系图"/,'activity defers graph mounting');
assert.match(renderPage({...state,taskTab:'assets'}),/aria-label="研究目标、探索方向、观测与发现关系图"/,'graph remains accessible on demand');
assert.doesNotMatch(task,/aria-label="实时轨迹"/,'hidden trajectory is not generated');
assert.match(task,/id="message-form"/);
assert.match(task,/<details id="run-goal" class="task-goal"><summary>/);
assert.match(task,/<pre>研究目标 &lt;fixture&gt;<\/pre>/,'full goal is readable without truncating its content');
assert.match(task,/class="task-control-row"/);
assert.doesNotMatch(task,/stream-label/);
for(const tab of ['assets','evidence','artifacts','result','planner','events']){
  const page=renderPage({...state,taskTab:tab});
  assert.equal((page.match(/id="run-selector"/g)||[]).length,0,tab+' stays within the selected task without another selector');
  assert.match(page,new RegExp(`id="task-tab-${tab}" aria-current="page"`));
  assert.ok(page.includes('研究目标 &lt;fixture&gt;'));
  assert.notEqual(viewScope({...state,taskTab:tab}),viewScope(state),'task sections retain separate reading/draft state');
}
for(const page of ['assets','planner','events'])assert.equal((renderPage({...state,page}).match(/id="run-selector"/g)||[]).length,1,page+' retains the standalone run selector');
assert.equal((renderPage({...state,page:'evidence',catalog:[]}).match(/id="run-selector"/g)||[]).length,0,'global evidence manager is independent of selected run');
assert.match(renderPage({...state,taskTab:'artifacts'}),/文件产物/);
assert.doesNotMatch(renderPage({...state,taskTab:'artifacts'}),/id="record-panel-evidence"/,'hidden evidence cards are not generated');

// Exercise the production artifact failure/back path without app boot or a network.
const app=fs.readFileSync(new URL('./static/app.js',import.meta.url),'utf8');
const section=(start,end)=>app.slice(app.indexOf(start),app.indexOf(end,app.indexOf(start)));
const elements={'#detail-dialog':{open:true},'#dialog-title':{textContent:'证据 ev'},'#dialog-content':{innerHTML:'证据来源内容'}};
const context=createContext({chainEvents:{action:async()=>false},state:{selected:'run'},$:selector=>elements[selector],esc,detailRevision:0,navigation:0,artifactOrigin:null,api:async()=>{throw Error('HTTP 404')}});
context.showDetail=(title,content)=>{context.detailRevision++;elements['#dialog-title'].textContent=title;elements['#dialog-content'].innerHTML=content};
vm.runInContext(section('async function dispatch(','async function submit('),context);
await assert.rejects(vm.runInContext("dispatch('artifact',{dataset:{hash:'missing'}})",context),/HTTP 404/);
assert.equal(elements['#dialog-title'].textContent,'产物读取失败');
assert.match(elements['#dialog-content'].innerHTML,/data-action="artifact-back"/);
assert.doesNotMatch(elements['#dialog-content'].innerHTML,/正在读取/);
await vm.runInContext("dispatch('artifact-back',{dataset:{}})",context);
assert.equal(elements['#dialog-title'].textContent,'证据 ev');
assert.equal(elements['#dialog-content'].innerHTML,'证据来源内容');

const selection=createContext({source:null,refreshTimer:null,cancelRunReads(){},clearTimeout(){},runViews:new Map(),runViewFields:[],refreshRun:async()=>{},runLink:id=>'#history/'+id,revealDeepLink:async()=>{},runManagement:{reconcile:async()=>null},history:{pushState(){}},revision:0,navigation:0,state:{selected:'old',page:'history',graphFilter:'verified',taskTab:'assets'},$:()=>null,api:async()=>({run:{id:'next'}}),sessionStorage:{setItem(){}},location:{hash:'#history'},render(){},subscribe(){},notice(){}});
vm.runInContext(section('async function selectRun(','function showDetail('),selection);
await vm.runInContext("selectRun('next')",selection);
assert.equal(selection.state.selected,'next');
assert.equal(selection.state.graphFilter,'');
assert.equal(selection.state.taskTab,'activity');
const modelMessage=messageBody({id:'summary',role:'assistant',content:JSON.stringify({trace_result:{status:'success',summary:'中文任务摘要 <result>',answers:[{value:'**实际答案**',evidence_ids:['ev']}],reproduction_steps:['执行检查']}})});
assert.match(modelMessage,/中文任务摘要 &lt;result&gt;/);
assert.match(modelMessage,/<strong>实际答案<\/strong>/);
assert.match(modelMessage,/复现步骤/);
assert.match(modelMessage,/<details id="message-raw-summary" class="request-details raw-record"><summary>原始模型消息/);
assert.doesNotMatch(modelMessage,/status success/,'model-reported status must not become a verified badge');
const toolMessage=toolOutput({stdout:JSON.stringify({title:'页面标题',text:'实际输出 <fixture>'}),stderr:'诊断消息',exit_code:2,error:'执行错误'});
for(const text of ['实际输出 &lt;fixture&gt;','页面标题','诊断消息','执行错误','退出码 2'])assert.ok(toolMessage.includes(text),text);
assert.ok(toolMessage.indexOf('实际输出 &lt;fixture&gt;')<toolMessage.indexOf('标准错误'));
assert.match(toolOutput({stdout:'',stderr:'',exit_code:0}),/无文本输出/);
assert.match(messageBody({role:'assistant',content:'# 正文\n\n- 条目\n\n<plain>'}),/<h2>正文<\/h2>/);
assert.match(messageBody({role:'assistant',content:'{"other":true}'}),/<dt>other<\/dt><dd>是<\/dd>/);
assert.match(toolOutput({tools:[{name:'exec',description:'执行命令'}]}),/执行命令/,'builtin tool catalog outputs remain visible');
assert.equal(typeof contentSummary('{"summary":{"nested":true}}'),'string');
for(const [content,summary] of [
  ['{"trace_result":{"summary":"运行摘要"}}','运行摘要'],
  ['{"final_result":{"summary":"最终摘要"}}','最终摘要'],
  ['```json\n{"summary":"围栏摘要"}\n```','围栏摘要'],
  ['{"summary":"未结束','正在整理结构化记录…'],
  ['```json\n{"summary":','正在整理结构化记录…'],
  ['[1,2]','2 项记录'],['[]','0 项记录'],
  ['null','null'],['false','false'],['0','0'],
  ['# 普通 **文本**\n 下一行','普通 文本 下一行'],
])assert.equal(contentSummary(content),summary,'summary preserves '+content);
const unsafe=markdown('<img src=x onerror=alert(1)>\n\n[bad](javascript:alert(1)) [ok](https://example.test/)\n\n```html\n<script>alert(1)</script>\n```');
assert.doesNotMatch(unsafe,/<img|<script|href="javascript:/);
assert.match(unsafe,/&lt;script&gt;/);
assert.match(unsafe,/href="https:\/\/example.test\/" target="_blank" rel="noopener noreferrer"/);
const partial=streamingBody('{"trace_result":{"summary":"未完成','r');
assert.match(partial,/正在整理结构化结果/);
assert.ok(partial.indexOf('trace_result')>partial.indexOf('<details'),'incomplete JSON is only in collapsed raw disclosure');
const mixedResult='# 检查总结\n\n| 项目 | 状态 |\n| --- | --- |\n| 执行 | 完成 |\n\n```json\n'+JSON.stringify({trace_result:{summary:'结构化总结',answers:[{value:'中文答案',evidence_ids:['ev']}],reproduction_steps:['复现第一步']}})+'\n```';
const mixedHTML=messageBody({id:'mixed',role:'assistant',content:mixedResult});
assert.match(mixedHTML,/<h2>检查总结<\/h2>/);
assert.match(mixedHTML,/<table>/);
assert.match(mixedHTML,/结构化结果详情/);
assert.match(mixedHTML,/中文答案/);
assert.match(mixedHTML,/复现第一步/);
assert.doesNotMatch(mixedHTML,/<div class="code-block">/,'result envelope is not displayed as a JSON code wall');
assert.ok(mixedHTML.indexOf('trace_result')>mixedHTML.indexOf('<details'),'raw envelope remains behind disclosure');
for(const tail of ['{','{"trace_','{"trace_result":{"summary":"未完成']){
  const mixedPartial=streamingBody('# 已完成的检查\n\n```json\n'+tail,'mixed-live');
  assert.match(mixedPartial,/<h2>已完成的检查<\/h2>/);
  assert.match(mixedPartial,/正在整理结构化结果/);
  assert.doesNotMatch(mixedPartial,/<div class="code-block">/);
  if(tail.includes('trace'))assert.ok(mixedPartial.indexOf('trace')>mixedPartial.indexOf('<details'));
}
const finalEnvelope=messageBody({id:'final-envelope',role:'assistant',content:'结论如下。\n\n```json\n{"final_result":{"summary":"最终摘要"}}\n```\n\n后续说明。'});
assert.match(finalEnvelope,/最终摘要/);
assert.match(finalEnvelope,/<p>后续说明。<\/p>/);
assert.doesNotMatch(finalEnvelope,/<div class="code-block">/);
const jsonExample='普通示例\n\n```json\n{"example":1}\n```';
assert.match(messageBody({id:'example',role:'assistant',content:jsonExample}),/class="code-block"/);
assert.match(messageBody({id:'pure-example',role:'assistant',content:'```json\n{"example":1}\n```'}),/class="code-block"/);
assert.match(streamingBody('普通示例\n\n```json\n{"example":','example-stream'),/class="code-block"/,'identified ordinary JSON samples remain code during streaming');
assert.match(messageBody({id:'user-example',role:'user',content:mixedResult}),/class="code-block"/,'user-provided result-envelope examples are kept verbatim');
const mixedPanel=resultPanel({run,result:{status:'success',summary:mixedResult,answers:[]}});
assert.match(mixedPanel,/<h2>检查总结<\/h2>/);
assert.match(mixedPanel,/中文答案/);
assert.doesNotMatch(mixedPanel,/<div class="code-block">/,'run result panel uses the same readable mixed-body rendering');
const paired={...snap,messages:[{id:'assistant',role:'assistant',content:'先检查',tool_calls:[{id:'c1',name:'read',arguments:'{"path":"file"}'}]},{id:'tool',role:'tool',tool_call_id:'c1',content:'{"stdout":"重复工具消息"}'}]};
const chat=conversation(paired);
assert.equal((chat.match(/id="chat-call-r1:c1"/g)||[]).length,1);
assert.doesNotMatch(chat,/重复工具消息/);
const activeTool=conversation({run,messages:[],actions:[{call_id:'live-call',name:'exec',arguments:'{"command":"check"}',status:'started'}],events:[{type:'tool_output',data:{call_id:'live-call',stream:'stdout',content:'实时工具输出'}},{type:'tool_output',data:{call_id:'live-call',stream:'stderr',content:'实时诊断'}},{type:'mcp_progress',data:{call_id:'live-call',message:'正在检查',progress:2,total:5}}]});
assert.match(activeTool,/实时工具输出/);
assert.match(activeTool,/实时诊断/);
assert.match(activeTool,/正在检查 · 2 \/ 5/);
assert.ok(activeTool.indexOf('正在检查 · 2 / 5')<activeTool.indexOf('</summary>'),'live progress is visible without opening the tool');
assert.match(activeTool,/执行中/);
assert.match(chat,/data-action="action-detail" data-id="c1" data-request="r1"/);
assert.match(chat,/id="live-r3"/);
assert.match(chat,/实时回复/);
const archived={...paired,requests:[...paired.requests.slice(0,-1),{...paired.requests.at(-1),messages:paired.messages}],responses:[...paired.responses,{request_id:'r3',response:{content:'实时回复'}}],messages:[...paired.messages,{id:'final',role:'assistant',content:'实时回复'}]};
assert.doesNotMatch(conversation(archived),/id="live-r3"/,'archived response replaces transient stream');
for(const [runStatus,phase] of [['failed','输出中断'],['cancelled','已取消'],['paused','已暂停']]){
  const interruptedChat=conversation({...paired,run:{...run,status:runStatus}});
  assert.ok(interruptedChat.includes(phase),runStatus);
  assert.match(interruptedChat,/实时回复/,'interrupted stream keeps received text');
  assert.doesNotMatch(interruptedChat,/正在生成/);
}
const responseFailure=conversation({...paired,run:{...run,status:'failed'},responses:[...paired.responses,{request_id:'r3',response:{content:'',thinking:''},error:'连接中断'}],events:[...paired.events,{type:'model_stream',data:{request_id:'r3',delta:{kind:'thinking',text:'尚未归档的思考'}}}]});
assert.match(responseFailure,/连接中断/);
assert.match(responseFailure,/实时回复/);
assert.match(responseFailure,/尚未归档的思考/);
assert.doesNotMatch(responseFailure,/正在生成/);
const eventFailure=conversation({...paired,events:[...paired.events,{type:'model_request',data:{request_id:'r3'}},{type:'model_error',data:{error:'流式读取失败'}}]});
assert.match(eventFailure,/输出中断/);
assert.match(eventFailure,/流式读取失败/);
assert.match(eventFailure,/实时回复/);
assert.doesNotMatch(eventFailure,/正在生成/);
const manual={run,requests:[{id:'manual'}],responses:[{request_id:'manual',response:{content:'',tool_calls:[{id:'manual-call',name:'exec',arguments:'{"command":"check"}'}]}}],messages:[{id:'manual-reply',role:'tool',tool_call_id:'manual-call',content:'{"error":"invalid arguments","exit_code":-1}'}],actions:[]};
assert.equal(callStatus(messageCallRecords(manual)[0].action),'failed');
assert.match(conversation(manual),/invalid arguments/);
const manualSuccess={...manual,messages:[{...manual.messages[0],content:'{"stdout":"已手工完成","exit_code":0}'}]};
assert.equal(callStatus(messageCallRecords(manualSuccess)[0].action),'completed');
assert.equal(callStatus(inspectionCalls({...manual,messages:[]})[0]),'waiting_tool');
for(const answer of [null,{value:'保留答案',evidence_ids:'ev-1'}]){
  const malformed=messageBody({id:'malformed',role:'assistant',content:JSON.stringify({trace_result:{summary:'形状异常摘要',answers:[answer]}})});
  assert.match(malformed,/形状异常摘要/);
  assert.match(malformed,/原始模型消息/);
  if(answer)assert.match(malformed,/保留答案/);
}
const oldReply={id:'old-reply',role:'assistant',content:'重复短回复',created_at:'2026-01-01T00:00:00Z'};
const repeated={run:{...run,status:'paused'},requests:[{id:'new-request',created_at:'2026-01-01T00:00:02Z',messages:[oldReply]}],responses:[{request_id:'new-request',response:{content:'重复短回复'}}],messages:[oldReply]};
assert.match(conversation(repeated),/id="live-new-request"/,'same text from a prior request does not hide a deferred latest response');
const repeatedError={...repeated,run:{...run,status:'failed'},responses:[{request_id:'new-request',response:{content:''},error:'新请求中断'}],events:[{type:'model_stream',data:{request_id:'new-request',delta:{kind:'text',text:'重复短回复'}}}]};
assert.match(conversation(repeatedError),/新请求中断/);
assert.match(conversation(repeatedError),/id="live-new-request"/);
assert.doesNotMatch(conversation({...repeated,messages:[oldReply,{id:'new-reply',role:'assistant',content:'重复短回复',created_at:'2026-01-01T00:00:03Z'}]}),/id="live-new-request"/,'actual latest message replaces its transient response');
const failed=toolCard({call_id:'bad',name:'exec',arguments:'{"command":"echo check"}',status:'completed',result:{stderr:'权限错误',exit_code:7}},snap);
assert.match(failed,/status failed/);
assert.match(failed,/echo check/);
assert.match(failed,/权限错误/);
assert.doesNotMatch(failed,/<details[^>]* open/,'tool cards default to collapsed');
console.log('Task workspace: grouped requests and calls, live fragments, errors, evidence/artifact links, inline task navigation.');

assert.doesNotMatch(task,/实时轨迹|trajectory-panel|workspace-tab/);
assert.match(task,/运行对话/);
const structuredDetail=recordDetail('action-detail',{call_id:'structured',name:'mcp',status:'completed',result:{items:[{title:'可读结构化输出'}]}},{run,actions:[],evidence:[]});
assert.match(structuredDetail,/可读结构化输出/);
assert.doesNotMatch(structuredDetail,/退出码 undefined/);
const streamed={run,requests:[{id:'pending'}],events:[{type:'model_stream',data:{request_id:'pending',delta:{kind:'tool_call',index:0,id:'pending-call',name:'exec',arguments:'{}'}}}]};
assert.equal(inspectionCalls(streamed)[0].call_id,'pending-call');
