import {esc,btn,status,when,short,empty,diagnosticText} from './ui.js';

const kinds={help:'求助',report:'报告',reply:'回复',resolved:'已解决'};
const phases={model:'模型请求在途',tool:'工具执行',running:'运行中',waiting_tool:'等待工具',paused:'已暂停',completed:'已结束',failed:'运行失败',cancelled:'已取消',created:'等待开始',unavailable:'状态来源不可用'};
const timestamp=value=>value&&!value.startsWith('0001')?when(value):'尚无记录';
const brief=(text,limit=24)=>{const chars=Array.from(String(text||'').replace(/\s+/g,' ').trim());return chars.length>limit?chars.slice(0,limit).join('')+'…':chars.join('')};
// Presentation only: occupancy remains a scheduler decision.
export function workerGroups(snap){
  const active=[],attention=[],history=[];
  for(const w of snap.workers||[]){
    if(['starting','running','cancelling'].includes(w.status))active.push(w);
    else if(['paused','uncertain','failed'].includes(w.status)||!['completed','not_required'].includes(w.cleanup_status))attention.push(w);
    else history.push(w);
  }
  return {active,attention,history};
}
export function pendingDispatch(snap){return (snap.plan?.tasks||[]).filter(t=>t.status==='running'&&!(snap.workers||[]).some(w=>w.task_id===t.id&&(!t.worker_id||w.id===t.worker_id)&&(!t.child_run_id||w.run_id===t.child_run_id)))}

export function workerActivity(row){
  if(!row)return '<p class="note">状态尚未读取；进入子运行查看。</p>';
  const calls=(row.calls||[]).slice(0,20).map(call=>'<li><code>'+esc(call.call_id||'未记录')+'</code> · '+esc(call.name||'工具')+' · '+esc(labelCall(call.status))+(call.worker_status?' · 进程 '+esc(call.worker_status):'')+(call.timeout_seconds?' · 单工具上限 '+esc(call.timeout_seconds)+' 秒':'')+(call.outcome_uncertain?' · 结果不确定':'')+(call.cleanup_error?' · 清理未确认：'+esc(call.cleanup_error):'')+diagnosticText(call.diagnostic)+'</li>').join('');
  return '<p class="note">'+esc(phases[row.phase]||row.phase)+(row.tools?.length?' · '+row.tools.map(esc).join(' / '):'')+' · 清理 '+status(row.cleanup_status)+'</p><p class="note">最近模型返回 '+timestamp(row.last_model_response)+' · 最近工具结算 '+timestamp(row.last_tool_result)+'</p><p class="note">本 Worker 显式预算 '+(row.budget_remaining_seconds==null?(row.available?'不限':'未知'):row.budget_remaining_seconds+' 秒')+' · 待投递 '+(row.pending_messages||0)+' · 未解决求助 '+(row.open_help_total??(row.open_help_ids||[]).length)+'</p>'+(calls?'<details><summary>当前调用 / 最近结算 · '+esc(row.call_total??row.calls.length)+'</summary><ul>'+calls+'</ul></details>':'');
}

const labelCall=value=>({started:'已开始',running:'运行中',result_ready:'结果待提交',completed:'已结算',uncertain:'待确认',interrupted:'已中断'})[value]||value||'未记录';

const dispatchCards=snap=>pendingDispatch(snap).map(t=>'<article class="planner-card" data-dispatch-task="'+esc(t.id)+'"><p title="'+esc(t.goal||t.id)+'">'+esc(brief(t.id))+'</p><p class="note">派发中 · 等待注册</p>'+btn('task-detail','任务详情','data-id="'+esc(t.id)+'"')+'</article>').join('');
export function workerSummary(snap){
 const g=workerGroups(snap),dispatch=dispatchCards(snap);
 if(!g.active.length&&!g.attention.length&&!g.history.length&&!dispatch)return '';
 const cards=list=>list.map(w=>'<details class="subsection worker-disclosure" id="workspace-worker-'+esc(w.id)+'"><summary><span data-worker-title title="'+esc((snap.plan?.tasks||[]).find(t=>t.id===w.task_id)?.goal||w.id)+'">'+esc(brief(w.task_id||short(w.id)))+'</span> '+status(w.status)+'</summary>'+workerActivity((snap.worker_activity||[]).find(r=>r.worker_id===w.id))+btn('select','查看子运行 ↗','data-id="'+esc(w.run_id)+'"')+'</details>').join('');
 return '<details id="workspace-workers" data-worker-activity class="run-live-status"><summary>子 Worker · '+(snap.workers||[]).length+(g.active.length?' · 运行 '+g.active.length:'')+(g.attention.length?' · 需核对 '+g.attention.length:'')+'</summary>'+cards(g.active)+dispatch+(g.attention.length?'<section><h3>已暂停 / 需核对</h3>'+cards(g.attention)+'</section>':'')+(g.history.length?'<details id="workspace-worker-history"><summary>已结束 · '+g.history.length+'</summary>'+cards(g.history)+'</details>':'')+'<p class="note">只读快照；无输出不代表停滞。'+btn('task-tab','全部协作 / 历史','data-tab="planner"')+btn('worker-refresh','刷新状态')+'</p></details>';
}

