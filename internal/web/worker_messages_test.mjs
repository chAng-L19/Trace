import assert from 'node:assert/strict';
import {workerActivity,workerPanel,workerSummary,workerMessageInput} from './static/workers.js';

const worker={id:'worker',run_id:'child',task_id:'task',status:'paused',cleanup_status:'completed'};
const help={id:'h1',from:'worker',to:'parent',kind:'help',content:'<script>alert(1)</script>',delivered:true};
const snap={workers:[worker],worker_messages:[help],worker_activity:[{worker_id:'worker',available:true,phase:'paused',cleanup_status:'completed',pending_messages:1,open_help_ids:['h1']}]};
let html=workerPanel(snap);
for(const text of ['已投递','尚无回复','尚未解决','name="reply_to"','value="h1"','name="content"','value="resolved"','不完成任务、不自动恢复 Worker','&lt;script&gt;'])assert.ok(html.includes(text),text);
assert.ok(!html.includes('<script>'));
const ids=[...html.matchAll(/\sid="([^"]+)"/g)].map(match=>match[1]);
assert.equal(new Set(ids).size,ids.length,'help input, labels and folded messages retain unique IDs');
assert.ok(!/name="content"[^>]*disabled/.test(html),'paused worker still accepts queued messages');
snap.worker_messages.push({id:'r1',from:'parent',to:'worker',kind:'reply',reply_to:'h1',content:'decision',delivered:false});
html=workerPanel(snap);
assert.match(html,/已有 1 条回复/);
assert.match(html,/尚未解决/);
assert.match(html,/待投递/);
snap.worker_messages.push({id:'foreign',from:'parent',to:'unrelated',kind:'resolved',reply_to:'h1',content:'unrelated'});
assert.match(workerPanel(snap),/尚未解决/,'wrong participants must not close a help');
snap.worker_messages.push({id:'done',from:'worker',to:'parent',kind:'resolved',reply_to:'h1',content:'confirmed',delivered:false});
html=workerPanel(snap);
assert.match(html,/已标记解决/);
assert.ok(!html.includes('id="worker-form-h1"'),'resolved help no longer offers close control');
assert.ok(html.includes('id="worker-form-worker"'),'general messages remain available');
const values={content:'real reply',kind:'reply',reply_to:'h1'},form={querySelector:selector=>({value:values[selector.split('"')[1]]})};
assert.deepEqual(workerMessageInput(form),values);
assert.deepEqual(workerMessageInput({querySelector:selector=>selector==='[name="content"]'?{value:'legacy'}:null}),{content:'legacy'});
const summary=workerSummary(snap);
for(const text of ['data-worker-activity','子 Worker','worker-refresh','data-id="child"'])assert.ok(summary.includes(text),text);
assert.doesNotMatch(summary,/<details[^>]*\bopen\b/,'subworkers start collapsed');
assert.match(html,/<details class="process-message" id="worker-process-message-h1">/);
assert.match(workerActivity({available:false,phase:'unavailable',cleanup_status:'completed'}),/状态来源不可用/);
assert.match(workerActivity({available:false,phase:'unavailable'}),/本 Worker 显式预算 未知/);
assert.match(workerActivity({available:true,phase:'tool',tools:['<tool>'],cleanup_status:'pending',budget_remaining_seconds:12}),/&lt;tool&gt;/);
assert.match(workerActivity({available:true,phase:'paused',last_model_response:'0001-01-01T00:00:00Z'}),/尚无记录/);
const calls=workerActivity({available:true,phase:'tool',open_help_total:85,open_help_ids:['one'],call_total:2,calls:[{call_id:'<call>',name:'exec',status:'uncertain',worker_status:'timed_out',timeout_seconds:300,outcome_uncertain:true,cleanup_error:'<live process>'}]});
assert.match(calls,/未解决求助 85/);
assert.match(calls,/&lt;call&gt;/);
assert.match(calls,/单工具上限 300 秒/);
assert.match(calls,/结果不确定/);
assert.match(calls,/清理未确认：&lt;live process&gt;/);
assert.doesNotMatch(calls,/<live process>/);
assert.equal(workerSummary({workers:[]}), '');

// Count data visits, not elapsed time: thousands of legacy and correlated
// messages must use a bounded number of full-array passes, with no help view
// supplied by the server. Stop a quadratic regression before it scans millions.
const history=[];
for(let i=0;i<600;i++){
  const id='history-help-'+i;
  history.push(
    {id,from:'worker',to:'parent',kind:'help',content:'request '+i,delivered:true},
    {id:'history-reply-a-'+i,from:'parent',to:'worker',kind:'reply',reply_to:id,content:'reverse reply'},
    {id:'history-reply-b-'+i,from:'worker',to:'parent',kind:'reply',reply_to:id,content:'same reply'},
    {id:'history-foreign-'+i,from:'parent',to:'sibling',kind:'resolved',reply_to:id,content:'unrelated'},
    {id:'history-first-'+i,from:'parent',to:'worker',kind:'resolved',reply_to:id,content:'first resolution'},
    {id:'history-later-'+i,from:'worker',to:'parent',kind:'resolved',reply_to:id,content:'later resolution'},
    {id:'history-legacy-'+i,from:'worker',to:'parent',content:'legacy'}
  );
}
let visits=0,relationReads=0;
const counted=history.map(message=>new Proxy(message,{get(target,key,receiver){
  if(key==='reply_to'){
    relationReads++;
    assert.ok(relationReads<=history.length*5,'repeated per-message relation scans');
  }
  return Reflect.get(target,key,receiver);
}}));
const observed=new Proxy(counted,{get(target,key,receiver){
  if(typeof key==='string'&&key!==''&&String(Number(key))===key){
    visits++;
    assert.ok(visits<=history.length*3,'more than three complete message passes');
  }
  return Reflect.get(target,key,receiver);
}});
const historyHTML=workerPanel({workers:[worker],worker_messages:observed});
assert.equal(visits,history.length*2,'one index pass and one ordered render pass');
assert.ok(relationReads<=history.length*4,'relation field work remains linear');
const renderedIDs=[...historyHTML.matchAll(/<article class="message" id="worker-msg-([^"]+)">/g)].map(match=>match[1]);
assert.deepEqual(renderedIDs,history.map(message=>message.id),'durable message order is preserved');
for(const i of [0,299,599]){
  const start=historyHTML.indexOf('id="worker-msg-history-help-'+i+'"');
  const card=historyHTML.slice(start,historyHTML.indexOf('</article>',start));
  assert.match(card,/已有 2 条回复/);
  assert.match(card,/已标记解决/);
  assert.ok(card.includes('history-first-'+i),'first matching resolution wins');
  assert.ok(!card.includes('history-foreign-'+i)&&!card.includes('history-later-'+i),'unrelated or later resolution must not replace first');
}
const collisionHTML=workerPanel({workers:[],worker_messages:[
  {id:'pair',from:'a|b',to:'c',kind:'help',content:'unresolved pair'},
  {id:'wrong-pair',from:'a',to:'b|c',kind:'resolved',reply_to:'pair',content:'different pair'},
  {id:'before-help',from:'right',to:'left',kind:'reply',reply_to:'out-of-order',content:'prior reply'},
  {id:'out-of-order',from:'left',to:'right',kind:'help',content:'later help'}
]});
assert.match(collisionHTML,/尚无回复 · 尚未解决/,'participant-key delimiters must not collide');
assert.match(collisionHTML,/已有 1 条回复 · 尚未解决/,'index includes relations before their help');
console.log('worker help correlation and read-only activity UI passed');
