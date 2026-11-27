import assert from 'node:assert/strict';
import {explorationPreview} from './static/graphs.js';
import {explorationData,explorationGraph,explorationSubset,explorationProjection,childReportKey,renderExplorationDetail,resolveEvidenceReference} from './static/exploration.js';

// Run only on the designated Ubuntu test host; no browser/runtime dependency.
const evidence=(id,request=id,extra={})=>({id,call_id:'call-'+id,request_id:request,type:'tool_execution',verification:'verified',content:JSON.stringify({stdout:'响应正文：'+id,exit_code:0}),...extra});
const action=(id,name='probe',adapter='local',request=id)=>({call_id:'call-'+id,request_id:request,name,adapter});
const snap={
  run:{id:'semantic-run',goal:'检查响应与报告引用',status:'completed'},
  actions:[action('search','tool_search','builtin'),action('state','worker_status','builtin'),action('plan','plan','builtin'),action('unused'),action('original'),action('retest'),action('latest'),action('old'),action('answer'),action('result'),action('asset'),action('direction')],
  evidence:['search','state','plan','unused','original','retest','latest','old','answer','result','asset','direction'].map(id=>evidence(id)),
  plan:{tasks:[{id:'prepare',goal:'准备比较',status:'completed'},{id:'compare',goal:'比较响应',status:'failed',dependencies:['prepare'],error:'对照响应未匹配',evidence_ids:['original','direction','plan']}]},
  assets:[{id:'endpoint',value:'/probe',source_task_id:'compare',evidence_ids:['asset']}],
  requirements:[{id:'response',description:'响应应满足检查判据'}],
  checks:[{requirement_id:'response',status:'pass',evidence_ids:['old']},{requirement_id:'response',status:'unknown',reason:'本次检查仍待核对',evidence_ids:['latest']}],
  findings:[{id:'observation',title:'响应差异',status:'needs_evidence',impact:'仍待复测',evidence_ids:['original'],retests:[{status:'fail',reason:'复测未通过',evidence_ids:['retest']}]}],
  result:{status:'incomplete',summary:'已记录响应，仍有未通过检查',evidence_ids:['result'],answers:[{value:'响应结论仍待验证',verified:false,evidence_ids:['answer']}]}
};
const graph=explorationData(snap),has=id=>graph.nodes.some(n=>n.id==='evidence:'+id);
for(const id of ['search','state','plan','unused','old'])assert.equal(has(id),false,id+' is activity or superseded evidence, not a current report fact');
for(const id of ['original','retest','latest','answer','result','asset','direction'])assert.equal(has(id),true,id+' has an explicit current reference');
assert.equal(graph.reportEvidenceCount,5);
assert.ok(graph.edges.some(e=>e.from==='evidence:answer'&&e.to==='report'&&e.relation==='结论引用'));
assert.ok(graph.edges.some(e=>e.from==='evidence:result'&&e.to==='report'&&e.relation==='报告引用'));
assert.ok(graph.edges.some(e=>e.from==='evidence:retest'&&e.to==='finding:observation'&&e.relation==='复测引用'));
assert.ok(graph.edges.some(e=>e.from==='task:compare'&&e.to==='evidence:asset'));
assert.equal(graph.nodes.find(n=>n.id==='requirement:response').state,'待核对');
assert.equal(graph.nodes.find(n=>n.id==='finding:observation').state,'待补证据');
assert.equal(graph.nodes.find(n=>n.id==='goal').state,'运行：已完成');
assert.equal(graph.nodes.find(n=>n.id==='report').state,'业务：未完成','run completion does not imply business success');
assert.ok(graph.nodes.every(n=>typeof n.title==='string'&&typeof n.summary==='string'));
assert.match(explorationGraph({snap}),/exploration-node-summary/);
assert.match(renderExplorationDetail(snap,'report'),/报告引用证据/);
assert.doesNotMatch(renderExplorationDetail(snap,'report'),/data-format="report"/,'a business result alone is not a saved report file');
const archived={...snap,reports:[{id:'semantic-v1',kind:'final',subject_id:'result',version:1,source_status:'current',files:[{name:'summary.zh-CN.md'}]}]};
const archivedDetail=renderExplorationDetail(archived,'report');
assert.match(archivedDetail,/data-format="report"/);
assert.match(archivedDetail,/data-run="semantic-run"/);
assert.match(archivedDetail,/data-report-id="semantic-v1"/);
assert.match(archivedDetail,/data-language="zh-CN"/);
assert.match(renderExplorationDetail(snap,'evidence:original'),/来源与完整原文/);