export function workerMessageInput(form){
  const value=name=>form.querySelector('[name="'+name+'"]')?.value||'';
  const body={content:value('content')};
  if(value('kind'))body.kind=value('kind');
  if(value('reply_to'))body.reply_to=value('reply_to');
  return body;
}

function messageForm(worker,help){
  const suffix=help?help.id:worker.id;
  const kind=help?'<option value="reply">回复求助</option><option value="resolved">标记已解决</option>':'<option value="">过程消息</option><option value="help">求助</option><option value="report">报告</option>';
  return '<form id="worker-form-'+esc(suffix)+'" data-worker="'+esc(worker.id)+'" class="subsection"><div class="form-row"><label for="worker-kind-'+esc(suffix)+'">消息类型<select name="kind" id="worker-kind-'+esc(suffix)+'">'+kind+'</select></label>'+(help?'<input name="reply_to" type="hidden" value="'+esc(help.id)+'">':'')+'<label for="worker-message-'+esc(suffix)+'">'+(help?'回复 / 解决说明':'过程消息')+'<input name="content" id="worker-message-'+esc(suffix)+'" required placeholder="'+(help?'说明决策或阻塞如何解决':'求助请写明尝试、阻塞与所需决策')+'"></label><button>发送</button></div><p class="note">'+(help?'已投递、已回复与已解决分别记录；标记解决不完成任务、不自动恢复 Worker。':'消息在工具批次间投递；暂停的 Worker 可保留消息等待恢复。')+'</p></form>';
}

export function workerPanel(snap){
  const workers=snap.workers||[],messages=snap.worker_messages||[];
  // Index each relation once; the unordered participant pair retains the old
  // same/reverse-direction match without delimiter collisions in identifiers.
  const relationKey=(id,from,to)=>JSON.stringify(from<=to?[id,from,to]:[id,to,from]);
  const relations=new Map(),workerByID=new Map();
  for(const w of workers)if(!workerByID.has(w.id))workerByID.set(w.id,w);
  for(const m of messages){
    if(m.kind!=='reply'&&m.kind!=='resolved')continue;
    const key=relationKey(m.reply_to,m.from,m.to);
    let related=relations.get(key);
    if(!related){related={replyCount:0,resolution:null};relations.set(key,related)}
    if(m.kind==='reply')related.replyCount++;
    else if(!related.resolution)related.resolution=m;
  }
  const card=w=>'<details class="planner-card worker-card" id="worker-card-'+esc(w.id)+'"><summary><span data-worker-title title="'+esc((snap.plan?.tasks||[]).find(t=>t.id===w.task_id)?.goal||w.id)+'">'+esc(brief(w.task_id||short(w.id)))+'</span>'+status(w.status)+'</summary><p>任务 '+esc(w.task_id)+'</p>'+workerActivity((snap.worker_activity||[]).find(r=>r.worker_id===w.id))+'<p class="note">开始 '+when(w.started_at)+'</p>'+(w.error?'<p class="inline-error">'+esc(w.error)+'</p>':'')+'<div class="buttons">'+btn('select','查看子运行 ↗','data-id="'+esc(w.run_id)+'"')+btn('worker-cancel','取消','data-worker="'+esc(w.id)+'" '+(w.status==='running'?'':'disabled'))+'</div>'+messageForm(w)+'</details>';
  const groups=workerGroups(snap),cards=groups.active.map(card).join('')+dispatchCards(snap)+(groups.attention.length?'<section><h3>已暂停 / 需核对</h3>'+groups.attention.map(card).join('')+'</section>':'')+(groups.history.length?'<details id="worker-history"><summary>历史 Worker · '+groups.history.length+'</summary>'+groups.history.map(card).join('')+'</details>':'');
  const log=messages.map(m=>{
    const related=m.kind==='help'?relations.get(relationKey(m.id,m.from,m.to)):null,replyCount=related?.replyCount||0,resolution=related?.resolution,peer=m.from==='parent'?m.to:m.to==='parent'?m.from:'',worker=workerByID.get(peer);
    return '<article class="message" id="worker-msg-'+esc(m.id)+'"><header>'+esc(short(m.from))+' → '+esc(short(m.to))+' · '+esc(kinds[m.kind]||'过程消息')+' · '+(m.delivered?'已投递':'待投递')+'</header><details class="process-message" id="worker-process-message-'+esc(m.id)+'"><summary>'+esc(brief(m.content,140)||'空消息')+'</summary><p class="note mono">'+esc(m.id)+(m.reply_to?' · 关联 '+esc(m.reply_to):'')+'</p><pre>'+esc(m.content)+'</pre></details>'+(m.kind==='help'?'<p class="note">'+(replyCount?'已有 '+replyCount+' 条回复':'尚无回复')+' · '+(resolution?'已标记解决':'尚未解决')+(resolution?' · '+esc(resolution.id):'')+'</p>'+(!resolution&&worker?messageForm(worker,m):''):'')+'</article>';
  }).join('');
  return '<div class="section-label">协作 Worker <span>INDEPENDENT RUNS</span></div>'+(cards||empty('暂无 Worker'))+'<div class="section-label subsection">过程消息 <span>投递 / 回复 / 解决</span></div>'+(log||'<p class="note">Worker 过程消息在此保留。</p>');
}
