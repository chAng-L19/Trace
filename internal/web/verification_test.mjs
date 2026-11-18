import assert from 'node:assert/strict';
import {verificationPanel,readRequirements} from './static/verification.js';

assert.deepEqual(readRequirements({value:'  '}),[]);
assert.throws(()=>readRequirements({value:'{}'}),/数组/);
const snap={run:{status:'completed'},requirements:[{id:'goal',description:'<goal>',category:'goal'}],checks:[],findings:[{id:'finding',title:'<finding>',severity:'low',triage:'open',created_sequence:5,reproduction_steps:['observe'],evidence_ids:['old']}],evidence:[{id:'old',call_id:'original',source_event_sequence:2,type:'tool_execution',verification:'verified'},{id:'fresh',call_id:'probe',source_event_sequence:7,type:'tool_execution',verification:'verified'},{id:'manual',call_id:'manual',source_event_sequence:9,type:'manual_observation',verification:'unverified'}]};
const html=verificationPanel(snap);
assert.ok(html.includes('&lt;finding&gt;')&&!html.includes('<finding>'));
assert.ok(html.includes('id="check-goal-fresh"'));
assert.ok(html.includes('id="retest-finding-fresh"'));
assert.ok(!html.includes('id="retest-finding-old"')&&!html.includes('value="manual"'));
assert.ok(html.includes('data-verification-operation="retest"'));
snap.run.status='running';assert.match(verificationPanel(snap),/disabled>提交复测证据/);
console.log('verification panel: source filtering, escaping and inline operations passed');