const filtered=explorationSubset(graph,'finding');
for(const id of ['goal','task:prepare','task:compare','evidence:original','evidence:retest','finding:observation'])assert.ok(filtered.nodes.some(n=>n.id===id),'keeps full ancestor chain '+id);
assert.equal(filtered.nodes.some(n=>n.id==='evidence:unused'),false);

// Same call ID in different requests must retain the observation's own adapter.
const duplicate={run:snap.run,evidence:[evidence('same','probe-request')],actions:[action('same','plan','builtin','control-request'),action('same','probe','local','probe-request')],result:{status:'incomplete',evidence_ids:['same']}};
assert.ok(explorationData(duplicate).nodes.some(n=>n.id==='evidence:same'));
const controlled={...duplicate,evidence:[evidence('same','control-request')]};
assert.equal(explorationData(controlled).nodes.some(n=>n.id==='evidence:same'),false,'report reference alone does not promote control output');
assert.match(renderExplorationDetail(controlled,'report'),/data-id="same"/,'report preserves original control citation for inspection');
assert.match(explorationGraph({snap:controlled}),/报告已有 1 条证据引用/);
assert.doesNotMatch(explorationGraph({snap:controlled}),/尚无报告证据/);

// A plain transcript with no references is explicitly empty, never filled by tools.
const empty={run:snap.run,evidence:snap.evidence,actions:snap.actions};
assert.deepEqual(explorationData(empty).nodes.map(n=>n.id),['goal']);
assert.match(explorationGraph({snap:empty}),/尚无报告证据/);
const missing={...empty,result:{status:'incomplete',evidence_ids:['deleted']}};
assert.equal(explorationData(missing).missingEvidenceCount,1);
assert.equal(explorationData(missing).nodes.some(n=>n.id==='evidence:deleted'),false);
assert.match(explorationGraph({snap:missing}),/1 条引用的证据未在当前快照中找到/);

// A persisted business result is not an archived report or a delivery receipt.
{
  const pending={run:{id:'pending-delivery',status:'running'},result:{status:'incomplete'}};
  const pendingHTML=explorationGraph({snap:pending});
  assert.match(pendingHTML,/报告交付：<span class="note">最终报告未归档<\/span>/);
  assert.doesNotMatch(pendingHTML,/已登记报告|data-report-id=/);
  const files=[{name:'final.zh-CN.md'},{name:'final.en.md'}];
  for(const [source_status,state] of [['not_checked','归档状态待核对'],['changed','仅有历史归档']]){
    const html=explorationGraph({snap:{...pending,reports:[{id:'old',kind:'final',source_status,files}]}});
    assert.ok(html.includes(state),source_status);
    assert.doesNotMatch(html,/已登记报告|data-report-id=/);
  }
  const partial={id:'current',kind:'final',version:2,source_status:'current',translation_status:'translation_missing',files:[files[0]],missing_attachments:['capture']};
  const partialHTML=explorationGraph({snap:{...pending,reports:[partial]}});
  for(const text of ['最终报告 v2','待补译','截图附件缺失 1 项','English 文件未归档','data-report-id="current"'])assert.ok(partialHTML.includes(text),text);
  assert.doesNotMatch(partialHTML,/data-language="en"/);
  const deliveredHTML=explorationGraph({snap:{...pending,reports:[{...partial,version:3,translation_status:'complete',missing_attachments:[],files}]}});
  for(const text of ['最终报告 v3','data-report-id="current"','data-language="zh-CN"','data-language="en"'])assert.ok(deliveredHTML.includes(text),text);
  assert.doesNotMatch(deliveredHTML,/已登记报告|待补译|截图附件缺失|文件未归档/);
}

// Evidence IDs in child runs are scoped to the run; no local same-ID collision.
const external={...empty,plan:{tasks:[{id:'child',goal:'子运行核验',status:'completed',evidence_run_id:'child-run',evidence_ids:['original']}]}};
assert.equal(explorationData(external).nodes.some(n=>n.id==='evidence:original'),false);
assert.ok(explorationData(external).nodes.some(n=>n.id==='external:child-run:original'));
assert.match(renderExplorationDetail(external,'external:child-run:original'),/data-id="child-run"/);

