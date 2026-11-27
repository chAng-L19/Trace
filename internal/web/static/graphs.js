import {esc,empty,label,btn} from './ui.js';
import {explorationData,explorationProjection,explorationGeometry,explorationViewStyle,childReportCounts,workProgressSummary} from './exploration.js';

// Layout uses persisted identities only. Edges express exact ledger references.
function graph(nodes,edges,label,viewStyle='',run=''){
  if(!nodes.length)return empty('暂无可追溯节点','执行动作或登记资产后，此处显示真实关系。');
  let positions=new Map();const columns=new Map(),preview=label==='workspace-lineage';
  for(const n of nodes){const row=columns.get(n.column)||0;columns.set(n.column,row+1);positions.set(n.id,{x:24+n.column*252,y:28+row*92})}
  let width=(Math.max(...columns.keys())+1)*252+20,height=Math.max(...columns.values())*92+35;
  let routes;
  if(preview)({positions,width,height,routes}=explorationGeometry(run,nodes,edges,true));
  return `<div class="graph-shell" data-scroll="${label}" ${preview?`tabindex="0" style="${viewStyle}" aria-label="探索简图，拖动空白平移，右下角调整尺寸"`:''}><svg width="${width}" height="${height}" viewBox="0 0 ${width} ${height}" role="group" aria-label="${label==='asset-coverage'?'资产覆盖关系图':'运行探索链路图'}"><defs><marker id="arrow-${label}" markerWidth="7" markerHeight="7" refX="6" refY="3" orient="auto"><path d="M0,0 L6,3 L0,6" fill="#a9a391"/></marker></defs>${edges.map((e,i)=>{
    const a=positions.get(e.from),b=positions.get(e.to);if(!a||!b)return '';
    const x=a.x+212,y=a.y+28,tx=b.x,ty=b.y+28;
    const d=routes?.[i].d||`M${x} ${y} C${x+24} ${y},${tx-24} ${ty},${tx} ${ty}`;
    if(preview)return `<g id="${esc('preview-edge-'+run+'-'+encodeURIComponent(JSON.stringify([e.from,e.to,e.relation])))}" class="exploration-edge ${routes[i].detour?'detour':''}" data-action="exploration-chain" data-from="${esc(e.from)}" data-to="${esc(e.to)}" data-relation="${esc(e.label)}" role="button" tabindex="0" aria-label="高亮链路：${esc(e.label)}" aria-pressed="false"><path class="edge-hit" d="${d}"/><path class="edge-line" d="${d}" marker-end="url(#arrow-${label})"/><title>${esc(e.label)}</title></g>`;
    return `<path class="edge" d="${d}" marker-end="url(#arrow-${label})"><title>${esc(e.label)}</title></path>`;
  }).join('')}${nodes.map(n=>{
    const p=positions.get(n.id);return `<a id="${esc(label+'-node-'+encodeURIComponent(n.id))}" href="#${n.page||'assets'}" data-action="${n.action}" ${n.attrs||''} ${preview?`data-chain-node="${esc(n.id)}" data-chain-title="${esc(n.title)}"`:''} data-node-search="${esc([n.id,n.title,n.subtitle].join(' ').toLowerCase())}" class="${esc(n.status||'')}" aria-label="${esc(n.title)}"><g transform="translate(${p.x},${p.y})"><rect class="node-box" width="212" height="58"/><text x="12" y="21">${esc(n.title.length>25?n.title.slice(0,24)+'…':n.title)}</text><text class="graph-small" x="12" y="42">${esc((n.subtitle||'').length>28?n.subtitle.slice(0,27)+'…':n.subtitle||'')}</text><title>${esc(n.title+' / '+(n.subtitle||''))}</title></g></a>`
  }).join('')}</svg></div>`;
}
export function assetGraph(s){
  const assets=s.snap.assets||[],ids=new Set(assets.map(a=>a.id));
  const edges=(s.snap.relations||[]).map(r=>({from:r.from,to:r.to,label:({contains:'包含',links_to:'指向',depends_on:'依赖',resolves_to:'解析到'})[r.kind]||r.kind}));
  for(const a of assets)if(a.parent_id)edges.push({from:a.parent_id,to:a.id,label:'上级资产'});
  const incoming=new Map(assets.map(a=>[a.id,0])),children=new Map(),depth=new Map();
  for(const e of edges){if(!ids.has(e.from)||!ids.has(e.to))continue;incoming.set(e.to,incoming.get(e.to)+1);if(!children.has(e.from))children.set(e.from,[]);children.get(e.from).push(e.to)}
  const ready=assets.filter(a=>!incoming.get(a.id)).map(a=>a.id);
  for(const id of ready)depth.set(id,0);
  for(let i=0;i<ready.length;i++){const id=ready[i];for(const child of children.get(id)||[]){depth.set(child,Math.max(depth.get(child)||0,depth.get(id)+1));incoming.set(child,incoming.get(child)-1);if(!incoming.get(child))ready.push(child)}}
  // Cyclic components stay in one column; actual edges remain visible.
  const nodes=assets.map(a=>({id:a.id,column:incoming.get(a.id)?0:depth.get(a.id)||0,title:a.value,subtitle:`${label(a.kind)} / ${label(a.coverage)}`,status:[a.coverage,s.graphFilter?(a.coverage===s.graphFilter?'coverage-match':'coverage-muted'):''].join(' '),action:'asset-detail',attrs:`data-id="${esc(a.id)}"`}));
  const note=s.graphFilter&&!assets.some(a=>a.coverage===s.graphFilter)?'<p class="note" role="status">没有匹配的资产；仍显示全部上下游关系。</p>':'';
  return note+graph(nodes,edges,'asset-coverage');
}
export {explorationGraph} from './exploration.js';

