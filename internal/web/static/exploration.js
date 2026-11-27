import {esc,json,label,btn,empty,evidenceLinks} from './ui.js';
import {evidenceReading,evidenceDetail} from './evidence.js';
import {evidencePresentation,evidenceText} from './evidence-report.js';
import {graphSnapshot,replayToolbar,mountGraphPan,historyDetail} from './history-projection.js';
import {reportDownloads} from './delivery.js';

// References: ARTEX e6ec56999175509551a1e43a3ee3b053d0941bd9,
// web/src/components/exploration-graph.tsx + db/exploration.go (AGPL-3.0);
// Muteki 144df7666dc78ef331e7746357d31c9e0f57c558, EvidenceChain.tsx (AGPL-3.0);
// Cairn 8e7e0ea67552383851dfcabfba0c4e9c8d007878, index.html: edge/background tap;
// LuaN1aoAgentv2 a035429766746866bcab30550cd0aac9c539be04, GraphView.tsx
// and graph.test.ts: dim non-neighbors; Trace keeps parallel relations, not a tree.
// pi 8a7b0c03dfb702663acafb6dc29f8acaa4ffe391, tree-selector.ts (MIT).
// Typed reasoning, provenance disclosure and ancestor-preserving filtering only;
// this projects Trace Store references, not tool activity or a second graph ledger.
// Projection/change detection: Cairn 8e7e0ea67552383851dfcabfba0c4e9c8d007878,
// server/static/index.html buildElements/updateGraph; Sapper v2
// 7b75e6f1f5d5b4ae0c55878b0dd0994d9b08f1fb, lib/view_template.html refresh.
const workStates={pending:'待续',scheduled:'已安排续作',verified:'验收通过',unknown:'待核对',blocked:'受阻（显式处置）',abandoned:'已放弃（显式处置）'};
const kinds={work:'工作项/范围覆盖',goal:'研究目标',intent:'探索方向',observation:'关联证据',requirement:'验收验证',finding:'登记发现',report:'研究报告',child_report:'子报告',source:'固定版本来源',external:'外部证据'};
const references=record=>[...new Set([...(record?.evidence_ids||[]),...(record?.references||[]).map(ref=>ref.id)].filter(Boolean))];
const findingStates={submitted:'待审阅',reviewed:'审阅通过',reproduced:'复测通过',rejected:'审阅未通过',needs_evidence:'待补证据',invalid:'来源失效'};
const queries=new Map();let matchIndex=0;
const viewStates=new Map(),snapshots=new Map();
let resizeObserver,layoutCache;
const mountedCanvases=new WeakSet();
const viewState=id=>{if(!viewStates.has(id))viewStates.set(id,{zoom:1,mode:'initial',kind:'',projection:'mainline',expanded:new Set(),get size(){return ['width','height'].filter(k=>this[k]).map(k=>k+':'+this[k]).join(';')}});return viewStates.get(id)};
const rootView=root=>viewState(root.dataset.explorationRun+(root.dataset.explorationPreview?':preview':''));
export const explorationViewStyle=(run,preview=false)=>viewState(run+(preview?':preview':'')).size||'';
export function workProgressSummary(progress=[]){
  if(!progress.length)return '工作项/范围覆盖未知：Plan 未提供 work_progress；阶段运行结束不代表目标验收通过。';
  const verified=progress.filter(p=>p.status==='verified').length,blocked=progress.filter(p=>p.status==='blocked').length,abandoned=progress.filter(p=>p.status==='abandoned').length;
  return `工作项/范围覆盖 ${progress.length} 项 · 验收通过 ${verified} 项 · 剩余/待核对 ${progress.length-verified-blocked-abandoned} 项 · 受阻 ${blocked} 项 · 已放弃 ${abandoned} 项。受阻与放弃为显式处置，不计入验收通过；未登记范围保持未知。`;
}
export function explorationSubset(data,kind){
  if(!kind)return data;
  const selected=new Set(data.nodes.filter(n=>n.kind===kind).map(n=>n.id)),visible=new Set(selected);
  for(const e of data.edges)if(selected.has(e.from))visible.add(e.to);
  // Keep every recorded ancestor of the selection, including dependency chains.
  const ancestors=[...selected],visited=new Set(selected);
  for(let i=0;i<ancestors.length;i++)for(const e of data.edges)if(e.to===ancestors[i]&&!visited.has(e.from)){visible.add(e.from);visited.add(e.from);ancestors.push(e.from)}
  return {nodes:data.nodes.filter(n=>visible.has(n.id)),edges:data.edges.filter(e=>visible.has(e.from)&&visible.has(e.to))};
}
export function explorationProjection(data,{projection='mainline',kind='',expanded=new Set(),query=''}={}){
  if(query.trim())return data;
  if(kind)return explorationSubset(data,kind);
  if(projection==='all')return data;
  const visible=new Set(data.nodes.filter(n=>['goal','intent','work','requirement','finding','report'].includes(n.kind)).map(n=>n.id));
  const current=new Map(),known=new Set();
  for(const n of data.nodes.filter(n=>n.kind==='child_report')){
    const key=n.record.task_id||n.record.source_run_id;
    if(n.record.current===false)continue;
    if(n.record.current===true){visible.add(n.id);known.add(key)}
    else current.set(key,n);
  }
  for(const [key,n] of current)if(!known.has(key))visible.add(n.id);
  for(const id of expanded){
    if(!data.nodes.some(n=>n.id===id))continue;
    visible.add(id);
    const children=data.edges.filter(e=>e.from===id).map(e=>e.to);
    for(const child of children){
      visible.add(child);
      if(data.nodes.some(n=>n.id===child&&n.kind==='child_report'))for(const e of data.edges)if(e.from===child)visible.add(e.to);
    }
    for(const e of data.edges)if(e.to===id)visible.add(e.from);
  }
  return {nodes:data.nodes.filter(n=>visible.has(n.id)),edges:data.edges.filter(e=>visible.has(e.from)&&visible.has(e.to))};
}
export function explorationScale(width,height,viewportWidth,viewportHeight,mode='fit'){
  const fit=Math.min(1,(viewportWidth-24)/width,(viewportHeight-24)/height);
  return Math.max(mode==='initial'?.65:.08,fit);
}
export function resolveEvidenceReference(snap,reference,sourceRunID=snap.run.id){
  const id=typeof reference==='string'?reference:reference.id,slash=id.indexOf('/');
  const run_id=slash<0?sourceRunID:id.slice(0,slash),evidence_id=slash<0?id:id.slice(slash+1),external=run_id!==snap.run.id;
  return {run_id,evidence_id,external,id:external?run_id+'/'+evidence_id:evidence_id,node_id:external?'external:'+run_id+':'+evidence_id:'evidence:'+evidence_id};
}
export const childReportKey=(report,version=false)=>JSON.stringify([report.source_run_id,report.task_id,report.worker_id,report.visit,...(version?[report.source_hash]:[])]);
export function childReportGroups(reports=[]){
  const groups=new Map(),seen=new Set();
  for(const report of reports){
    const version=childReportKey(report,true);if(seen.has(version))continue;seen.add(version);
    const key=childReportKey(report);if(!groups.has(key))groups.set(key,{key,versions:[]});
    const group=groups.get(key);group.versions.push(report);group.report=report;
    // Map order follows the most recent registration, never CreatedAt.
    groups.delete(key);groups.set(key,group);
  }
  return [...groups.values()];
}
export function childReportCounts(snap){
  const loaded=(snap.child_reports||[]).length,total=snap.totals?.child_reports??loaded;
  return '累计回执 '+total+' 条 · 载入回执 '+loaded+' 条 · 载入访问组 '+childReportGroups(snap.child_reports).length+' 组'+(total>loaded?' · 未载入 '+(total-loaded)+' 条':'');
}
export function explorationData(snap){
  const nodes=[],edges=[],run=snap.run||{},tasks=snap.plan?.tasks||[],progress=snap.plan?.work_progress||[],evidence=snap.evidence||[];
  const seenEdges=new Set(),missing=new Set(),presentations=new Map(evidence.map(e=>[e.id,evidencePresentation(e,snap)]));
  const add=(id,kind,title,state,record,summary='')=>{
    const t=evidenceText(title,kinds[kind],80),s=evidenceText(summary,'此处保留'+kinds[kind]+'的原始记录；展开详情阅读原文。',180);
    const node={id,kind,title:t.value,summary:s.value,original:{title:t.original,summary:s.original},state,record,search:[id,title,summary,t.value,s.value,state,record.call_id,record.request_id,record.child_run_id,record.hint,record.next_action,record.reason].filter(Boolean).join(' ').toLowerCase()};nodes.push(node);return node;
  };
  const edge=(from,to,relation)=>{const key=[from,to,relation].join('\0');if(from!==to&&!seenEdges.has(key)){seenEdges.add(key);edges.push({from,to,relation})}};
  const latest=new Map();for(const c of snap.checks||[])latest.set(c.requirement_id,c);
  const reportRecords=[snap.result,...(snap.result?.answers||[]),...(snap.findings||[]).flatMap(f=>[f,...(f.retests||[])]),...(snap.requirements||[]).map(r=>latest.get(r.id))];
  const reportIDs=new Set(reportRecords.flatMap(references).map(id=>resolveEvidenceReference(snap,id).id));
  const explorationIDs=new Set();
  for(const t of tasks)for(const id of t.evidence_ids||[])explorationIDs.add(resolveEvidenceReference(snap,id,t.evidence_run_id||run.id).id);
  for(const p of progress)for(const id of p.evidence_ids||[])explorationIDs.add(resolveEvidenceReference(snap,id).id);
  for(const a of snap.assets||[])for(const id of a.evidence_ids||[])explorationIDs.add(resolveEvidenceReference(snap,id,a.source_run_id||run.id).id);
  for(const r of snap.relations||[])for(const id of r.evidence_ids||[])explorationIDs.add(resolveEvidenceReference(snap,id).id);
  const evidenceNode=(id,sourceRunID)=>{
    const ref=resolveEvidenceReference(snap,id,sourceRunID);
    if(ref.external&&!nodes.some(n=>n.id===ref.node_id))add(ref.node_id,'external','来源证据 · '+ref.evidence_id,'尚未载入来源',ref,'来源 '+ref.run_id+'；原始读取、内容完整性与父目标验收分别核对');
    return ref.node_id;
  };
  add('goal','goal','研究目标','运行：'+label(run.status),run,run.goal||'尚未填写研究目标');
  for(const t of tasks){add('task:'+t.id,'intent',t.goal||'探索方向',t.history_incomplete?'历史信息不完整':'运行：'+label(t.status),t,t.error?'执行说明：'+t.error:t.completion_criteria?'完成条件：'+t.completion_criteria:t.success_contains?'精确判据：'+t.success_contains:'计划记录中的探索方向');edge('goal','task:'+t.id,'计划方向');for(const d of t.dependencies||[])edge('task:'+d,'task:'+t.id,'依赖');}
  for(const p of progress){
    const id='work:'+JSON.stringify([p.task_id,p.item_id]);
    add(id,'work',p.goal,'工作项：'+(workStates[p.status]||p.status||'待核对'),p,[p.next_action,p.reason].filter(Boolean).join('；'));
    if(tasks.some(t=>t.id===p.task_id))edge('task:'+p.task_id,id,'工作范围');
    for(const taskID of p.task_ids||[])if(tasks.some(t=>t.id===taskID))edge(id,'task:'+taskID,'关联阶段');
    for(const evidenceID of p.evidence_ids||[])edge(evidenceNode(evidenceID),id,'工作项引用');
  }
  for(const e of evidence){
    const p=presentations.get(e.id);if(p.graphEligible===false||!reportIDs.has(e.id)&&!(explorationIDs.has(e.id)&&p.graphEligible))continue;
    const r=evidenceReading(e),failed=r.result&&(r.result.error||r.result.mcp_is_error||r.result.exit_code!==undefined&&r.result.exit_code!==0),uncertain=p.action?.status==='uncertain'||r.result&&(r.result.interrupted||r.result.outcome_uncertain||r.result.worker_outcome_uncertain);
    add('evidence:'+e.id,'observation',p.title,(uncertain?'执行结果待核对 · ':failed?'执行失败 · ':'')+r.source,e,p.summary);
  }
  for(const t of tasks)for(const id of t.evidence_ids||[])edge('task:'+t.id,evidenceNode(id,t.evidence_run_id||run.id),'方向引用');
  for(const a of snap.assets||[])if(a.source_task_id&&tasks.some(t=>t.id===a.source_task_id))for(const id of a.evidence_ids||[])edge('task:'+a.source_task_id,evidenceNode(id,a.source_run_id||run.id),'资产引用');
  for(const r of snap.requirements||[]){const c=latest.get(r.id);add('requirement:'+r.id,'requirement',r.description,c?({pass:'判据通过',fail:'判据未通过',unknown:'待核对'}[c.status]||c.status):'待验证',{requirement:r,check:c},c?.reason||'尚无验收检查记录');edge('goal','requirement:'+r.id,'验收要求');for(const id of references(c))edge(evidenceNode(id),'requirement:'+r.id,'检查引用');}
  for(const f of snap.findings||[]){add('finding:'+f.id,'finding',f.title,findingStates[f.status]||label(f.status),f,f.impact||'尚未记录影响说明');for(const id of references(f))edge(evidenceNode(id),'finding:'+f.id,'发现引用');for(const t of f.retests||[])for(const id of references(t))edge(evidenceNode(id),'finding:'+f.id,'复测引用');}
  for(const group of childReportGroups(snap.child_reports)){
    const report=group.report,key=group.key,id='child-report:'+key,source='child-source:'+key;
    add(id,'child_report','子报告 · '+(report.task_id||report.source_run_id),'运行：'+label(report.run_status)+' · 业务：'+label(report.result_status),report,report.summary).versions=group.versions;
    edge(tasks.some(t=>t.id===report.task_id)?'task:'+report.task_id:'goal',id,'子结算报告');
    add(source,'source','来源 · '+report.source_run_id,label(report.source_status)+(report.current===true?' · 当前 visit':report.current===false?' · 历史 visit':' · visit 未核对'),report,'回执版本 '+(report.source_hash||'未记录')+'；清理 '+label(report.cleanup_status)).versions=group.versions;
    edge(id,source,'固定来源指针');
  }
  const result=snap.result;
  if(result&&(result.summary||references(result).length||result.answers?.length)){
    add('report','report','研究报告','业务：'+label(result.status),result,result.summary||'报告已登记证据引用');
    for(const id of references(result))edge(evidenceNode(id),'report','报告引用');
    for(const a of result.answers||[])for(const id of references(a))edge(evidenceNode(id),'report','结论引用');
    edge('goal','report','运行报告');
  }
  const taskIDs=new Set(tasks.map(t=>t.id)),missingTaskIDs=[...new Set([...tasks.flatMap(t=>t.dependencies||[]),...progress.flatMap(p=>[p.task_id,...(p.task_ids||[])])].filter(id=>!taskIDs.has(id)))];
  const ids=new Set(nodes.map(n=>n.id)),available=new Set(evidence.map(e=>e.id));
  for(const id of new Set([...reportIDs,...explorationIDs]))if(!available.has(id))missing.add(id);
  for(const n of nodes.filter(n=>n.kind==='observation'))if(!edges.some(e=>e.to===n.id))edge('goal',n.id,reportIDs.has(n.record.id)?'报告证据':'资产引用');
  return {nodes,edges:edges.filter(e=>ids.has(e.from)&&ids.has(e.to)),reportReferenceCount:reportIDs.size,reportEvidenceCount:nodes.filter(n=>n.kind==='observation'&&reportIDs.has(n.record.id)).length,missingEvidenceCount:missing.size,missingTaskIDs};
}
export function explorationLayout(nodes,edges){
  const indegree=new Map(nodes.map(n=>[n.id,0])),depth=new Map(nodes.map(n=>[n.id,0])),out=new Map();
  for(const e of edges){indegree.set(e.to,indegree.get(e.to)+1);if(!out.has(e.from))out.set(e.from,[]);out.get(e.from).push(e.to)}
  const ready=nodes.filter(n=>!indegree.get(n.id)).map(n=>n.id);
  const placed=new Set();
  for(let i=0;placed.size<nodes.length;i++){
    // Break cycles for layout only; retain the original dependency edges.
    if(i===ready.length)ready.push(nodes.filter(n=>!placed.has(n.id)).reduce((a,b)=>indegree.get(a.id)<=indegree.get(b.id)?a:b).id);
    const current=ready[i];if(placed.has(current))continue;placed.add(current);
    for(const id of out.get(current)||[]){if(placed.has(id))continue;depth.set(id,Math.max(depth.get(id),depth.get(current)+1));indegree.set(id,indegree.get(id)-1);if(!indegree.get(id))ready.push(id)}
  }
  // Adapted from ARTEX e6ec569, exploration-graph.tsx computeLayout (AGPL-3.0):
  // four stable barycenter sweeps + centered columns. Retain every ledger edge,
  // including cycles, and use the existing Trace cycle-safe depth assignment.
  const columns=Array.from({length:Math.max(0,...depth.values())+1},()=>[]),neighbors=new Map(nodes.map(n=>[n.id,[]]));
  for(const n of nodes)columns[depth.get(n.id)].push(n.id);
  for(const e of edges)if(Math.abs(depth.get(e.from)-depth.get(e.to))===1){neighbors.get(e.from).push(e.to);neighbors.get(e.to).push(e.from)}
  for(let pass=0;pass<4;pass++)for(const direction of [1,-1]){
    for(let c=direction===1?1:columns.length-2;c>=0&&c<columns.length;c+=direction){
      const previous=new Map(columns[c-direction].map((id,i)=>[id,i]));
      columns[c]=columns[c].map((id,i)=>{const ranks=neighbors.get(id).filter(n=>previous.has(n)).map(n=>previous.get(n));return {id,i,rank:ranks.length?ranks.reduce((a,b)=>a+b,0)/ranks.length:i}}).sort((a,b)=>a.rank-b.rank||a.i-b.i).map(n=>n.id);
    }
  }
  const count=Math.max(1,...columns.map(c=>c.length)),positions=new Map();
  columns.forEach((ids,c)=>ids.forEach((id,i)=>positions.set(id,{x:24+c*340,y:24+((count-ids.length)/2+i)*164})));
  return {positions,width:(columns.length-1)*340+294,height:count*164+30};
}
// The horizontal section crosses only intervening columns (both end columns
// for a back edge). Prefer the nearest open row gap over a trip to the roof.
export function explorationRoutes(layout,edges,{nodeWidth,port,columnGap}){
  const {positions,height}=layout;
  const width=Math.max(layout.width,...Array.from(positions.values(),p=>p.x+nodeWidth+24));
  const nodeHeight=port*2+2,tracks=[]; // Both renderers put ports 1px above the card center.
  const pairs=new Map(),counts=new Map();
  for(const e of edges){const key=JSON.stringify([e.from,e.to]);counts.set(key,(counts.get(key)||0)+1)}
  const paths=edges.map(e=>{
    const a=positions.get(e.from),b=positions.get(e.to),key=JSON.stringify([e.from,e.to]);
    const i=pairs.get(key)||0; pairs.set(key,i+1);
    const count=counts.get(key),rank=i-(count-1)/2;
    const offset=rank*Math.min(20,2*port/count);
    const x=a.x+nodeWidth,y=a.y+port+offset,tx=b.x,ty=b.y+port+offset;
    const gutter=(columnGap-nodeWidth)/2;
    const out=Math.min(width-8,x+gutter),into=Math.max(8,tx-gutter);
    if(b.x>a.x&&b.x-a.x<=columnGap+.01){
      // Cairn 8e7e0ea, vendor/dagre.min.js: independent edge tracks, edgesep=20.
      // Vertical translation alone collapses the normal gap of steep Beziers.
      // Reverse track order down the slope so the port approaches do not cross.
      const lane=count>1?(x+tx)/2-rank*Math.min(20,(tx-x)/count)*(Math.sign(ty-y)||1):undefined;
      return {x,y,tx,ty,out,into,lane};
    }
    const left=Math.min(out,into),right=Math.max(out,into),blocked=[];
    for(const p of positions.values())if(p.x<right&&p.x+nodeWidth>left)blocked.push([p.y-10,p.y+nodeHeight+10]);
    for(const t of tracks)if(t.left<right&&t.right>left)blocked.push([t.y-18,t.y+18]);
    const bands=[];
    for(const interval of blocked.sort((u,v)=>u[0]-v[0])){
      const last=bands.at(-1);
      if(last&&interval[0]<last[1])last[1]=Math.max(last[1],interval[1]);else bands.push(interval);
    }
    const mid=(y+ty)/2,band=bands.find(([lo,hi])=>lo<mid&&mid<hi);
    const track=!band?mid:mid-band[0]<=band[1]-mid?band[0]:band[1];
    tracks.push({left,right,y:track});return {x,y,tx,ty,out,into,track};
  });
  const inset=Math.max(0,...tracks.map(t=>14-t.y));
  const placed=new Map([...positions].map(([id,p])=>[id,{x:p.x,y:p.y+inset}]));
  const routes=paths.map(({x,y,tx,ty,out,into,track,lane})=>{
    y+=inset;ty+=inset;
    if(lane!==undefined)return {d:`M${x} ${y} H${lane} V${ty} H${tx}`,x:lane,y:(y+ty)/2-6};
    if(track===undefined)return {d:`M${x} ${y} C${out} ${y},${into} ${ty},${tx} ${ty}`,x:(x+tx)/2,y:(y+ty)/2-6};
    track+=inset;return {d:`M${x} ${y} H${out} V${track} H${into} V${ty} H${tx}`,x:(out+into)/2,y:track-4,detour:true};
  });
  return {positions:placed,width,height:inset+Math.max(height,...tracks.map(t=>t.y+14)),routes};
}
export function explorationGeometry(run,nodes,edges,preview=false){
  // Geometry belongs only to the active Run, with one full and one preview layout.
  if(layoutCache?.run!==run)layoutCache={run};
  const mode=preview?'preview':'full',cached=layoutCache[mode];
  const edgeKey=e=>JSON.stringify([e.from,e.to,e.relation]);
  const orderedNodes=[...nodes].sort((a,b)=>a.id.localeCompare(b.id));
  const orderedEdges=[...edges].sort((a,b)=>edgeKey(a).localeCompare(edgeKey(b)));
  const topology=JSON.stringify([orderedNodes.map(n=>n.id),orderedEdges.map(edgeKey)]);
  if(cached?.topology!==topology){
    let layout=explorationLayout(orderedNodes,orderedEdges);
    if(preview)layout={positions:new Map([...layout.positions].map(([id,p])=>[id,{x:p.x*252/340,y:p.y*92/164}])),width:layout.width*252/340+12,height:layout.height*92/164+12};
    const geometry=explorationRoutes(layout,orderedEdges,preview?{nodeWidth:212,port:28,columnGap:252}:{nodeWidth:246,port:65,columnGap:340});
    layoutCache[mode]={topology,geometry,routes:new Map(orderedEdges.map((e,i)=>[edgeKey(e),geometry.routes[i]]))};
  }
  const {geometry,routes}=layoutCache[mode];
  return {...geometry,routes:edges.map(e=>routes.get(edgeKey(e)))};
}
// Traverse upstream and downstream separately: visiting a shared ancestor must
// not highlight unrelated siblings. A selection is presentation state only.
export function explorationChain(nodes,edges,selection){
  const ids=new Set(nodes.map(n=>n.id)),active=new Set(),linked=new Set();
  if(!selection||!ids.has(selection.from)||!ids.has(selection.to))return {active,linked};
  const nodeSelection=selection.from===selection.to;
  const exact=e=>e.from===selection.from&&e.to===selection.to&&e.relation===selection.relation;
  if(!nodeSelection&&!edges.some(exact))return {active,linked};
  const walk=(start,reverse)=>{
    const queue=[start],seen=new Set(queue);active.add(start);
    for(let i=0;i<queue.length;i++)edges.forEach((e,index)=>{
      if((reverse?e.to:e.from)!==queue[i])return;
      const next=reverse?e.from:e.to;
      if(!nodeSelection&&next===(reverse?selection.to:selection.from))return;
      linked.add(index);active.add(next);
      if(!seen.has(next)){seen.add(next);queue.push(next)}
    });
  };
  walk(selection.from,true);walk(selection.to,false);
  edges.forEach((e,i)=>{if(exact(e))linked.add(i)});
  return {active,linked};
}
export function explorationGraph(s){
  if(!s.snap?.run)return empty('尚未选择任务');
  snapshots.clear();snapshots.set(s.snap.run.id,s);
  const replay=replayToolbar(s);s={...s,snap:graphSnapshot(s)};
  const view=viewState(s.snap.run.id),all=explorationData(s.snap),query=queries.get(s.snap.run.id)||'',{nodes,edges}=explorationProjection(all,{...view,query}),{positions,width,height,routes}=explorationGeometry(s.snap.run.id,nodes,edges),progress=s.snap.plan?.work_progress||[];
  const matches=nodes.filter(n=>n.search.includes(query.toLowerCase()));
  return `<div class="exploration-view" data-exploration-run="${esc(s.snap.run.id)}">${replay}
    <p class="note mono">来源运行 ${esc(s.snap.run.id)} · ${all.nodes.filter(n=>n.kind==='intent').length} 个方向 · ${all.nodes.filter(n=>n.kind==='observation').length} 条关联观测${view.kind?' · 当前类型筛选：'+kinds[view.kind]:''}</p><div class="exploration-toolbar"><label>查找探索节点<input id="exploration-search" data-ephemeral="true" type="search" value="${esc(query)}" placeholder="目标、方向、证据或报告…"></label>${btn('exploration-locate','定位下一个')}<span id="exploration-match-count" class="note" role="status">${query?'匹配 '+matches.length+' 个节点':nodes.length+' / '+all.nodes.length+' 个节点 · '+edges.length+' 条关系'}</span></div>
    <div class="exploration-legend" aria-label="按节点类型查看">${btn('exploration-control','任务主线',`data-command="mainline" aria-pressed="${view.projection==='mainline'&&!view.kind}"`)}${btn('exploration-control','全部 · '+all.nodes.length,`data-command="kind" data-kind="" aria-pressed="${view.projection==='all'&&!view.kind}"`)}${view.expanded.size?btn('exploration-control','收起明细','data-command="collapse"'):''}${Object.entries(kinds).map(([k,v])=>btn('exploration-control',v+' · '+all.nodes.filter(n=>n.kind===k).length,`data-command="kind" data-kind="${k}" aria-pressed="${view.kind===k}"`)).join('')}</div>
    <div class="exploration-controls" aria-label="探索图视图控制">${btn('exploration-control','−','data-command="out" aria-label="缩小探索图"')}${btn('exploration-control','+','data-command="in" aria-label="放大探索图"')}<output class="exploration-zoom" aria-live="polite">${Math.round(view.zoom*100)}%</output>${btn('exploration-control','适应全图','data-command="fit"')}${btn('exploration-control','100%','data-command="actual"')}<span class="note">${query.trim()?'搜索覆盖全量节点 · ':view.kind?'所选类型、上游链路与直接下游 · ':view.projection==='mainline'?'任务主线 · 从节点详情展开来源、证据与历史 visit · ':''}拖动空白平移 · 右下角拖动尺寸 · 点击连线高亮 · 节点打开详情</span></div>
    <div class="exploration-selection" aria-live="polite"><span class="note">选择连线或节点查看上下游链路</span>${btn('exploration-control','清除高亮','data-command="clear"')}</div>
    <div class="graph-shell exploration-canvas" data-scroll="execution-lineage" tabindex="0" style="${view.size||''}" aria-label="探索图视口，可用方向键或滚动条移动，右下角拖动调整尺寸"><svg data-natural-width="${width}" data-natural-height="${height}" width="${width*view.zoom}" height="${height*view.zoom}" viewBox="0 0 ${width} ${height}" role="group" aria-label="研究目标、探索方向、观测与发现关系图"><defs><marker id="exploration-arrow" markerWidth="7" markerHeight="7" refX="6" refY="3" orient="auto"><path d="M0,0 L6,3 L0,6" fill="currentColor"/></marker></defs>
    ${edges.map((e,i)=>{const {d,x,y,detour}=routes[i];return `<g id="${esc('exploration-edge-'+s.snap.run.id+'-'+encodeURIComponent(JSON.stringify([e.from,e.to,e.relation])))}" class="exploration-edge ${e.from==='goal'?'membership':''} ${detour?'detour':''}" data-action="exploration-chain" data-from="${esc(e.from)}" data-to="${esc(e.to)}" data-relation="${esc(e.relation)}" role="button" tabindex="0" aria-label="高亮链路：${esc(e.relation)}" aria-pressed="false"><path class="edge-hit" d="${d}"/><path class="edge-line" d="${d}" marker-end="url(#exploration-arrow)"/><text x="${x}" y="${y}">${esc(e.relation)}</text><title>${esc(e.relation)}</title></g>`}).join('')}
    ${nodes.map(n=>{const p=positions.get(n.id),search=n.search;return `<foreignObject id="${esc('exploration-node-'+s.snap.run.id+'-'+encodeURIComponent(n.id))}" x="${p.x}" y="${p.y}" width="246" height="132"><button type="button" class="exploration-node ${n.kind} ${view.kind&&n.kind!==view.kind?'context-node':''} ${query&&search.includes(query.toLowerCase())?'search-match':''}" data-action="exploration-detail" data-id="${esc(n.id)}" data-exploration-search="${esc(search)}" title="${esc(n.title+'：'+n.summary)}"><span class="exploration-node-kind">${kinds[n.kind]}</span><span class="exploration-node-title">${esc(n.title)}</span><span class="exploration-node-summary">${esc(n.summary)}</span><span class="exploration-node-state">${esc(n.state)}</span></button></foreignObject>`}).join('')}</svg>${!nodes.length?empty('此类型暂无节点','选择“全部”查看当前探索；未记录的节点不会补造。'):''}</div>
    <p class="note">${workProgressSummary(progress)}</p>
    <p class="note">运行：${esc(label(s.snap.run.status))} · 阶段验收：按验收节点与工作项记录核对 · 资源释放：${esc(label(s.snap.result?.cleanup_status||s.snap.run.cleanup_status||'unknown'))} · 报告交付：${reportDownloads(s.snap,true)}。这些状态分别记录。</p>
    ${all.missingTaskIDs.length?`<p class="note">任务引用未载入：${all.missingTaskIDs.map(esc).join('、')}；未补造任务关系。</p>`:''}
    ${!all.reportEvidenceCount?`<p class="note exploration-empty-evidence">${all.reportReferenceCount?'报告已有 '+all.reportReferenceCount+' 条证据引用，当前没有可展示的执行观测；打开证据详情核对来源。':'尚无报告证据。已登记方向和明确关联的执行观测仍独立展示；未引用的工具记录可在运行对话查看。'}</p>`:''}${all.missingEvidenceCount?`<p class="note">${all.missingEvidenceCount} 条引用的证据未在当前快照中找到；保留报告原始引用供核对。</p>`:''}${!(s.snap.plan?.tasks||[]).length?'<p class="note">尚未登记探索方向；当前仅展示已有引用与验证记录。</p>':''}${!(s.snap.findings||[]).length?'<p class="note">尚无登记发现。工具输出和来源校验不会自动生成漏洞结论。</p>':''}<details class="exploration-reading"><summary>如何阅读这张图</summary><p class="note">实线表示已保存的方向依赖或证据引用，虚线表示本运行中的归属。图仅收录报告相关证据及方向、资产明确引用的观测；工具搜索、计划控制和状态查询保留在运行对话中。默认显示任务主线，来源、证据及历史 visit 可从节点详情展开；全量视图与搜索保留全部载入链路。按类型查看时保留上游链路与直接下游。来源完整、判据通过、独立审阅和业务成功分别以原记录为准，关系线不表示因果证明。</p></details></div>`;
}
function applyZoom(root,zoom,center=false){
  const canvas=root.querySelector('.exploration-canvas'),svg=canvas.querySelector('svg'),view=viewState(root.dataset.explorationRun),previous=view.zoom;
  const x=(canvas.scrollLeft+canvas.clientWidth/2)/previous,y=(canvas.scrollTop+canvas.clientHeight/2)/previous;
  view.zoom=Math.max(.08,Math.min(2,zoom));svg.setAttribute('width',Number(svg.dataset.naturalWidth)*view.zoom);svg.setAttribute('height',Number(svg.dataset.naturalHeight)*view.zoom);
  svg.style.width=Number(svg.dataset.naturalWidth)*view.zoom+'px';svg.style.height=Number(svg.dataset.naturalHeight)*view.zoom+'px';
  root.querySelector('.exploration-zoom').textContent=Math.round(view.zoom*100)+'%';
  if(center){canvas.scrollLeft=x*view.zoom-canvas.clientWidth/2;canvas.scrollTop=y*view.zoom-canvas.clientHeight/2}
}
export function mountExploration(){
  resizeObserver?.disconnect();
  const fits=new Map();
  for(const root of document.querySelectorAll('.exploration-view,.exploration-preview')){
    const canvas=root.querySelector('.graph-shell'),svg=canvas?.querySelector('svg');if(!svg)continue;
    const position=root.querySelector('#replay-position');if(position)position.value=position.getAttribute('value');
    const view=rootView(root),preview=!!root.dataset.explorationPreview;
    // Native resize is bounded by the column. Reserve first-drag headroom,
    // and restore geometry via CSSOM on pages that disallow inline attributes.
    canvas.style.width=view.width||canvas.style.width||'min(100%, max(320px, calc(100% - 96px)))';
    if(view.height)canvas.style.height=view.height;
    if(!mountedCanvases.has(canvas)){
      mountedCanvases.add(canvas);mountGraphPan(canvas);
      if(view.scroll){canvas.scrollLeft=view.scroll.left;canvas.scrollTop=view.scroll.top}
      canvas.addEventListener('scroll',()=>{view.scroll={left:canvas.scrollLeft,top:canvas.scrollTop}},{passive:true});
      let blank;
      canvas.addEventListener('pointerdown',e=>{blank=e.button===0&&e.target.matches('svg,.graph-shell')?[e.clientX,e.clientY]:null});
      canvas.addEventListener('pointermove',e=>{if(blank&&Math.hypot(e.clientX-blank[0],e.clientY-blank[1])>4)blank=null});
      canvas.addEventListener('click',e=>{
        if(blank&&e.target.matches('svg,.graph-shell'))clearExploration(canvas.closest('.exploration-view,.exploration-preview'));
        blank=null;
      });
      canvas.addEventListener('keydown',e=>{
        const edge=e.target.closest('[data-action="exploration-chain"]');
        if(edge&&['Enter',' '].includes(e.key)){e.preventDefault();selectExplorationChain(edge)}
        if(e.key==='Escape')clearExploration(canvas.closest('.exploration-view,.exploration-preview'));
      });
    }
    const fit=()=>{
      // Native resizing writes inline dimensions. Preserve those across live
      // reconciliation; no Store/API persistence or inferred graph mutations.
      view.width=canvas.style.width;view.height=canvas.style.height;
      if(preview||view.mode==='manual'||!canvas.clientWidth||!canvas.clientHeight)return;
      applyZoom(root,explorationScale(Number(svg.dataset.naturalWidth),Number(svg.dataset.naturalHeight),canvas.clientWidth,canvas.clientHeight,view.mode));if(view.mode==='initial')view.mode='manual';
    };
    if(!preview)applyZoom(root,view.zoom);fit();paintExplorationChain(root);fits.set(canvas,fit);
  }
  if(!fits.size){layoutCache=undefined;snapshots.clear()}
  if(typeof ResizeObserver!=='undefined'){resizeObserver=new ResizeObserver(entries=>entries.forEach(entry=>fits.get(entry.target)?.()));for(const canvas of fits.keys())resizeObserver.observe(canvas)}
}
function paintExplorationChain(root){
  const view=rootView(root),nodeElements=[...root.querySelectorAll('[data-exploration-search],[data-chain-node]')],edgeElements=[...root.querySelectorAll('[data-action="exploration-chain"]')];
  const nodes=nodeElements.map(n=>({id:n.dataset.id,title:n.dataset.chainTitle||n.querySelector('.exploration-node-title')?.textContent||n.getAttribute('aria-label')||n.dataset.id})),edges=edgeElements.map(e=>({from:e.dataset.from,to:e.dataset.to,relation:e.dataset.relation}));
  const picked=view.selection;
  if(picked&&(picked.from===picked.to?!nodes.some(n=>n.id===picked.from):!edges.some(e=>e.from===picked.from&&e.to===picked.to&&e.relation===picked.relation)))view.selection=null;
  const {active,linked}=explorationChain(nodes,edges,view.selection),selected=view.selection;
  nodeElements.forEach(n=>{n.classList.toggle('chain-active',active.has(n.dataset.id));n.classList.toggle('chain-muted',!!selected&&!active.has(n.dataset.id));n.classList.toggle('chain-selected',!!selected&&(selected.from===n.dataset.id||selected.to===n.dataset.id))});
  const titles=new Map(nodes.map(n=>[n.id,n.title]));
  edgeElements.forEach((e,i)=>{
    const {from,to,relation}=edges[i],exact=!!selected&&from===selected.from&&to===selected.to&&relation===selected.relation;
    e.classList.toggle('chain-active',linked.has(i));e.classList.toggle('chain-muted',!!selected&&!linked.has(i));e.classList.toggle('chain-selected',exact);
    e.setAttribute('aria-pressed',String(exact));e.setAttribute('aria-label',`${titles.get(from)} → ${titles.get(to)}：${relation||''}`);
  });
  const selection=root.querySelector('.exploration-selection');
  if(selection)selection.innerHTML=(selected?`<span class="note">已高亮 ${active.size} 个节点 · ${linked.size} 条关系</span>${[...new Set([selected.from,selected.to])].map(id=>btn('exploration-detail','查看 '+esc(nodes.find(n=>n.id===id).title),`data-id="${esc(id)}" ${root.dataset.explorationPreview?'data-current="true"':''}`)).join('')}`:'<span class="note">选择连线或节点查看上下游链路</span>')+btn('exploration-control','清除高亮',`data-command="clear" ${selected?'':'disabled'}`);
  if(selection&&selected?.relation)selection.firstElementChild.prepend(selected.relation+' · ');
}
export function selectExplorationChain(el){
  const root=el.closest('.exploration-view,.exploration-preview');if(!root)return;
  rootView(root).selection=el.dataset.action==='exploration-chain'?{from:el.dataset.from,to:el.dataset.to,relation:el.dataset.relation}:{from:el.dataset.id,to:el.dataset.id};paintExplorationChain(root);
}
function clearExploration(root){
  if(rootView(root).selection){rootView(root).selection=null;paintExplorationChain(root)}
}
function redrawExploration(root){
  const run=root.dataset.explorationRun,canvas=root.querySelector('.exploration-canvas'),view=rootView(root);
  view.scroll={left:canvas.scrollLeft,top:canvas.scrollTop};
  const holder=document.createElement('div');holder.innerHTML=explorationGraph(snapshots.get(run));
  const next=holder.firstElementChild;root.replaceWith(next);mountExploration();return next;
}
export function explorationControl(el){
  const root=el.closest('.exploration-view,.exploration-preview')||(el.dataset.run?[...document.querySelectorAll('.exploration-view')].find(n=>n.dataset.explorationRun===el.dataset.run):null);if(!root)return;
  const view=rootView(root),command=el.dataset.command;
  if(command==='clear'){clearExploration(root);return}
  if(['kind','mainline','expand','collapse'].includes(command)){
    if(command==='kind'){view.kind=el.dataset.kind;view.projection='all'}
    if(command==='mainline'){view.kind='';view.projection='mainline';view.expanded.clear()}
    if(command==='collapse'){view.kind='';view.projection='mainline';view.expanded.clear()}
    if(command==='expand'){view.kind='';view.projection='mainline';view.expanded.add(el.dataset.id)}
    matchIndex=0;
    const next=redrawExploration(root);
    [...next.querySelectorAll('[data-command]')].find(b=>b.dataset.command===command&&(command!=='kind'||b.dataset.kind===view.kind))?.focus({preventScroll:true});return;
  }
  const canvas=root.querySelector('.exploration-canvas'),svg=canvas.querySelector('svg');
  view.mode=command==='fit'?'fit':'manual';
  const zoom=command==='fit'?explorationScale(Number(svg.dataset.naturalWidth),Number(svg.dataset.naturalHeight),canvas.clientWidth,canvas.clientHeight):command==='actual'?1:view.zoom*(command==='in'?1.25:.8);
  applyZoom(root,zoom,true);if(command==='fit'){canvas.scrollLeft=0;canvas.scrollTop=0}
}
export function updateExplorationSearch(input){
  let root=input?.closest('.exploration-view');if(!root)return [];
  const query=input.value.trim().toLowerCase(),previous=queries.get(root.dataset.explorationRun)||'';queries.set(root.dataset.explorationRun,input.value);matchIndex=0;
  if(!!query!==!!previous.trim()&&root.replaceWith&&snapshots.has(root.dataset.explorationRun)){
    const focused=document.activeElement===input;
    root=redrawExploration(root);input=root.querySelector('#exploration-search');
    if(focused)input.focus({preventScroll:true});
  }
  const nodes=[...root.querySelectorAll('[data-exploration-search]')],matches=nodes.filter(n=>query&&n.dataset.explorationSearch.includes(query));
  nodes.forEach(n=>n.classList.toggle('search-match',matches.includes(n)));const count=root.querySelector('#exploration-match-count');if(count)count.textContent=query?`匹配 ${matches.length} 个节点`:`${nodes.length} 个节点 · 保留上下游关系`;return matches;
}
export function locateExploration(){
  const input=document.getElementById('exploration-search'),root=input?.closest('.exploration-view'),query=input?.value.trim().toLowerCase();
  const matches=[...(root?.querySelectorAll('[data-exploration-search]')||[])].filter(n=>query&&n.dataset.explorationSearch.includes(query));
  if(matches.length){const node=matches[matchIndex++%matches.length];for(let p=node.parentElement;p;p=p.parentElement)if(p.tagName==='DETAILS')p.open=true;const canvas=root.querySelector('.exploration-canvas');if(canvas?.scrollTo){const view=viewState(root.dataset.explorationRun);view.mode='manual';if(view.zoom<.85)applyZoom(root,.85);const bounds=node.getBoundingClientRect(),viewport=canvas.getBoundingClientRect();canvas.scrollTo({left:canvas.scrollLeft+bounds.left-viewport.left-(canvas.clientWidth-bounds.width)/2,top:canvas.scrollTop+bounds.top-viewport.top-(canvas.clientHeight-bounds.height)/2})}else node.scrollIntoView({block:'nearest',inline:'center'});node.focus({preventScroll:true})}return matches.length;
}
export function renderExplorationDetail(snap,id){
  const {nodes,edges}=explorationData(snap);let n=nodes.find(n=>n.id===id);
  if(!n&&(id.startsWith('child-report:')||id.startsWith('child-source:'))){
    const report=(snap.child_reports||[]).find(r=>id.slice(id.indexOf(':')+1)===childReportKey(r,true));
    if(report){n=explorationData({...snap,child_reports:[report]}).nodes.find(n=>n.kind===(id.startsWith('child-source:')?'source':'child_report'));n.id=id;n.versions=childReportGroups(snap.child_reports).find(g=>g.key===childReportKey(report)).versions}
  }
  if(!n)return historyDetail(snap,empty('引用版本未载入','保留原始引用；未以最新版本替换。'));
  if(n.kind==='observation')return historyDetail(snap,evidenceDetail(n.record,snap));
  const r=n.record;
  let detail='';
  if(n.kind==='child_report'||n.kind==='source'){
    const fixedPath=typeof r.report_path==='string'&&r.report_path.startsWith('/api/runs/'+encodeURIComponent(r.source_run_id)+'/export?')&&!r.report_path.includes('\\')&&new URLSearchParams(r.report_path.split('?')[1]).get('report_id');
    const versions=n.versions||[r],latest=versions.at(-1),recent=childReportKey(latest,true)===childReportKey(r,true);
    detail='<p>运行：'+esc(label(r.run_status))+' · 业务：'+esc(label(r.result_status))+' · 来源：'+esc(label(r.source_status))+' · 清理：'+esc(label(r.cleanup_status))+'</p><p class="note">'+(r.current===true?'当前 visit':r.current===false?'历史 visit':'visit 未核对')+' · '+(recent?'本组最近登记回执':'本组历史回执')+' · 报告交付 '+(r.delivery_pending?'待完成':'未标记待完成')+' · 生命周期结算 '+(r.settlement_pending?'待完成':'未标记待完成')+'；子报告不代表父目标成功。</p><pre>'+esc(r.source_hash)+'</pre><h4>登记发现</h4><pre>'+json(r.findings||[])+'</pre><h4>未决项</h4><pre>'+json(r.unresolved||[])+'</pre>'+btn('select','查看当前来源子运行 ↗','data-id="'+esc(r.source_run_id)+'"')+(fixedPath?'<a href="'+esc(r.report_path)+'" target="_blank" rel="noopener">读取固定版本源报告 ↗</a>':'<p class="note">未登记固定 report_id 归档；此回执正文之外的旧版报告未保存，查看当前来源不会替换此版本。</p>')+(versions.length>1?'<details><summary>此 visit 的回执版本 · '+versions.length+'</summary>'+versions.map(v=>'<p><code>'+esc(v.source_hash)+'</code> '+btn('exploration-detail','查看此回执详情','data-id="'+esc('child-report:'+childReportKey(v,true))+'"')+'</p>').join('')+'</details>':'');
  }
  if(n.kind==='child_report'||n.kind==='source')detail+=`<details><summary>回执原始记录</summary><pre>${json(r)}</pre></details>`;
  if(n.kind==='goal')detail=`<p>运行：${esc(label(r.status))} · 目标验收：见各验收记录 · 资源释放：${esc(label(snap.result?.cleanup_status||r.cleanup_status||'unknown'))}</p><details><summary>研究目标与用户原始输入</summary><pre>${json(r)}</pre></details>`;
  if(n.kind==='work')detail=`<dl class="key-values"><dt>原始任务</dt><dd>${esc(r.task_id)}</dd><dt>工作项</dt><dd>${esc(r.item_id)}</dd><dt>工作项状态</dt><dd>${esc(workStates[r.status]||r.status||'待核对')}</dd><dt>下一步</dt><dd>${esc(r.next_action||'未登记')}</dd><dt>原因</dt><dd>${esc(r.reason||'未登记')}</dd></dl><h4>关联阶段</h4>${[r.task_id,...(r.task_ids||[])].filter((id,i,ids)=>ids.indexOf(id)===i).map(id=>nodes.some(n=>n.id==='task:'+id)?btn('exploration-detail',esc(id),`data-id="${esc('task:'+id)}"`):`<p class="note">任务引用未载入：${esc(id)}</p>`).join('')}<h4>工作项引用证据</h4>${evidenceLinks(r.evidence_ids||[])}<p class="note">只读 Plan.work_progress 投影；运行完成、资源释放和报告交付分别核对。</p><details><summary>工作项原始记录</summary><pre>${json(r)}</pre></details>`;
  if(n.kind==='intent')detail=`${r.error?`<p class="inline-error">${esc(r.error)}</p>`:''}<p>完成条件：${esc(r.completion_criteria||r.success_contains||'未设置')}</p><div class="buttons">${btn('task-detail','查看计划记录',`data-id="${esc(r.id)}"`)}${r.child_run_id?btn('select','进入执行子运行 ↗',`data-id="${esc(r.child_run_id)}"`):''}</div><details><summary>原始计划记录（含 hint）</summary><pre>${json(r)}</pre></details>`;
  if(n.kind==='external')detail='<p class="note">原始来源尚未载入；内容完整性待核对。父目标验收资格以父运行绑定检查为准，原始证据可读不授予验收资格。</p>'+btn('evidence-detail','读取此来源证据',`data-id="${esc(r.id)}"`)+btn('select','查看当前证据来源运行 ↗',`data-id="${esc(r.run_id)}"`);
  if(n.kind==='requirement')detail=`<p>${esc(r.check?.reason||'尚无检查记录')}</p>${evidenceLinks(references(r.check))}${r.check?.review?`<p>独立审阅：${esc(r.check.review.reason)}</p>`:''}<details><summary>验收条件与检查原文</summary><pre>${json(r)}</pre></details>`;
  if(n.kind==='report')detail=`<p>${esc(r.summary||'尚无报告摘要')}</p><h4>报告引用证据</h4>${evidenceLinks(references(r))}${(r.answers||[]).map(a=>`<section><h4>报告结论 · ${a.verified?'引用校验通过':'待核对'}</h4><p>${esc(a.value)}</p>${evidenceLinks(references(a))}</section>`).join('')}<p class="note">业务状态：${esc(label(r.status))}；清理状态：${esc(label(r.cleanup_status))}。引用校验不等于语义结论已证明。</p>${reportDownloads(snap,true)}<details><summary>完整报告记录</summary><pre>${json(r)}</pre></details>`;
  if(n.kind==='finding')detail=`<dl class="key-values"><dt>影响</dt><dd>${esc(r.impact)}</dd><dt>根因</dt><dd>${esc(r.root_cause)}</dd><dt>修复建议</dt><dd>${esc(r.remediation)}</dd><dt>来源状态</dt><dd>${esc(label(r.source_status))}</dd></dl><h4>引用证据</h4>${evidenceLinks(references(r))}<h4>复现步骤</h4><ol>${(r.reproduction_steps||[]).map(v=>`<li>${esc(v)}</li>`).join('')}</ol>${r.review?`<p>独立审阅：${esc(r.review.reason)}</p>`:''}${(r.retests||[]).map(t=>`<p>复测：${esc(t.status)} · ${esc(t.reason)}</p>${evidenceLinks(references(t))}`).join('')}<p class="note">登记、审阅和复测是不同阶段；以各自记录为准。</p><details><summary>完整发现记录</summary><pre>${json(r)}</pre></details>`;
  const graphID=n.kind==='child_report'?'child-report:'+childReportKey(r):n.kind==='source'?'child-source:'+childReportKey(r):n.id;
  const linked=edges.filter(e=>e.from===graphID||e.to===graphID);
  const expandable=linked.length&&['goal','intent','work','requirement','finding','report','child_report','source'].includes(n.kind);
  const expand=expandable&&typeof document!=='undefined'&&[...document.querySelectorAll('.exploration-view')].some(root=>root.dataset.explorationRun===snap.run.id)?btn('exploration-control','在主线展开关联明细',`data-command="expand" data-run="${esc(snap.run.id)}" data-id="${esc(graphID)}"`):'';
  return historyDetail(snap,`<div class="exploration-detail"><p class="note">${kinds[n.kind]} · ${esc(n.state)}</p><h3>${esc(n.title)}</h3><p>${esc(n.summary)}</p>${n.original.title||n.original.summary?`<details><summary>查看节点原文</summary><pre>${esc([n.original.title,n.original.summary].filter(Boolean).join('\n'))}</pre></details>`:''}${detail}${expand}${linked.length?`<details><summary>关联节点 · ${linked.length}</summary><div class="exploration-relations">${linked.map(e=>{const other=nodes.find(x=>x.id===(e.from===graphID?e.to:e.from)),otherID=['child_report','source'].includes(n.kind)&&['child_report','source'].includes(other.kind)&&childReportKey(other.record)===childReportKey(r)?(other.kind==='source'?'child-source:':'child-report:')+childReportKey(r,true):other.id;return `<div><span class="note">${e.from===graphID?'→':'←'} ${esc(e.relation)}</span>${btn('exploration-detail',esc(other.title),`data-id="${esc(otherID)}"`)}</div>`}).join('')}</div></details>`:''}</div>`);
}
