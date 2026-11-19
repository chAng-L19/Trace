import assert from 'node:assert/strict';
import {explorationData,explorationGraph,explorationSubset,renderExplorationDetail} from './static/exploration.js';

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
assert.equal(graph.nodes.find(n=>n.id==='goal').state,'已完成');
assert.equal(graph.nodes.find(n=>n.id==='report').state,'未完成','run completion does not imply business success');
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
console.log('Semantic exploration: report citations, latest checks, retests, control exclusion, ancestor filtering, scoped calls, missing evidence and cycles passed.');