// Cyclic persisted dependencies remain reviewable and cannot stall filtering/layout.
const cyclic={...snap,plan:{tasks:[{id:'a',goal:'方向甲',status:'failed',dependencies:['b'],evidence_ids:['original']},{id:'b',goal:'方向乙',status:'pending',dependencies:['a']}]}};
const cycleGraph=explorationData(cyclic),cycleView=explorationSubset(cycleGraph,'finding');
assert.ok(cycleView.nodes.some(n=>n.id==='task:a')&&cycleView.nodes.some(n=>n.id==='task:b'));
assert.doesNotMatch(explorationGraph({snap:cyclic}),/NaN|Infinity/);
assert.equal(new Set(cycleGraph.edges.map(e=>[e.from,e.to,e.relation].join('\0'))).size,cycleGraph.edges.length);
assert.ok(cycleGraph.edges.every(e=>cycleGraph.nodes.some(n=>n.id===e.from)&&cycleGraph.nodes.some(n=>n.id===e.to)));
const refsOnly={run:snap.run,actions:[action('ref-only'),action('removed-check')],evidence:[evidence('ref-only'),evidence('removed-check')],findings:[{id:'ref-finding',title:'来源引用发现',status:'invalid',references:[{id:'ref-only'}],retests:[{status:'fail',references:[{id:'ref-only'}]}]}],requirements:[{id:'current',description:'当前验收'}],checks:[{requirement_id:'current',status:'unknown',references:[{id:'ref-only'}]},{requirement_id:'removed',status:'pass',evidence_ids:['removed-check']}]};
const refsGraph=explorationData(refsOnly);
assert.equal(refsGraph.reportEvidenceCount,1);
assert.equal(refsGraph.nodes.some(n=>n.id==='evidence:removed-check'),false);
for(const relation of ['发现引用','复测引用','检查引用'])assert.ok(refsGraph.edges.some(e=>e.from==='evidence:ref-only'&&e.relation===relation));
const hiddenRefs={...refsOnly,actions:[]};
assert.match(renderExplorationDetail(hiddenRefs,'finding:ref-finding'),/data-action="evidence-detail"[^>]*data-id="ref-only"/);
assert.match(renderExplorationDetail(hiddenRefs,'requirement:current'),/data-action="evidence-detail"[^>]*data-id="ref-only"/);
const english={...snap,run:{...snap.run,goal:'English research goal'},plan:{tasks:[{id:'en-task',goal:'English plan',status:'pending'}]},findings:[{id:'en-finding',title:'English finding',impact:'English impact',status:'invalid',evidence_ids:['original']}],requirements:[{id:'en-check',description:'English check'}],result:{status:'incomplete',summary:'English report',evidence_ids:['result']}};
const englishGraph=explorationData(english);
assert.ok(englishGraph.nodes.every(n=>/\p{Script=Han}/u.test(n.title)&&/\p{Script=Han}/u.test(n.summary)));
assert.match(renderExplorationDetail(english,'task:en-task'),/English plan/);
assert.match(renderExplorationDetail(english,'goal'),/English research goal/);
assert.match(englishGraph.nodes.find(n=>n.id==='finding:en-finding').search,/english finding/);
{
// Qualified references retain one source identity across every graph consumer.
const qualified={run:{id:'parent',status:'running'},actions:[action('same')],evidence:[evidence('same')],plan:{tasks:[{id:'cross',goal:'跨运行方向',status:'running',evidence_run_id:'child',evidence_ids:['same','child/same']}]},requirements:[{id:'qualified',description:'来源检查'}],checks:[{requirement_id:'qualified',status:'unknown',references:[{id:'child/same',hash:'raw-content',source_hash:'parent-binding'}]}],findings:[{id:'qualified',title:'跨运行发现',evidence_ids:['child/same'],retests:[{references:[{id:'child/same'}]}]}],result:{status:'incomplete',references:[{id:'parent/same'}],answers:[{evidence_ids:['child/same']} ]}};
const qualifiedBefore=JSON.stringify(qualified),qualifiedGraph=explorationData(qualified),external='external:child:same';
assert.equal(qualifiedGraph.nodes.filter(n=>n.id===external).length,1);
assert.ok(qualifiedGraph.nodes.some(n=>n.id==='evidence:same'),'qualified local evidence resolves in its owning Run');
for(const [from,to,relation] of [['task:cross',external,'方向引用'],[external,'requirement:qualified','检查引用'],[external,'finding:qualified','发现引用'],[external,'finding:qualified','复测引用'],[external,'report','结论引用'],['evidence:same','report','报告引用']])assert.ok(qualifiedGraph.edges.some(e=>e.from===from&&e.to===to&&e.relation===relation),relation+' keeps the correct source endpoint');
assert.equal(qualifiedGraph.reportReferenceCount,2);assert.equal(qualifiedGraph.reportEvidenceCount,1);
assert.ok(renderExplorationDetail(qualified,external).includes('data-id="child/same"'));
assert.match(renderExplorationDetail(qualified,'requirement:qualified'),/parent-binding/);
assert.deepEqual(resolveEvidenceReference(qualified,{id:'child/same',hash:'raw',source_hash:'binding'}),{run_id:'child',evidence_id:'same',external:true,id:'child/same',node_id:external});
assert.equal(resolveEvidenceReference(qualified,'same','child').node_id,external);
assert.equal(JSON.stringify(qualified),qualifiedBefore,'projection neither copies child evidence nor changes parent acceptance');
}

