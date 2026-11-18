// Muteki 144df7666dc78ef331e7746357d31c9e0f57c558, EvidenceChain.tsx
// (AGPL-3.0): concise presentation with original text disclosure.
// ARTEX e6ec56999175509551a1e43a3ee3b053d0941bd9, db/exploration.go
// (AGPL-3.0): activity and explicitly cited findings are distinct records.
// Independent projection of Trace's report_evidence.go; no new evidence store.
const compact=value=>String(value??'').replace(/\s+/g,' ').trim();
export function evidenceText(raw,fallback,limit=180){
  let value=compact(raw);if(!/\p{Script=Han}/u.test(value))value=fallback;
  const chars=Array.from(value);if(chars.length>limit)value=chars.slice(0,limit).join('')+'…';
  return {value,original:value!==String(raw??'')?String(raw??''):''};
}
const sameRun=(e,snap)=>!(e.run_id&&snap.run?.id&&e.run_id!==snap.run.id)&&!(snap.catalogRun&&snap.run?.id&&snap.catalogRun!==snap.run.id)&&!(e.run_id&&snap.catalogRun&&e.run_id!==snap.catalogRun);
export function evidenceSourceAction(e,snap={}){
  if(!sameRun(e,snap)||!e.call_id||!e.request_id)return undefined;
  const matches=(snap.actions||[]).filter(a=>a.call_id===e.call_id&&a.request_id===e.request_id);
  return matches.length===1?matches[0]:undefined;
}
const cited=(record,id)=>(record?.evidence_ids||[]).includes(id)||(record?.references||[]).some(ref=>ref.id===id);
function snapshotLinks(e,snap){
  const links=[],seen=new Set();
  const add=(record,kind,id,title,summary,fallbackTitle,fallbackSummary,status)=>{
    if(!cited(record,e.id))return;
    const key=kind+'\0'+id;if(seen.has(key))return;seen.add(key);
    const t=evidenceText(title,fallbackTitle,80),s=evidenceText(summary,fallbackSummary,180);
    links.push({kind,id,title:t.value,summary:s.value,status,original_title:t.original,original_summary:s.original});
  };
  const result=snap.result||{};
  add(result,'result','result','',result.summary,'研究结论','此记录被研究结论直接引用；引用关系不代表验证通过。',result.status);
  for(const [i,a] of (result.answers||[]).entries())add(a,'answer','answer:'+(i+1),a.title||a.kind,a.summary||a.value,a.kind==='flag'?'目标结果标记':'结论条目 '+(i+1),evidenceText(result.summary,'此记录用于支撑报告中的一个结论条目，原文可在详情查看。',180).value,a.verified?'verified':'unverified');
  for(const f of snap.findings||[]){
    add(f,'finding',f.id,f.title,f.root_cause,'发现：'+f.id,'此记录关联发现的原始观察；审阅与复测状态分别记录。',f.status);
    for(const [i,r] of (f.retests||[]).entries())add(r,'retest',f.id+':retest:'+(i+1),'',r.reason,'发现复测：'+f.id+' · '+(i+1),'此记录关联一次实际复测，保留当次结果及失败原因。',r.status);
  }
  const latest=new Map();for(const c of snap.checks||[])latest.set(c.requirement_id,c);
  for(const r of snap.requirements||[]){const c=latest.get(r.id);if(c)add(c,'check',r.id,r.description,c.reason,'验收检查：'+r.id,'此记录关联该验收项的最近一次检查，检查状态以当前记录为准。',c.status)}
  return links;
}
const linkNames={result:'研究结论',answer:'结论条目',finding:'登记发现',retest:'发现复测',check:'验收检查'};
function normalizedLinks(links){
  const seen=new Set();return links.filter(link=>{
    const key=link.kind+'\0'+link.id;if(!linkNames[link.kind]||seen.has(key))return false;seen.add(key);return true;
  }).map(link=>{
    const t=evidenceText(link.title,linkNames[link.kind],80),s=evidenceText(link.summary,'此记录由报告中的'+linkNames[link.kind]+'明确引用，原文与状态可在详情核对。',180);
    return {...link,title:t.value,summary:s.value,original_title:link.original_title||t.original,original_summary:link.original_summary||s.original};
  });
}
export function evidencePresentation(e,snap={}){
  const scoped=sameRun(e,snap),action=evidenceSourceAction(e,snap);
  // Mirror internal/agent/verification.go observationAction. Tool names alone
  // never promote bookkeeping, missing sources or ambiguous calls to facts.
  const observation=!!action&&(['exec','local','mcp'].includes(action.adapter)||action.adapter==='builtin'&&['read','artifact_read'].includes(action.name));
  let links=[];
  if(scoped){
    if(Array.isArray(snap.reportLinks))links=snap.reportLinks;
    else if(Array.isArray(e.report_links))links=e.report_links;
    else if(!snap.catalogRun)links=snapshotLinks(e,snap);
  }
  links=normalizedLinks(links);
  const rank={answer:0,finding:1,retest:2,check:3,result:4};
  const primary=[...links].sort((a,b)=>rank[a.kind]-rank[b.kind])[0];
  let title=primary?.title|| (observation?'执行观测':'过程记录'),summary=primary?.summary||'尚未被报告、结论、发现或当前验收检查引用。';
  let original_title=primary?.original_title||'',original_summary=primary?.original_summary||'';
  if(!primary&&scoped){
    const task=(snap.plan?.tasks||[]).find(t=>(!t.evidence_run_id||t.evidence_run_id===snap.run?.id)&&cited(t,e.id));
    const asset=(snap.assets||[]).find(a=>(!a.source_run_id||a.source_run_id===snap.run?.id)&&cited(a,e.id));
    if(task){const t=evidenceText(task.goal,'探索方向引用证据',80);title=t.value;original_title=t.original;summary='此记录被探索方向引用；尚未关联报告结论。'}
    else if(asset){title='资产观察记录';const s=evidenceText(asset.value,'此记录被资产引用；尚未关联报告结论。',180);summary=s.value;original_summary=s.original}
  }
  return {title,summary,original:{title:original_title,summary:original_summary},original_title,original_summary,links,reportRelated:links.length>0,graphEligible:observation,observation,action};
}