// Workspace already has the plan and source-pinned child receipts. Reuse those
// references without loading full evidence/assets or creating a second ledger.
export function explorationPreview(snap){
  const all=explorationData({run:snap.run,plan:snap.plan,child_reports:snap.child_reports,result:snap.result}),mainline=explorationProjection(all);
  const reports=all.nodes.filter(n=>n.kind==='child_report'),tasks=all.nodes.filter(n=>n.kind==='intent');
  const recent=new Map(reports.map((n,i)=>['task:'+n.record.task_id,i]));
  const active=n=>['starting','running','claimed','paused','uncertain','cancelling'].includes(n.record.status)||(snap.workers||[]).some(w=>w.task_id===n.record.id&&(['starting','running','cancelling','paused','uncertain'].includes(w.status)||w.cleanup_status!=='completed'))||reports.some(r=>r.record.task_id===n.record.id&&(r.record.delivery_pending||r.record.settlement_pending));
  const priority=n=>active(n)?0:n.record.status==='blocked_capability'||n.record.blocked_reason?1:recent.has(n.id)?2:3;
  const selectedTasks=tasks.map((n,i)=>({n,i})).sort((a,b)=>priority(a.n)-priority(b.n)||(recent.get(b.n.id)??-1)-(recent.get(a.n.id)??-1)||a.i-b.i).slice(0,8).map(x=>x.n);
  const taskIDs=new Set(selectedTasks.map(n=>n.id)),activeIDs=new Set(selectedTasks.filter(active).map(n=>n.id));
  const reportPriority=n=>activeIDs.has('task:'+n.record.task_id)||n.record.delivery_pending||n.record.settlement_pending?0:1;
  const selectedReports=mainline.nodes.filter(n=>n.kind==='child_report').map((n,i)=>({n,i})).filter(({n})=>taskIDs.has('task:'+n.record.task_id)||!tasks.some(t=>t.id==='task:'+n.record.task_id)).sort((a,b)=>reportPriority(a.n)-reportPriority(b.n)||b.i-a.i).slice(0,6).sort((a,b)=>a.i-b.i).map(x=>x.n);
  const ids=new Set(['goal','report',...selectedTasks.map(n=>n.id),...selectedReports.map(n=>n.id)]);
  const progress=all.nodes.filter(n=>n.kind==='work'&&taskIDs.has('task:'+n.record.task_id));
  const workPriority=n=>n.record.status==='verified'?2:['blocked','abandoned'].includes(n.record.status)?1:0;
  const selectedProgress=progress.map((n,i)=>({n,i})).sort((a,b)=>workPriority(a.n)-workPriority(b.n)||a.i-b.i).slice(0,6).map(({n})=>n);
  for(const n of selectedProgress)ids.add(n.id);
  const columns={goal:0,intent:1,child_report:2,report:2,work:2};
  const nodes=all.nodes.filter(n=>ids.has(n.id)).map(n=>({id:n.id,column:columns[n.kind],title:n.title,subtitle:n.state,status:n.kind,action:'exploration-detail',attrs:`data-current="true" data-id="${esc(n.id)}"`,page:'activity'}));
  const edges=all.edges.filter(e=>ids.has(e.from)&&ids.has(e.to)).map(e=>({...e,label:e.relation}));
  return `<div class="exploration-preview" data-exploration-run="${esc(snap.run.id)}" data-exploration-preview="true" aria-label="探索链路简图"><div class="exploration-selection"><span class="note">点击连线高亮 · 右下角拖动尺寸</span>${btn('exploration-control','清除高亮','data-command="clear"')}</div>${graph(nodes,edges,'workspace-lineage',explorationViewStyle(snap.run.id,true),snap.run.id)}</div><p class="note">当前快照：${tasks.length} 个方向 · ${childReportCounts(snap)}；简图显示 ${selectedTasks.length} 个方向、${selectedReports.length} 个访问组；省略 ${tasks.length-selectedTasks.length} 个方向、${reports.length-selectedReports.length} 个载入访问组。工作项显示 ${selectedProgress.length} / ${all.nodes.filter(n=>n.kind==='work').length} 项。${workProgressSummary(snap.plan?.work_progress)}默认任务主线；点节点按需读取来源、证据与历史 visit，完整探索图保留全量视图和搜索。拖动空白平移；连线只表示已保存的关系。</p>`;
}
