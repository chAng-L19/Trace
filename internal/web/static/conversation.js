// Adapted presentation patterns: DeepSeek Harness @ 571902028a75c64c1d767f4ae5fd099485f0ddf0
// ui-chat/ReasoningRow.tsx, MessageItem.module.css; ui-tool/ToolRow.tsx, ToolCallTree.tsx (MIT).
import {esc,btn,status,empty,evidenceLinks,label,diagnosticText} from './ui.js';
import {markdown,readable,structured,messageBody,streamingBody,duration,rawDetails,parseContent,contentSummary} from './presentation.js';
import {attachmentRecords} from './attachments.js';
import {runLink} from './run-management.js';
import {pendingModelCalls,streamPreview,streamedAction} from './live.js';

export function callLinks(call,snap){
  const actions=(snap.actions||[]).filter(a=>a.call_id===call.call_id),matches=record=>record.call_id===call.call_id&&(record.request_id?record.request_id===call.request_id:actions.length<=1);
  return `<div class="buttons call-links">${snap.run?.id?btn('copy-link','调用链接',`data-href="${esc(runLink(snap.run.id,{request:call.request_id,call:call.call_id}))}"`):''}${btn('action-detail','调用详情',`data-id="${esc(call.call_id)}" data-request="${esc(call.request_id||'')}"`)}${evidenceLinks((snap.evidence||[]).filter(matches).map(e=>e.id))}${(call.result?.artifacts||[]).map(a=>`${btn('artifact','预览 · '+esc(label(a.kind)),`data-hash="${esc(a.hash)}"`)}<a href="/api/artifacts/${esc(a.hash)}/download" download>下载产物 ↓</a>`).join('')}</div>`;
}
// Historical FinishedAt can include output archival time. Only display saved
// diagnostics; run events alone do not establish a command interruption cause.
export const callDiagnostics=call=>diagnosticText(call.result?.diagnostic);
export function toolOutput(result){
  if(!result)return '<p class="note">等待工具结果…</p>';
  const extra=Object.fromEntries(Object.entries(result).filter(([key])=>!['stdout','stderr','error','exit_code','artifacts','diagnostic'].includes(key)));
  return `${diagnosticText(result.diagnostic)}${result.stdout?readable(result.stdout):''}${result.stderr?`<h4>标准错误</h4>${readable(result.stderr)}`:''}${result.error?`<p class="inline-error">${esc(result.error)}</p>`:''}${Object.keys(extra).length?structured(extra):!result.stdout&&!result.stderr&&!result.error&&!result.diagnostic?'<p class="note">工具已返回 · 无文本输出</p>':''}${result.exit_code!==undefined?`<p class="note">退出码 ${esc(result.exit_code)}</p>`:''}`;
}
export const callStatus=call=>['uncertain','interrupted'].includes(call.status)?call.status:call.result?.worker_status==='cancelled'?'interrupted':call.result?.error||(Number.isFinite(Number(call.result?.exit_code))&&Number(call.result.exit_code)!==0)?'failed':call.status;
const glyphs={terminal:'<rect x="3" y="4" width="18" height="16" rx="3"/><path d="m7 9 3 3-3 3m6 0h4"/>',edit:'<path d="m15 4 5 5M4 20l5-1L20 8a2 2 0 0 0-5-5L4 14z"/>',read:'<path d="M6 3h8l4 4v14H6zM14 3v5h4M9 12h6m-6 4h6"/>',search:'<circle cx="10" cy="10" r="6"/><path d="m15 15 6 6"/>',tool:'<path d="m8 5-6 7 6 7m8-14 6 7-6 7M14 3l-4 18"/>'};
const toolIcon=kind=>`<svg class="tool-glyph" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${glyphs[kind]||glyphs.tool}</svg>`;
function toolAction(call){
  const args=parseContent(call.arguments)||{},name=call.local_name||call.remote_name||call.name||'工具',key=[call.remote_name,call.local_name,call.name].filter(Boolean).join('/').toLowerCase();
  const finished=callStatus(call)==='completed',path=args.file_path||args.path||'';
  if(/(?:^|[_./-])(write|edit|patch|apply_patch)(?:$|[_./-])/.test(key))return {kind:'edit',title:finished?'编辑了文件':'编辑文件',detail:path};
  if(args.command||args.cmd||/(?:^|[_./-])(exec|shell|terminal|run_command)(?:$|[_./-])/.test(key))return {kind:'terminal',title:finished?'运行了命令':'运行命令',detail:args.description||args.command||args.cmd||''};
  if(/(?:^|[_./-])(read|read_file|read_skill)(?:$|[_./-])/.test(key))return {kind:'read',title:finished?'读取了文件':'读取文件',detail:path||args.name||''};
  if(args.query||/(?:search|grep|find)/.test(key))return {kind:'search',title:finished?'完成了搜索':'搜索',detail:args.query||args.pattern||''};
  return {kind:'tool',title:finished?'调用了工具':'调用工具',detail:name};
}
const callProgress=call=>call.progress?[call.progress.message,call.progress.progress!==undefined?String(call.progress.progress)+(call.progress.total!==undefined?' / '+call.progress.total:''):''].filter(Boolean).join(' · '):'';
const visibleStatus=call=>callStatus(call)==='completed'?'':status(callStatus(call));
export const callKey=call=>call.request_id?`${encodeURIComponent(call.request_id)}:${encodeURIComponent(call.call_id)}`:call.call_id;
export function messageCallRecords(snap){
  const messages=snap.messages||[],actions=snap.actions||[],usedActions=new Set(),usedResponses=new Set(),usedReplies=new Set(),records=[];
  for(const [index,message] of messages.entries()){
    if(message.role!=='assistant'||!message.tool_calls?.length)continue;
    const response=(snap.responses||[]).find(r=>!usedResponses.has(r)&&(message.request_id?r.request_id===message.request_id:message.tool_calls.every(call=>r.response?.tool_calls?.some(c=>c.id===call.id&&c.name===call.name&&c.arguments===call.arguments))));
    if(response)usedResponses.add(response);
    for(const call of message.tool_calls){
      let reply;
      for(let i=index+1;i<messages.length&&messages[i].role!=='assistant'&&messages[i].role!=='user';i++)if(messages[i].role==='tool'&&messages[i].tool_call_id===call.id&&!usedReplies.has(messages[i])){reply=messages[i];break}
      if(reply)usedReplies.add(reply);
      const requestID=message.request_id||response?.request_id;
      const source=actions.find(a=>a.call_id===call.id&&!usedActions.has(a)&&(!requestID||!a.request_id||a.request_id===requestID));
      if(source)usedActions.add(source);
      const parsed=reply&&parseContent(reply.content),action=source||{...call,call_id:call.id,request_id:requestID||message.id,status:reply?'completed':'waiting_tool',result:reply?(parsed&&typeof parsed==='object'?parsed:{stdout:reply.content}):undefined};
      records.push({message,reply,action,source});
    }
  }
  for(const message of messages.filter(m=>m.role==='tool'&&!usedReplies.has(m))){
    const response=(snap.responses||[]).find(r=>r.response?.tool_calls?.some(call=>call.id===message.tool_call_id)&&!records.some(record=>record.action.request_id===r.request_id&&record.action.call_id===message.tool_call_id));
    const owner=message.request_id||response?.request_id;
    const source=actions.find(a=>a.call_id===message.tool_call_id&&!usedActions.has(a)&&(!owner||!a.request_id||a.request_id===owner));
    if(source)usedActions.add(source);
    const call=response?.response.tool_calls.find(call=>call.id===message.tool_call_id),parsed=parseContent(message.content);
    const action=source||{...call,call_id:message.tool_call_id,request_id:owner||message.id,name:call?.name||'工具',status:'completed',result:parsed&&typeof parsed==='object'?parsed:{stdout:message.content}};
    records.push({message,reply:message,action,source});
  }
  return records;
}
// Detail views use the same identities as the conversation, including manual
// results and streamed calls that have not yet entered the action ledger.
export function inspectionCalls(snap){
  const calls=[...(snap.actions||[])];
  const add=call=>{if(!calls.some(a=>callKey(a)===callKey(call)))calls.push(call)};
  for(const record of messageCallRecords(snap))if(!record.source)add({...record.action,source_message:record.message,result_message:record.reply});
  for(const response of snap.responses||[])for(const call of response.response?.tool_calls||[])add({...call,call_id:call.id,request_id:response.request_id,status:'waiting_tool'});
  for(const request of snap.requests||[])if(!(snap.responses||[]).some(r=>r.request_id===request.id))for(const call of pendingModelCalls(snap,request.id))add(call);
  return calls;
}
function toolBody(call,snap,prefix,links){
  const progress=callProgress(call),name=call.local_name||call.remote_name||call.name||'工具';
  return `<p class="tool-meta">${esc(name)} · ${duration(call.started_at,call.finished_at)}</p><h4>输入</h4>${readable(call.arguments||'{}')}<h4>输出</h4>${progress?`<p class="note">${esc(progress)}</p>`:''}${!call.result&&call.status==='uncertain'?'<p class="note">执行结果未保存；需核对实际执行与清理状态，原调用不会自动重放。</p>':toolOutput(call.result)}${childRuns(call,snap)}${links??callLinks(call,snap)}${rawDetails(`${prefix}-raw-${callKey(call)}`,call,'原始调用与结果')}`;
}
export function toolDisclosureBody(element,snap){
  if(element.dataset.toolRun!==snap.run?.id)return null;
  const call=inspectionCalls(snap).find(call=>callKey(call)===element.dataset.toolKey);
  return call?toolBody(streamedAction(snap,call),snap,element.dataset.toolPrefix):null;
}
export function toolCard(call,snap,prefix='chat',links,lazy=false){
  call=streamedAction(snap,call);
  const meta=toolAction(call),progress=callProgress(call),id=prefix+'-call-'+callKey(call),open=!lazy||typeof document!=='undefined'&&document.getElementById?.(id)?.open;
  return `<details id="${esc(id)}" class="tool-disclosure"${lazy?` data-lazy-tool="true" data-tool-key="${esc(callKey(call))}" data-tool-run="${esc(snap.run?.id)}" data-tool-prefix="${prefix}"`:''}><summary>${toolIcon(meta.kind)}<span class="tool-title">${esc(meta.title)}</span><span class="tool-description">${esc(meta.detail||(!call.result&&call.arguments?'接收参数 · '+call.arguments.length+' 字符':''))}</span>${progress?`<span class="tool-progress">${esc(progress)}</span>`:''}${visibleStatus(call)}</summary><div class="tool-body" data-scroll="tool-${esc(callKey(call))}">${open?toolBody(call,snap,prefix,links):'<p class="note">展开后读取本条调用的输入、输出和来源。</p>'}</div></details>`;
}
function childRuns(call,snap){
  const result=parseContent(call.result?.stdout)||call.result;
  const rows=Array.isArray(result)?result:result?.workers||[result];
  const children=rows.filter(row=>row?.run_id&&(snap.workers||[]).some(worker=>worker.run_id===row.run_id));
  return children.map(row=>{const worker=snap.workers.find(w=>w.run_id===row.run_id),preview=snap.childPreviews?.[row.run_id];return `<section class="child-run"><p>子运行 · ${esc(worker.task_id)} ${status(worker.status)}</p><div class="buttons">${btn('child-preview',preview?'收起子运行':'展开子运行',`data-id="${esc(row.run_id)}" aria-expanded="${!!preview}"`)}${btn('select','进入子运行 ↗',`data-id="${esc(row.run_id)}"`)}</div>${preview?`<div class="child-preview" data-scroll="child-${esc(row.run_id)}">${preview.error?`<p class="inline-error">${esc(preview.error)}</p>`:(preview.messages||[]).filter(m=>['user','assistant'].includes(m.role)).slice(-6).map(m=>`<article class="message ${esc(m.role)}">${m.thinking?reasoning(m.thinking,'child-thinking-'+m.id):''}${messageBody(m)}</article>`).join('')||'<p class="note">子运行已建立，等待首条模型消息。</p>'}</div>`:''}</section>`}).join('');
}
function toolGroup(calls,snap,lazy){
  if(calls.length===1)return toolCard(calls[0],snap,'chat',undefined,lazy);
  // Keep the first call's disclosure identity when a single live call grows into a group.
  return `<details id="chat-call-${esc(callKey(calls[0]))}" class="tool-disclosure tool-group"><summary>${toolGroupSummary(calls,snap)}</summary><div class="tool-group-body">${calls.map(call=>toolCard(call,snap,'chat-item',undefined,lazy)).join('')}</div></details>`;
}
function toolGroupSummary(calls,snap,settled=new Map()){
  const live=calls.map(call=>streamedAction(snap,call)),titles=[...new Set(live.map(call=>(settled.get(call)||toolAction(call)).title))],active=live.find(call=>['started','running','waiting_tool'].includes(callStatus(call))),attention=live.find(call=>['failed','uncertain','interrupted','cancelled'].includes(callStatus(call))),progress=active&&callProgress(active);
  return `${toolIcon(toolAction(live[0]).kind)}<span class="tool-title">${esc(titles.join(' · '))}</span><span class="tool-count">${calls.length}</span>${progress?`<span class="tool-progress">${esc(progress)}</span>`:''}${attention?visibleStatus(attention):active?visibleStatus(active):''}`;
}
// Muteki Thinking.working and Harness ReasoningRow.running separate active
// reasoning from settled history. Keep this native disclosure manually reopenable.
export const reasoning=(text,id,active=false,title='思考')=>`<details id="${esc(id)}" class="reasoning-disclosure" data-reasoning-phase="${active?'active':'done'}"${active?' open':''}><summary>${esc(title)}</summary><div class="reasoning-content" data-scroll="${esc(id)}">${markdown(text)}</div></details>`;
// Only the mounted snapshot owns a preview plan. Semantic refreshes rebuild the
// transcript; SSE frames render its active calls and request, never old Markdown.
// Muteki ConversationStreamBindings.tsx isolates stream subscribers; Harness
// MessageItem/ReasoningRow memoize settled nodes and retain disclosure identity.
let previewPlan;
// Muteki Conversation folds settled process prose; final reports and live text
// remain readable. Only the presentation is shortened, never the saved message.
function processBody(message,final){
  const value=parseContent(message.content),text=message.content||'';
  if(message.role!=='assistant'||final||text.length<=520||value?.trace_result||value?.final_result)return messageBody(message);
  const summary=Array.from(contentSummary(text));
  return `<details id="process-message-${esc(message.id)}" class="process-message"><summary>${esc(summary.slice(0,140).join(''))}${summary.length>140?'…':''}</summary>${messageBody(message)}</details>`;
}
export function conversationPagination(page){
  return page?`<nav class="conversation-pages" aria-label="对话历史分页">${page.has_more_before?btn('messages-page','更早一页',`data-before="${esc(page.oldest)}"`):''}<span class="note">${page.total?page.start+1:0}–${page.end} / ${page.total} 条 · 已加载消息</span>${page.has_more_after?btn('messages-page','更新一页',`data-after="${esc(page.newest)}"`)+btn('messages-latest','返回实时尾部'):''}</nav>`:'';
}
export function conversation(snap,{lazy=false}={}){
  const messages=snap.messages||[],page=snap.messages_page,pageRequests=new Set(messages.map(m=>m.request_id).filter(Boolean)),actions=(snap.actions||[]).filter(a=>!page||pageRequests.has(a.request_id)||!snap.historyWindow&&!a.result),represented=new Set(),replies=new Set();
  const finalMessage=['completed','failed','cancelled'].includes(snap.run?.status)&&!page?.has_more_after?messages.findLast(m=>m.role==='assistant'):null;
  const calls=new Map();
  for(const record of messageCallRecords(snap)){
    if(!calls.has(record.message))calls.set(record.message,[]);
    calls.get(record.message).push(record);
  }
  const rows=[],groups=[];let pending=[];
  const flush=()=>{if(lazy)pending=pending.filter(call=>callStatus(call)!=='uncertain');if(pending.length){rows.push(toolGroup(pending,snap,lazy));const active=pending.filter(call=>!call.result);if(active.length)groups.push({calls:pending,active,links:new Map(active.map(call=>[call,callLinks(call,snap)])),settled:new Map(pending.filter(call=>call.result).map(call=>[call,toolAction(call)]))});pending=[]}};
  for(const m of messages){
    if(m.role==='tool'){
      if(replies.has(m))continue;
      const record=calls.get(m)?.[0];if(!record)continue;
      if(record.source)represented.add(record.source);pending.push(record.action);continue;
    }
    if(m.role==='user')flush();
    if(m.role==='system'){flush();rows.push(rawDetails('system-'+m.id,m.content,'系统上下文'));continue}
    if(m.content||m.thinking||m.attachments?.length||m.preview){
      flush();
      const request=m.request_id;
      rows.push(`<article class="message ${esc(m.role)}" data-message-id="${esc(m.id||'')}" id="${request?'live-'+esc(request):'message-'+esc(m.id)}" aria-label="${m.role==='user'?'用户消息':'助手消息'}">${m.thinking?reasoning(m.thinking,request?'live-thinking-'+request:'thinking-'+m.id):''}${m.content?processBody(m,m===finalMessage):''}${m.preview?`<p class="note">当前为有界预览 · 正文 ${m.content_bytes} 字节 / 思考 ${m.thinking_bytes} 字节 ${btn('message-detail','读取完整消息',`data-message="${esc(m.id)}"`)}</p>`:''}${attachmentRecords(m.attachments)}${m.id?`<div class="message-actions">${btn('copy-message','复制正文',`data-message="${esc(m.id)}"`)}${btn('copy-link','定位链接',`data-href="${esc('#history/'+encodeURIComponent(snap.run.id)+'?message='+encodeURIComponent(m.id))}"`)}${btn('branch','从此分支',`data-message="${esc(m.id)}" aria-label="从此消息创建分支"`)}</div>`:''}</article>`);
    }
    for(const record of calls.get(m)||[]){
      if(record.reply)replies.add(record.reply);
      if(record.source)represented.add(record.source);
      pending.push(record.action);
    }
  }
  pending.push(...actions.filter(a=>!represented.has(a)).slice(0,page?80:undefined));flush();
  const tail=tailContext(snap),live=snap.historyWindow?'':conversationTail(snap,tail,lazy),elapsed=snap.run?.elapsed_millis,overview=typeof elapsed==='number'&&Number.isFinite(elapsed)&&elapsed>=0?`<details id="conversation-work-summary" class="conversation-work-summary"><summary>用时 ${(elapsed/1000).toFixed(1)} 秒</summary><p>已结算运行用时 ${(elapsed/1000).toFixed(1)} 秒</p><p>${snap.totals?.requests??(snap.requests||[]).length} 次模型请求</p><p class="note">用时取自运行账本；当前尚未结算的执行区间未计入。</p></details>`:'';
  previewPlan={snap,groups,tail,lazy,hasRows:!!rows.length,messages:snap.messages,messageCount:messages.length,requests:snap.requests,requestCount:snap.requests?.length||0,responses:snap.responses,responseCount:snap.responses?.length||0,actions:snap.actions,actionCount:snap.actions?.length||0};

  return overview+rows.join('')+'<template id="conversation-live-start"></template>'+(live||(!rows.length?empty('等待第一条消息'):''))+'<template id="conversation-live-end"></template>';
}
export function conversationPreview(snap,{lazy=false}={}){
  if(snap.historyWindow)return {groups:[],tail:''};
  const p=previewPlan;
  if(!p||p.snap!==snap||p.messages!==snap.messages||p.messageCount!==(snap.messages?.length||0)||p.requests!==snap.requests||p.requestCount!==(snap.requests?.length||0)||p.responses!==snap.responses||p.responseCount!==(snap.responses?.length||0)||p.actions!==snap.actions||p.actionCount!==(snap.actions?.length||0))return {full:conversation(snap,{lazy})};
  return {groups:p.groups.map(({calls,active,links,settled})=>({id:'chat-call-'+callKey(calls[0]),summary:calls.length>1?toolGroupSummary(calls,snap,settled):undefined,calls:active.map(call=>({id:(calls.length>1?'chat-item-call-':'chat-call-')+callKey(call),html:toolCard(call,snap,calls.length>1?'chat-item':'chat',links.get(call),p.lazy)}))})),tail:conversationTail(snap,p.tail,p.lazy)||(!p.hasRows?empty('等待第一条消息'):'')};
}
function tailContext(snap){
  const messages=snap.messages||[],actions=snap.actions||[],preview=streamPreview(snap),latest=preview.latest,response=(snap.responses||[]).find(r=>r.request_id===latest?.id),body=response?.response;
  const interrupted=!!(response?.error||preview.error)||['failed','cancelled','paused'].includes(snap.run?.status);
  const text=body?.content||((interrupted||!response)?preview.text:''),thinking=body?.thinking||((interrupted||!response)?preview.thinking:'');
  const historyIDs=new Set((latest?.messages||[]).map(m=>m.id).filter(Boolean)),boundary=messages.reduce((last,m,i)=>historyIDs.has(m.id)?i:last,-1),requestedAt=Date.parse(latest?.created_at);
  const archived=!!latest&&messages.some((m,i)=>{
    if(m.role!=='assistant')return false;
    // Explicit ownership survives compact snapshots and final-frame overlap.
    // Legacy content/time matching must never override a different request ID.
    if(m.request_id)return m.request_id===latest.id;
    if(!body)return false;
    const afterRequest=boundary>=0?i>boundary:Number.isFinite(requestedAt)&&Date.parse(m.created_at)>=requestedAt;
    if(afterRequest&&!historyIDs.has(m.id)&&body.tool_calls?.length&&body.tool_calls.every(call=>m.tool_calls?.some(c=>c.id===call.id)))return true;
    return afterRequest&&!historyIDs.has(m.id)&&((text&&m.content===text)||(!text&&thinking&&m.thinking===thinking));
  });
  return {latest,response,body,archived,links:new Map(),actionCalls:new Set(actions.filter(a=>a.request_id===latest?.id).map(a=>a.call_id))};
}
function conversationTail(snap,{latest,response,body,archived,actionCalls,links},lazy=false){
  const preview=streamPreview(snap),error=response?.error||preview.error,phase=({failed:'输出中断',cancelled:'已取消',paused:'已暂停'})[snap.run?.status]||(error?'输出中断':response?'已返回':'正在生成');
  const interrupted=!!error||['failed','cancelled','paused'].includes(snap.run?.status);
  const text=body?.content||((interrupted||!response)?preview.text:''),thinking=body?.thinking||((interrupted||!response)?preview.thinking:'');
  const calls=[...preview.calls.values()],pendingCalls=latest&&!response?calls.filter(c=>!actionCalls.has(c.call_id)):[];
  const thinkingActive=!response&&!interrupted&&['running','waiting_tool'].includes(snap.run?.status)&&!text&&!calls.length&&!actionCalls.size;
  const pendingKeys=new Set(pendingCalls.map(callKey));for(const key of links.keys())if(!pendingKeys.has(key))links.delete(key);
  for(const call of pendingCalls)if(!links.has(callKey(call)))links.set(callKey(call),callLinks(call,snap));
  const live=latest&&!archived&&(text||thinking||error||pendingCalls.length)?`<article class="message assistant live-message" id="live-${esc(latest.id)}" aria-label="助手消息">${error?`<p class="inline-error">${esc(error)}</p>`:''}${thinking?reasoning(thinking,'live-thinking-'+latest.id,thinkingActive):''}${text?streamingBody(text,latest.id):''}<p class="message-phase">${phase}</p></article>${pendingCalls.map(call=>toolCard(call,snap,'chat',links.get(callKey(call)),lazy)).join('')}`:'';
  return live;
}
