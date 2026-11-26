import {esc,btn} from './ui.js';
import {eventLabel} from './event-labels.js';

// Graph interaction references: LuaN1aoAgentv2 a035429, GraphView.tsx;
// Cairn 8e7e0ea, cairn/server/static/index.html snapshotNodePositions (AGPL-3.0).
// Replay is a read-only projection of Trace events, never a Runtime rollback.
const graphEvent=/^(run_|plan_(updated|failed)$|task_|worker_(started|resumed|result_ready|finished|cleanup_reconciled|cancel_requested|interrupted|lease_expired|yielded|paused|capacity_updated)$|asset_|relation_|requirements_set$|evidence_checked$|tool_result$|finding_)/;
export const replayEvents=snap=>(snap.events||[]).filter(e=>graphEvent.test(e.type)).sort((a,b)=>a.sequence-b.sequence);
export function projectHistory(snap,sequence){
  const out={run:{id:snap.run.id,goal:snap.run.goal,status:'created'},plan:{tasks:[]},assets:[],relations:[],requirements:[],checks:[],findings:[],evidence:[],actions:[],workers:[],child_reports:[],events:[],messages:[],requests:[],responses:[],history_sequence:sequence,history_incomplete:[]};
  const put=(list,record,key='id')=>{const i=list.findIndex(v=>v[key]===record[key]);if(i<0)list.push(record);else list[i]=record};
  const incomplete=(event,record)=>{if(record)record.history_incomplete=true;out.history_incomplete.push('#'+event.sequence+' '+event.type+'：历史信息不完整')};
  for(const event of replayEvents(snap)){
    if(event.sequence>sequence)break;const d=event.data;out.events.push(event);
    const status={run_created:'created',run_started:'running',run_resumed:'running',run_paused:'paused',run_cancelled:'cancelled',run_completed:'completed',run_error:'failed',plan_failed:'failed'}[event.type];if(status)out.run.status=status;
    if(event.type==='run_cancel_requested')out.run.cancel_requested=true;
    if(event.type==='run_created'&&d){out.requirements=structuredClone(d.requirements||[]);out.run.success_contains=d.success_contains;out.run.budget=structuredClone(d.budget);if(d.goal)out.run.goal=d.goal}
    if(event.type==='plan_updated'&&d?.tasks)out.plan=structuredClone(d);
    if(['task_claimed','task_completed','task_disposed','task_dispatch_settled'].includes(event.type)&&d?.id)put(out.plan.tasks,structuredClone(d));
    if(event.type==='task_retry_requested'){const t=out.plan.tasks.find(t=>t.id===d?.task_id);if(t){t.status='pending';t.child_run_id='';t.worker_id='';delete t.history_incomplete}}
    if(event.type==='task_blocked_capability'||event.type==='task_capability_restored'){
      const t=out.plan.tasks.find(t=>t.id===d?.task_id);
      if(t){t.blocked_reason=event.type==='task_blocked_capability'?structuredClone(d):null;if(event.type==='task_blocked_capability')t.status='blocked_capability';else if(t.status==='blocked_capability')t.status='pending'}else incomplete(event);
    }
    if(event.type==='worker_capacity_updated'&&Number.isInteger(d?.capacity))out.plan.capacity=d.capacity;
    if(event.type==='worker_started'||event.type==='worker_resumed'){
      if(d?.id){
        const worker=structuredClone(d);worker.started_at=event.created_at;worker.visit_started_sequence=event.sequence;
        if(worker.visit===undefined)incomplete(event,worker);
        put(out.workers,worker);
      }else incomplete(event);
    }
    if(['worker_result_ready','worker_finished','worker_cleanup_reconciled','worker_cancel_requested','worker_interrupted','worker_lease_expired','worker_paused','worker_yielded'].includes(event.type)){
      const workerID=d?.worker_id||d?.id,worker=out.workers.find(w=>w.id===workerID),visit=d?.visit===undefined?undefined:Number(d.visit);
      const sameVisit=worker&&visit!==undefined&&Number.isInteger(visit)&&visit===worker.visit&&(!d.run_id||d.run_id===worker.run_id)&&(!d.child_run_id||d.child_run_id===worker.run_id)&&(!d.task_id||d.task_id===worker.task_id);
      if(event.type==='worker_cancel_requested'){
        if(sameVisit){
          const cancelled=d.worker,task=d.task,currentTask=out.plan.tasks.find(t=>t.id===worker.task_id);
          // Already-cleaned paused workers carry the committed cancellation snapshots.
          if(cancelled?.id===worker.id&&cancelled.run_id===worker.run_id&&cancelled.task_id===worker.task_id&&cancelled.visit===visit&&cancelled.status==='cancelled'&&cancelled.cleanup_status==='completed'&&task?.id===worker.task_id&&task.worker_id===worker.id&&task.child_run_id===worker.run_id&&task.status==='cancelled'&&currentTask?.worker_id===worker.id&&currentTask.child_run_id===worker.run_id){
            put(out.workers,{...structuredClone(cancelled),started_at:worker.started_at,visit_started_sequence:worker.visit_started_sequence});
            put(out.plan.tasks,structuredClone(task));
          }else{worker.cancel_requested=true;if(['starting','running','paused','uncertain'].includes(worker.status))worker.status='cancelling'}
        }else if(visit===undefined||!worker)incomplete(event,worker);
      }else if(!sameVisit){
        if(visit===undefined||!worker){incomplete(event,worker);const task=out.plan.tasks.find(t=>t.id===(d?.task_id||worker?.task_id));if(task)task.history_incomplete=true}
      }else{
        if(event.type==='worker_result_ready'){worker.result_ready=true;worker.source_hash=d.source_hash}
        if(event.type==='worker_interrupted'||event.type==='worker_lease_expired'){worker.status='uncertain';if(d.error)worker.error=d.error}
        if(event.type==='worker_paused'||event.type==='worker_yielded')worker.status='paused';
        if(event.type==='worker_finished'||event.type==='worker_cleanup_reconciled'){
          if(d.status)worker.status=d.status;
          if(d.cleanup_status)worker.cleanup_status=d.cleanup_status;
          if(d.status&&d.cleanup_status){worker.finished_at=event.created_at;worker.settled_visit=visit}else incomplete(event,worker);
          const task=out.plan.tasks.find(t=>t.id===worker.task_id);if(task)incomplete(event,task);
        }
      }
      if(['worker_result_ready','worker_finished','worker_cleanup_reconciled'].includes(event.type)&&d?.source_hash&&visit!==undefined&&Number.isInteger(visit)){
        const taskID=d.task_id||(sameVisit?worker.task_id:undefined),runID=d.run_id||(sameVisit?worker.run_id:undefined);
        if(taskID&&runID&&workerID){
          const report={parent_run_id:out.run.id,source_run_id:runID,task_id:taskID,worker_id:workerID,visit,source_hash:d.source_hash,run_status:d.run_status,result_status:d.result_status,cleanup_status:d.cleanup_status,summary:'历史信息不完整：事件未保存固定报告正文。',source_status:'历史未核验',settlement_pending:event.type==='worker_result_ready',history_incomplete:true};
          const key=r=>JSON.stringify([r.source_run_id,r.task_id,r.worker_id,r.visit,r.source_hash]);
          if(!out.child_reports.some(r=>key(r)===key(report)))out.child_reports.push(report);
        }else incomplete(event);
      }
    }
    if(['asset_added','asset_updated'].includes(event.type)&&d?.id)put(out.assets,structuredClone(d));
    if(['relation_added','relation_updated'].includes(event.type)&&d){const index=out.relations.findIndex(r=>r.from===d.from&&r.to===d.to&&r.kind===d.kind);if(index<0)out.relations.push(structuredClone(d));else out.relations[index]=structuredClone(d)}
    if(event.type==='requirements_set'&&Array.isArray(d))out.requirements=structuredClone(d);
    if(event.type==='evidence_checked'&&d?.requirement_id)out.checks.push(structuredClone(d));
    if(event.type==='finding_submitted'&&d?.id)out.findings.push({id:d.id,title:'已登记发现 '+d.id,status:'submitted',impact:'此事件仅保留发现身份及证据引用，未保存完整历史正文。',evidence_ids:(d.evidence||[]).map(e=>e.id).filter(Boolean),reproduction_steps:[],history_incomplete:true});
    if(event.type==='finding_reviewed'){const f=out.findings.find(f=>f.id===d?.id);if(f)f.status=d.status==='pass'?'reviewed':d.status==='fail'?'rejected':'needs_evidence'}
    if(event.type==='finding_triaged'){const f=out.findings.find(f=>f.id===d?.id);if(f)f.triage=d.triage}
    if(event.type==='tool_result'){
      const e=(snap.evidence||[]).find(e=>e.id===d?.evidence_id&&e.source_event_sequence===event.sequence);
      if(e){
        const {id,request_id,call_id,source_event_sequence,content_hash,content,type,evidence_scope}=e;
        out.evidence.push({id,request_id,call_id,source_event_sequence,content_hash,content,type,evidence_scope:structuredClone(evidence_scope),verification:'unverified'});
        const action=(snap.actions||[]).find(a=>a.call_id===call_id&&a.request_id===request_id);
        if(action){let result;try{result=JSON.parse(content)}catch{result={stdout:content}}out.actions.push({request_id,call_id,name:action.name,local_name:action.local_name,adapter:action.adapter,arguments:action.arguments,status:result.outcome_uncertain?'uncertain':result.error?'failed':'completed',result})}
      }else incomplete(event);
    }
  }
  out.run.event_seq=sequence;return out;
}
export function graphSnapshot(s){return s.replaySnapshot?.run.id===s.snap?.run.id&&s.replaySequence!==undefined?projectHistory(s.replaySnapshot,s.replaySequence):s.snap}
export function detailSnapshot(s,element){
  const cutoff=element.closest?.('[data-history-sequence]')?.dataset;
  if(!cutoff)return s.snap;
  const source=s.replaySnapshot?.run.id===cutoff.historyRun?s.replaySnapshot:{run:{id:cutoff.historyRun}};
  return projectHistory(source,Number(cutoff.historySequence));
}
export function historyDetail(snap,html){
  return Number.isSafeInteger(snap.history_sequence)?'<div data-history-run="'+esc(snap.run.id)+'" data-history-sequence="'+snap.history_sequence+'"><p class="note">历史截止 #'+snap.history_sequence+'；仅显示该截止点已保存的信息，当前来源核验不追认历史。'+(snap.history_incomplete?.length?' 历史信息不完整。':'')+'</p>'+html+'</div>':html;
}
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
