import {esc,btn,json,label} from './ui.js';
import {readRequirements} from './verification.js';
import {attachmentRecords} from './attachments.js';

// Interaction references, independently adapted to Trace's Store contracts:
// Muteki 144df766 (AGPL-3.0), lib/useRun.ts patchRun, useConversation.ts search,
// sendIntentStore.ts: retain logical send identity until receipt is reconciled.
export function creationOptions(){return `<label>完成条件<textarea id="new-completion" placeholder="用自然语言说明怎样算完成；不会当作输出精确匹配"></textarea></label><details id="creation-advanced"><summary>高级：名称、精确谓词与预算</summary><label>会话名称（可选）<input id="new-title" maxlength="200" placeholder="留空使用研究目标"></label><label>成功谓词：执行输出包含（可选）<input id="new-success" placeholder="必须在真实工具执行输出中出现的文本"></label><details><summary>此运行预算</summary><label class="checkbox-label"><input id="new-budget-enabled" type="checkbox">为此运行设置独立预算</label><p class="note">未勾选时继承默认配置；勾选后 0 表示不限。父、子运行分别计数。</p>${[['max_model_requests','模型请求'],['max_actions','工具动作'],['max_seconds','累计运行秒数']].map(([key,title])=>`<label>${title}<input id="new-budget-${key}" name="${key}" type="number" min="0" step="1" value="0" required></label>`).join('')}</details></details>`}
export function readCreationOptions(form,defaults={}){
  const keys=['max_model_requests','max_actions','max_seconds'],values=keys.map(k=>form.elements[k].value);
  const budget=form.querySelector('#new-budget-enabled').checked?Object.fromEntries(keys.map((k,i)=>[k,values[i]===''?(defaults[k]??0):Number(values[i])])):undefined;
  return {completion_criteria:form.querySelector('#new-completion')?.value||'',title:form.querySelector('#new-title').value,success_contains:form.querySelector('#new-success').value,budget};
}
export function requirementsForm(snap){
  const locked=['running','waiting_tool'].includes(snap.run.status)||!!(snap.requests?.length||snap.actions?.length||snap.run.usage?.model_requests||snap.run.usage?.tool_actions),disabled=locked?'disabled':'';
  return `<p class="note">成功谓词与验收条件在首次执行前可修改。执行后保留原判据；新目标请创建分支。来源完整、运行结束和业务成功分别记录。</p><form id="requirements-form" data-run="${esc(snap.run.id)}"><label>执行输出包含<input id="edit-success" name="success_contains" value="${esc(snap.run.success_contains||'')}" ${disabled}></label><label>验收条件 JSON 数组<textarea id="edit-requirements" name="requirements" spellcheck="false" ${disabled}>${json(snap.requirements||[])}</textarea></label><button ${disabled}>保存成功条件</button>${locked?'<p class="note">此运行已有执行记录，条件已锁定。</p>':''}</form>`;
}
export const hasTimestamp=value=>!!value&&!String(value).startsWith('0001-');
export const pendingMessages=snap=>(snap.mailbox||[]).filter(m=>!m.message_id&&!hasTimestamp(m.consumed_at)&&!hasTimestamp(m.cancelled_at));
export function moveQueuedMessage(snap,id,direction){const pending=pendingMessages(snap),index=pending.findIndex(m=>m.id===id);if(index<0)return null;let next=index+direction;while(next>=0&&next<pending.length&&pending[next].kind!==pending[index].kind)next+=direction;if(next<0||next>=pending.length)return null;[pending[index],pending[next]]=[pending[next],pending[index]];return pending.map(m=>m.id)}
export function mailboxPanel(snap){
  const pending=pendingMessages(snap);
  return `<details id="run-mailbox"><summary>指令队列 · ${pending.length} 待投递</summary>${btn('message-deliveries','核对投递与归档回执',`data-run="${esc(snap.run?.id)}"`)}<p class="note">调整指令在安全边界投递，后续指令在当前回合后投递；排序只在同一投递类别内生效。</p>${(snap.mailbox||[]).map(m=>{const index=pending.indexOf(m);return `<article class="queue-message"><p class="note"><b>${esc(label(m.kind))}</b> · ${hasTimestamp(m.cancelled_at)?'已取消':m.message_id||hasTimestamp(m.consumed_at)?'已投递':'待投递'}<br>${esc(m.content)}</p>${attachmentRecords(m.attachments)}${index>=0?`<div class="buttons">${btn('queue-edit','编辑',`data-id="${esc(m.id)}"`)}${btn('queue-cancel','取消投递',`data-id="${esc(m.id)}"`)}${btn('queue-move','↑',`data-id="${esc(m.id)}" data-direction="-1" aria-label="指令上移" ${!pending.slice(0,index).some(other=>other.kind===m.kind)?'disabled':''}`)}${btn('queue-move','↓',`data-id="${esc(m.id)}" data-direction="1" aria-label="指令下移" ${!pending.slice(index+1).some(other=>other.kind===m.kind)?'disabled':''}`)}</div>`:''}</article>`}).join('')||'<p class="note">暂无排队指令</p>'}</details>`;
}
export function archiveSearch(s){return `<details id="archive-body-search"><summary>跨会话正文检索</summary><form id="body-search-form" class="form-row"><label>搜索消息正文<input id="body-search-query" name="query" type="search" value="${esc(s.bodyQuery||'')}" required></label><label class="checkbox-label"><input id="body-search-archived" name="archived" type="checkbox" ${s.bodyIncludeArchived?'checked':''}>包含已归档</label><button>检索</button></form>${s.bodySearchError?`<p role="alert" class="inline-error">${esc(s.bodySearchError)}</p>`:''}${s.bodySearch?`<p class="note" role="status">${s.bodySearch.matches.length} 条匹配${s.bodySearch.truncated?' · 结果已截断，请缩小关键词':''}</p>${s.bodySearch.matches.map(m=>`<article class="search-hit"><a href="${esc(runLink(m.run_id,{message:m.message_id,request:m.request_id,call:m.call_id,tab:m.call_id?'activity':undefined}))}">${esc(m.title||m.goal||m.run_id)} · ${esc(m.role||'会话')}${m.archived?' · 已归档':''}</a><pre>${esc(m.content)}</pre></article>`).join('')}`:''}</details>`}
export function runLink(id,{tab,message,request,call,event,chain,chain_run,chain_hash}={}){const params=new URLSearchParams();for(const [k,v]of Object.entries({tab,message,request,call,event,chain,chain_run,chain_hash}))if(v!==undefined&&v!=='')params.set(k,v);return '#history/'+encodeURIComponent(id)+(params.size?'?'+params:'')}
export function parseRunRoute(route){
  const [id,query='']=route.slice(8).split('?');let runID;try{runID=decodeURIComponent(id)}catch{return {page:'history',historyDetail:false}}
  const params=new URLSearchParams(query),tabs=['activity','assets','evidence','artifacts','result','planner','events'];
  const link=Object.fromEntries(['message','request','call','event','chain','chain_run','chain_hash'].filter(k=>!k.startsWith('chain')||params.has(k)).map(k=>[k,params.get(k)||'']));
  if(tabs.includes(params.get('tab')))link.tab=params.get('tab');
  return {page:'history',historyDetail:true,runID,deepLink:link};
}
export function createRunManagement({state,api,showDetail,refresh,refreshRun,render,notice,captureNotice=()=>notice}){
  const sends=new Map(),receipts=new Map();let searchVersion=0;
  const storageKey=id=>'trace.operation:'+id;
  function stored(id){return sessionStorage.getItem(storageKey(id))}
  function clear(id){sends.delete(id);sessionStorage.removeItem(storageKey(id))}
  async function reconcile(id){const operation=stored(id);if(!operation)return null;try{const ack=await api(`/runs/${id}/operations/${encodeURIComponent(operation)}`);const send=sends.get(id);if(send)receipts.set(id,{signature:send.signature,ack});clear(id);return ack}catch(error){if(error.status===404)return null;throw error}}
  async function send(id,kind,payload){
    const signature=JSON.stringify({kind,...payload}),old=sends.get(id);
    const receipt=receipts.get(id);if(receipt){receipts.delete(id);if(receipt.signature===signature)return receipt.ack}
    if(stored(id)){const ack=await reconcile(id);if(ack){receipts.delete(id);if(!old||old.signature!==signature)throw new Error('上条指令已确认受理；请再次提交当前新指令');return ack}if(old&&old.signature!==signature)throw new Error('上条指令尚待确认，请恢复原文重试，或先核对提交状态');}
    let send=sends.get(id);
    if(!send){const bytes=new Uint8Array(16);crypto.getRandomValues(bytes);send={operation:stored(id)||[...bytes].map(b=>b.toString(16).padStart(2,'0')).join(''),signature,kind,payload};sends.set(id,send);sessionStorage.setItem(storageKey(id),send.operation)}
    try{const ack=await api(`/runs/${id}/${send.kind}`,'POST',{...send.payload,operation_id:send.operation});clear(id);return ack}catch(error){
      if(error.status>=400&&error.status<500){clear(id);throw error}
      // A lost response is reconciled against the durable operation, never a new send.
      try{const ack=await reconcile(id);if(ack){receipts.delete(id);return ack}}catch{}
      throw new Error(error.message+'；未确认受理时，重试将沿用同一操作身份');
    }
  }
  async function dispatch(action,el){const notice=captureNotice();
    const id=el.dataset.run||state.selected,r=state.runs.find(r=>r.id===id)||state.snap?.run;
    if(action==='requirements-edit'){showDetail('成功条件',requirementsForm(state.snap));return true}
    if(action==='session-rename'){showDetail('会话名称',`<form id="session-name-form" data-run="${esc(id)}"><label>名称<input id="session-title" name="title" maxlength="200" value="${esc(r?.title||'')}" placeholder="留空使用研究目标"></label><button>保存名称</button></form>`);return true}
    if(action==='session-archive'){await api('/runs/'+id,'PATCH',{archived:el.dataset.archived!=='true'});await refresh();notice(el.dataset.archived==='true'?'会话已恢复到档案列表；执行状态不变':'会话已归档；执行状态不变');return true}
    if(action==='archive-archived'){state.historyArchived=!state.historyArchived;state.historyHidden=false;state.historyDetail=false;render();return true}
    if(action==='operation-check'){const ack=await reconcile(id);notice(ack?'提交状态：'+label(ack.status):'服务端尚无已受理记录；当前草稿可重试');await refreshRun(id);return true}
    if(action.startsWith('queue-')){
      const m=(state.snap.mailbox||[]).find(m=>m.id===el.dataset.id);if(!m)throw new Error('队列记录已变化，请刷新');
      if(action==='queue-edit'){showDetail('编辑待投递指令',`<form id="queue-edit-form" data-run="${esc(id)}" data-id="${esc(m.id)}" data-revision="${m.revision||0}"><label>指令正文<textarea id="queue-content" name="content" required>${esc(m.content)}</textarea></label><p class="note">已有关联附件保留；投递或并发编辑后，保存会提示冲突。</p><button>保存指令</button></form>`);return true}
      if(action==='queue-cancel'){try{await api(`/runs/${id}/mailbox/${encodeURIComponent(m.id)}`,'DELETE',{revision:m.revision||0})}catch(error){await refreshRun(id);throw error}}
      if(action==='queue-move'){const ids=moveQueuedMessage(state.snap,m.id,Number(el.dataset.direction));if(!ids)return true;try{await api(`/runs/${id}/mailbox/order`,'PUT',{message_ids:ids})}catch(error){await refreshRun(id);throw error}}
      await refreshRun(id);notice('指令队列已更新');return true;
    }
    return false;
  }
  async function submit(form){const notice=captureNotice();
    const id=form.dataset.run||state.selected;
    if(form.id==='requirements-form'){await api(`/runs/${id}/requirements`,'PUT',{requirements:readRequirements(form.elements.requirements),success_contains:form.elements.success_contains.value});await refreshRun(id);notice('成功条件已保存');return true}
    if(form.id==='session-name-form'){await api('/runs/'+id,'PATCH',{title:form.elements.title.value});await refresh();notice('会话名称已保存');return true}
    if(form.id==='queue-edit-form'){try{await api(`/runs/${id}/mailbox/${encodeURIComponent(form.dataset.id)}`,'PATCH',{content:form.elements.content.value,revision:Number(form.dataset.revision)})}catch(error){await refreshRun(id);throw error}await refreshRun(id);notice('指令已更新');return true}
    if(form.id==='body-search-form'){const version=++searchVersion;state.bodyQuery=form.elements.query.value;state.bodyIncludeArchived=form.elements.archived.checked;state.bodySearchError='';try{const result=await api('/runs/search?'+new URLSearchParams({q:state.bodyQuery,include_archived:String(state.bodyIncludeArchived),limit:'50'}));if(version===searchVersion)state.bodySearch=result}catch(error){if(version===searchVersion)state.bodySearchError=error.message;throw error}finally{render()}return true}
    return false;
  }
  return {dispatch,submit,send,reconcile};
}
