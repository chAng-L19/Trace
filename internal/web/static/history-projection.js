import {esc,btn} from './ui.js';
import {eventLabel} from './event-labels.js';

// Graph interaction references: LuaN1aoAgentv2 a035429, GraphView.tsx;
// Cairn 8e7e0ea, cairn/server/static/index.html snapshotNodePositions (AGPL-3.0).
// Replay is a read-only projection of Trace events, never a Runtime rollback.
const graphEvent=/^(run_|plan_(updated|failed)$|task_|asset_|relation_|requirements_set$|evidence_checked$|tool_result$|finding_)/;
export const replayEvents=snap=>(snap.events||[]).filter(e=>graphEvent.test(e.type)).sort((a,b)=>a.sequence-b.sequence);
export function projectHistory(snap,sequence){
  const out={run:{id:snap.run.id,goal:snap.run.goal,status:'created'},plan:{tasks:[]},assets:[],relations:[],requirements:[],checks:[],findings:[],evidence:[],actions:[],workers:[],events:[],messages:[],requests:[],responses:[]};
  const put=(list,record,key='id')=>{const i=list.findIndex(v=>v[key]===record[key]);if(i<0)list.push(record);else list[i]=record};
  for(const event of replayEvents(snap)){
    if(event.sequence>sequence)break;const d=event.data;out.events.push(event);
    const status={run_created:'created',run_started:'running',run_paused:'paused',run_cancelled:'cancelled',run_completed:'completed',run_error:'failed',plan_failed:'failed'}[event.type];if(status)out.run.status=status;
    if(event.type==='run_created'&&d){out.requirements=structuredClone(d.requirements||[]);out.run.success_contains=d.success_contains;out.run.budget=d.budget}
    if(event.type==='plan_updated'&&d?.tasks)out.plan=structuredClone(d);
    if(['task_claimed','task_completed','task_disposed'].includes(event.type)&&d?.id)put(out.plan.tasks,structuredClone(d));
    if(event.type==='task_retry_requested'){const t=out.plan.tasks.find(t=>t.id===d?.task_id);if(t){t.status='pending';t.child_run_id='';t.worker_id=''}}
    if(['asset_added','asset_updated'].includes(event.type)&&d?.id)put(out.assets,structuredClone(d));
    if(['relation_added','relation_updated'].includes(event.type)&&d){const index=out.relations.findIndex(r=>r.from===d.from&&r.to===d.to&&r.kind===d.kind);if(index<0)out.relations.push(structuredClone(d));else out.relations[index]=structuredClone(d)}
    if(event.type==='requirements_set'&&Array.isArray(d))out.requirements=structuredClone(d);
    if(event.type==='evidence_checked'&&d?.requirement_id)out.checks.push(structuredClone(d));
    if(event.type==='finding_submitted'&&d?.id)out.findings.push({id:d.id,title:'已登记发现 '+d.id,status:'submitted',impact:'此事件仅保留发现身份及证据引用，未保存完整历史正文。',evidence_ids:(d.evidence||[]).map(e=>e.id).filter(Boolean),reproduction_steps:[]});
    if(event.type==='finding_reviewed'){const f=out.findings.find(f=>f.id===d?.id);if(f)f.status=d.status==='pass'?'reviewed':d.status==='fail'?'rejected':'needs_evidence'}
    if(event.type==='finding_triaged'){const f=out.findings.find(f=>f.id===d?.id);if(f)f.triage=d.triage}
    if(event.type==='tool_result'){
      const e=(snap.evidence||[]).find(e=>e.id===d?.evidence_id&&e.source_event_sequence===event.sequence);
      if(e){out.evidence.push({...e,verification:'unverified'});const action=(snap.actions||[]).find(a=>a.call_id===e.call_id&&a.request_id===e.request_id);if(action){let result;try{result=JSON.parse(e.content)}catch{result={stdout:e.content}}out.actions.push({request_id:action.request_id,call_id:action.call_id,name:action.name,local_name:action.local_name,adapter:action.adapter,arguments:action.arguments,status:result.outcome_uncertain?'uncertain':result.error?'failed':'completed',result})}}
    }
  }
  out.run.event_seq=sequence;return out;
}
export function graphSnapshot(s){return s.replaySnapshot?.run.id===s.snap?.run.id&&s.replaySequence!==undefined?projectHistory(s.replaySnapshot,s.replaySequence):s.snap}
export function replayToolbar(s){
  const replay=s.replaySnapshot?.run.id===s.snap?.run.id&&s.replaySequence!==undefined,events=replay?replayEvents(s.replaySnapshot):[],index=events.findIndex(e=>e.sequence===s.replaySequence),event=events[index];
  return `<div class="replay-toolbar">${btn(replay?'replay-live':'replay-open',replay?'返回当前图':'事件回放',s.replayLoading?'disabled':'')}${s.replayLoading?'<span role="status">正在读取历史事件…</span>':''}${s.replayError?`<p class="inline-error">${esc(s.replayError)}</p>`:''}${replay?`<label>历史事件<input id="replay-position" data-ephemeral="true" type="range" min="0" max="${Math.max(0,events.length-1)}" value="${Math.max(0,index)}" ${events.length?'':'disabled'}></label><div class="buttons">${btn('replay-step','← 上一步',`data-step="-1" ${index<=0?'disabled':''}`)}${btn('replay-step','下一步 →',`data-step="1" ${index>=events.length-1?'disabled':''}`)}<output>#${esc(event?.sequence??0)} · ${esc(event?eventLabel(event.type):'尚无图事件')} · ${esc(event?.created_at||'')}</output></div><p class="note">只读历史投影，执行继续按当前状态进行。仅恢复事件中保存的内容；未保存的完整报告/发现正文不从当前状态回填。历史证据的来源验证状态不作追认。</p>`:''}</div>`;
}
export function mountGraphPan(canvas){
  if(canvas.dataset.panMounted)return;canvas.dataset.panMounted='true';let drag;
  canvas.addEventListener('pointerdown',e=>{if(e.button!==0||e.target.closest('button,a,input,[data-action]'))return;const bounds=canvas.getBoundingClientRect();if(e.clientX>=bounds.left+canvas.clientWidth-20||e.clientY>=bounds.top+canvas.clientHeight-20)return;drag={id:e.pointerId,x:e.clientX,y:e.clientY,left:canvas.scrollLeft,top:canvas.scrollTop};canvas.setPointerCapture(e.pointerId);canvas.classList.add('panning');e.preventDefault()});
  canvas.addEventListener('pointermove',e=>{if(drag&&drag.id===e.pointerId){canvas.scrollLeft=drag.left+drag.x-e.clientX;canvas.scrollTop=drag.top+drag.y-e.clientY}});
  const end=()=>{drag=null;canvas.classList.remove('panning')};canvas.addEventListener('pointerup',end);canvas.addEventListener('pointercancel',end);canvas.addEventListener('lostpointercapture',end);
}