{
const receipt={source_run_id:'child',task_id:'compare',worker_id:'worker',visit:2,source_hash:'ready',current:true,run_status:'completed',result_status:'incomplete',source_status:'verified',cleanup_status:'completed',summary:'当前阶段保留剩余工作',delivery_pending:true};
const history={...receipt,visit:1,source_hash:'prior',current:false,summary:'历史访问回执'};
const work=[{task_id:'compare',item_id:'response',goal:'核对本次响应',next_action:'读取剩余对象',status:'pending',task_ids:['prepare','not-loaded'],evidence_ids:['original','child/raw'],reason:'覆盖尚不完整'},{task_id:'compare',item_id:'accepted',goal:'已验收部分',next_action:'等待其他工作项',status:'verified',task_ids:[],evidence_ids:['latest'],reason:'本项证据通过'},{task_id:'compare',item_id:'blocked',goal:'受阻的声明范围',next_action:'等待所需能力',status:'blocked',task_ids:[],evidence_ids:[],reason:'已记录能力缺口'},{task_id:'compare',item_id:'abandoned',goal:'已放弃的声明范围',next_action:'保留显式处置',status:'abandoned',task_ids:[],evidence_ids:[],reason:'已记录放弃原因'}];
const projected={...snap,run:{...snap.run,id:'projection-run',hint:'保留用户 hint <原文>'},plan:{...snap.plan,work_progress:work,tasks:snap.plan.tasks.map(t=>({...t,hint:'阶段 hint <原文>'}))},child_reports:[history,receipt,receipt]};
const before=JSON.stringify(projected),all=explorationData(projected),mainline=explorationProjection(all),ids=new Set(mainline.nodes.map(n=>n.id));
assert.equal(all.nodes.filter(n=>n.kind==='child_report').length,2,'duplicate receipt does not create a new visit');
for(const kind of ['observation','external','source'])assert.equal(mainline.nodes.some(n=>n.kind===kind),false,kind+' is disclosed on demand');
const latest=all.nodes.find(n=>n.kind==='child_report'&&n.record.visit===2),old=all.nodes.find(n=>n.kind==='child_report'&&n.record.visit===1);
assert.ok(ids.has(latest.id));assert.equal(ids.has(old.id),false,'historical visits stay out of the default task line');
assert.equal(latest.id,'child-report:'+childReportKey(receipt));
assert.equal(explorationData({...projected,child_reports:[{...receipt,source_hash:'settled'}]}).nodes.find(n=>n.kind==='child_report').id,latest.id,'same visit retains its graph identity across report revisions');
const expanded=explorationProjection(all,{expanded:new Set(['task:compare'])});
for(const id of [old.id,latest.id,'child-source:'+childReportKey(history),'child-source:'+childReportKey(receipt),'evidence:original'])assert.ok(expanded.nodes.some(n=>n.id===id),'task disclosure retains '+id);
assert.equal(explorationProjection(all,{expanded:new Set(['task:prepare'])}).nodes.some(n=>n.id===old.id),false,'expanding another task does not expose unrelated history');
assert.deepEqual(explorationProjection(all),mainline,'expansion is presentation state, not a mutation of all graph data');
assert.equal(explorationProjection(all,{projection:'all'}),all);
assert.equal(explorationProjection(all,{query:'历史访问'}),all,'search includes folded visits and original text');
assert.deepEqual(explorationProjection(all,{kind:'source'}),explorationSubset(all,'source'),'existing type and ancestor filtering remains available');
assert.ok(mainline.nodes.some(n=>n.kind==='work'&&n.record.status==='pending'));
assert.ok(mainline.nodes.some(n=>n.kind==='work'&&n.record.status==='verified'));
assert.ok(all.missingTaskIDs.includes('not-loaded'));
assert.equal(all.edges.some(e=>e.from==='task:not-loaded'||e.to==='task:not-loaded'),false,'unloaded task references never become invented edges');
const workID='work:'+JSON.stringify(['compare','response']),detail=renderExplorationDetail(projected,workID),html=explorationGraph({snap:projected});
for(const text of ['核对本次响应','读取剩余对象','覆盖尚不完整','not-loaded','child/raw','只读 Plan.work_progress'])assert.ok(detail.includes(text),text);
const coverage='工作项/范围覆盖 4 项 · 验收通过 1 项 · 剩余/待核对 1 项 · 受阻 1 项 · 已放弃 1 项';
assert.ok(html.includes(coverage),'finite declarations and remaining work share distinct outcome counts');
assert.ok(explorationPreview(projected).includes(coverage),'preview and full graph keep the same coverage semantics');
assert.equal(all.nodes.find(n=>n.kind==='work'&&n.record.status==='verified').state,'工作项：验收通过');
for(const status of ['blocked','abandoned']){
  const node=all.nodes.find(n=>n.kind==='work'&&n.record.status===status);
  assert.match(node.state,/显式处置/);assert.doesNotMatch(node.state,/验收通过|剩余工作/);
  assert.ok(renderExplorationDetail(projected,node.id).includes(work.find(p=>p.status===status).reason));
}
assert.ok(renderExplorationDetail(projected,workID).includes('工作项/范围覆盖'));
assert.doesNotMatch(html,/剩余工作：|剩余工作 ·/);
assert.match(html,/data-command="mainline" aria-pressed="true"/);
assert.doesNotMatch(html,/data-exploration-search="evidence:/);
assert.match(renderExplorationDetail(projected,'task:compare'),/阶段 hint &lt;原文&gt;/);
assert.match(renderExplorationDetail(projected,'goal'),/保留用户 hint &lt;原文&gt;/);
const versions={...projected,child_reports:[history,receipt,{...receipt,source_hash:'settled',summary:'新回执正文',delivery_pending:false}]};
const pinned='child-report:'+childReportKey(receipt,true);
assert.match(renderExplorationDetail(versions,pinned),/当前阶段保留剩余工作/);
const pinnedSource='child-source:'+childReportKey(receipt,true);
assert.ok(renderExplorationDetail(versions,pinned).includes('data-id="'+pinnedSource.replaceAll('"','&quot;')+'"'),'a pinned historical receipt links its exact source version');
assert.ok(renderExplorationDetail(versions,pinnedSource).includes('data-id="'+pinned.replaceAll('"','&quot;')+'"'),'a pinned source links back to the same receipt hash');
assert.doesNotMatch(renderExplorationDetail(versions,pinned),/新回执正文/);
assert.match(renderExplorationDetail({...versions,child_reports:versions.child_reports.filter(r=>r.source_hash!=='ready')},pinned),/引用版本未载入/);
assert.match(renderExplorationDetail(projected,latest.id),/报告交付 待完成/);
assert.match(renderExplorationDetail(projected,latest.id),/业务：未完成/);
assert.match(renderExplorationDetail(projected,latest.id),/清理：已完成/);
assert.equal(JSON.stringify(projected),before,'projection cannot change acceptance, remaining work, hints or source records');
assert.ok(explorationGraph({snap:{run:{id:'unknown-scope',status:'completed'}}}).includes('工作项/范围覆盖未知'));
}
console.log('Semantic exploration: report citations, latest checks, retests, control exclusion, ancestor filtering, scoped calls, missing evidence and cycles passed.');
