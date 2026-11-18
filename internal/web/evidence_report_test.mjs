import assert from 'node:assert/strict';
import {evidencePresentation,evidenceSourceAction} from './static/evidence-report.js';

// Execute only on the designated Ubuntu test host.
const e={id:'shared',call_id:'same-call',request_id:'probe',type:'tool_execution',verification:'invalid',content:'raw payload'};
const action={call_id:'same-call',request_id:'probe',adapter:'local',name:'probe'};
const snap={run:{id:'one'},actions:[{...action,request_id:'control',adapter:'builtin',name:'plan'},action],result:{status:'failed',summary:'English result',evidence_ids:['shared'],answers:[{kind:'answer',title:'目标响应差异',summary:'响应中出现已声明的差异。',value:'raw value',verified:false,evidence_ids:['shared']}]},findings:[{id:'f',title:'English finding',root_cause:'English cause',status:'invalid',references:[{id:'shared'}],retests:[{status:'fail',reason:'negative result',references:[{id:'retest'}]}]}],requirements:[{id:'check',description:'检查响应字段'}],checks:[{requirement_id:'check',status:'pass',evidence_ids:['old']},{requirement_id:'removed',status:'pass',evidence_ids:['removed']},{requirement_id:'check',status:'unknown',reason:'响应仍待核对',evidence_ids:['shared']}],plan:{tasks:[{id:'task',goal:'方向证据',evidence_ids:['plan-only']}]},assets:[{id:'asset',value:'资产证据',evidence_ids:['asset-only']}]};
const before=JSON.stringify(snap),p=evidencePresentation(e,snap);
assert.equal(p.title,'目标响应差异');assert.equal(p.summary,'响应中出现已声明的差异。');
assert.equal(p.reportRelated,true);assert.equal(p.graphEligible,true);assert.equal(p.action,action);
assert.deepEqual(p.links.map(l=>l.kind+'/'+l.id+'/'+l.status),['result/result/failed','answer/answer:1/unverified','finding/f/invalid','check/check/unknown']);
assert.equal(p.links[0].original_summary,'English result');assert.equal(p.links[2].original_title,'English finding');
assert.equal(JSON.stringify(snap),before,'presentation never mutates source or verification state');
for(const id of ['old','removed','plan-only','asset-only','absent'])assert.equal(evidencePresentation({...e,id},snap).reportRelated,false,id);
const retest=evidencePresentation({...e,id:'retest'},snap);assert.equal(retest.links.length,1);assert.equal(retest.links[0].kind,'retest');assert.equal(retest.links[0].status,'fail');
assert.equal(evidencePresentation({...e,request_id:'control'},snap).graphEligible,false,'cited control remains activity');
assert.equal(evidencePresentation(e,{...snap,actions:[action,{...action}]}).graphEligible,false,'ambiguous source is not a graph observation');
assert.equal(evidencePresentation({...e,request_id:''},snap).graphEligible,false,'missing request ID does not join a call from another request');
assert.equal(evidencePresentation(e,{...snap,actions:[]}).graphEligible,false);
assert.equal(evidenceSourceAction(e,snap),action);
for(const name of ['read','artifact_read'])assert.equal(evidencePresentation(e,{...snap,actions:[{...action,adapter:'builtin',name}]}).graphEligible,true);
for(const name of ['plan','tool_search','worker_status'])assert.equal(evidencePresentation(e,{...snap,actions:[{...action,adapter:'builtin',name}]}).graphEligible,false);

const english=evidencePresentation(e,{...snap,result:{...snap.result,answers:[{kind:'answer',value:'English answer',evidence_ids:['shared']}]}});
assert.equal(english.title,'结论条目 1');assert.equal(english.original.summary,'English answer');assert.match(english.summary,/结论条目/);
const long='证'.repeat(200),longer=evidencePresentation(e,{...snap,result:{answers:[{title:long,summary:long,evidence_ids:['shared']}]}});
assert.equal(Array.from(longer.title).length,81);assert.equal(Array.from(longer.summary).length,181);assert.equal(longer.original.title,long);

// The catalog's provided report links, including an empty list, are authoritative.
const links=[{kind:'finding',id:'catalog-f',title:'已保留的发现',summary:'来源会话删除后保留引用。',status:'needs_evidence'}];
const catalog={catalogRun:'one',run:{id:'one'},actions:[action],reportLinks:links};
assert.equal(evidencePresentation(e,catalog).title,'已保留的发现');
assert.equal(evidencePresentation(e,{...snap,reportLinks:[]}).reportRelated,false);
assert.equal(evidencePresentation({...e,report_links:links},snap).title,'已保留的发现');
assert.equal(evidencePresentation(e,{...catalog,run:{id:'other'}}).reportRelated,false);
assert.equal(evidencePresentation({...e,run_id:'other'},catalog).reportRelated,false);
assert.equal(evidencePresentation({...e,run_id:'other'},catalog).graphEligible,false);
assert.equal(evidencePresentation(e,{...snap,catalogRun:'one'}).reportRelated,false,'catalog without report links does not borrow snapshot conclusions');
assert.equal(evidencePresentation(e,{...catalog,reportLinks:[...links,...links]}).links.length,1);
console.log('Evidence presentation: explicit report links, Chinese metadata, raw originals, latest checks, run-scoped catalog and exact action identity passed.');

const legacy=evidencePresentation(e,{...snap,findings:[],requirements:[],result:{summary:'观察到响应中的目标标记。',answers:[{kind:'flag',value:'raw-marker',evidence_ids:['shared']}]}});
assert.equal(legacy.title,'目标结果标记');assert.equal(legacy.summary,'观察到响应中的目标标记。');assert.equal(legacy.original_summary,'raw-marker');
