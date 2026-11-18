export const esc=value=>String(value??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
export const json=value=>esc(JSON.stringify(value,null,2));
// A missing field is unknown (notably Agent-owned children); keep the durable status guard.
export const runActive=run=>run.execution_active??['running','waiting_tool'].includes(run.status);
export const diagnosticText=diagnostic=>diagnostic&&typeof diagnostic==='object'?`${diagnostic.summary?`<p class="note">诊断：${esc(diagnostic.summary)}</p>`:''}${diagnostic.next_action?`<p class="note">下一步：${esc(diagnostic.next_action)}</p>`:''}`:'';
export const label=value=>({starting:'启动中',cancelling:'取消中',blocked_capability:'能力受阻',available:'来源可读取',unavailable:'来源不可用',historical:'历史来源',changed:'来源已变化',created:'待开始',started:'执行中',running:'运行中',waiting_tool:'等待工具结果',paused:'已暂停',cancelled:'已取消',completed:'已完成',failed:'失败',pending:'待调度',claimed:'已认领',verified:'已验证',unverified:'未验证',success:'成功',incomplete:'未完成',uncertain:'待核对',negative:'阴性',discovered:'已发现',in_progress:'进行中',observed:'已观察',excluded:'范围外',included:'范围内',unknown:'未知',not_required:'无需清理',superseded:'已替换',reason:'分析',explore:'执行探索',user:'用户',assistant:'助手',tool:'工具',system:'系统',steer:'运行调整',follow_up:'后续指令',execution:'执行记录',tool_execution:'工具执行',manual_observation:'手工观测',resolved_manually:'已手工处理',host:'主机',port:'端口',domain:'域名',url:'网址',endpoint:'接口',service:'服务',file:'文件',stdout:'标准输出',stderr:'标准错误'})[value]||value||'—';
export const pauseReason=value=>value==='tool outcomes reviewed; awaiting operator resume'?'核对记录已保存，等待手动恢复运行。':value==='tool outcome uncertain; inspect side effects and submit its result manually'?'工具结果尚待核对；请打开调用详情查看具体诊断、原始输出和清理状态，记录实际结果后再决定是否恢复。':value==='operator_pause'?'用户已暂停运行。':value==='parent_pause'?'父会话暂停，当前 Worker 已随之暂停。':String(value||'').startsWith('worker_yield:')?String(value).split('; checkpoint:')[0]:value;
export const status=value=>`<span class="status ${esc(value)}" title="${esc(value)}">${esc(value==='invalid'?'证据失效':value==='interrupted'?'已中断':label(value))}</span>`;
export const when=value=>value?new Date(value).toLocaleString('zh-CN',{hour12:false}):'—';
export const short=value=>String(value||'').slice(0,12);
export const empty=(title,text='')=>`<div class="empty"><strong>${esc(title)}</strong>${esc(text)}</div>`;
export const field=(id,label,value='',required=false)=>`<label>${esc(label)}<input id="${id}" value="${esc(value)}" ${required?'required':''}></label>`;
export const btn=(action,label,attrs='')=>`<button type="button" class="small" data-action="${action}" ${attrs}>${label}</button>`;
export const evidenceLinks=(ids=[])=>ids.map(id=>btn('evidence-detail',esc(short(id)),`data-id="${esc(id)}"`)).join(' ');
// Mirror planner.go occupiedWorkerSlots only when authoritative totals are absent.
export const occupiedWorkerCount=snap=>{
  if(Number.isInteger(snap.totals?.workers_occupying))return snap.totals.workers_occupying;
  const workers=snap.workers||[];
  return workers.filter(w=>w.status==='running'||w.cleanup_status!=='completed').length+(snap.plan?.tasks||[]).filter(t=>['running','uncertain'].includes(t.status)&&!workers.some(w=>w.id===t.worker_id&&w.task_id===t.id&&w.run_id===t.child_run_id&&(t.status==='uncertain'||w.status==='running'||w.cleanup_status!=='completed'))).length;
};
