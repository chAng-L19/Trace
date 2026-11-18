import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

const source=fs.readFileSync(new URL('./static/interactions.js',import.meta.url),'utf8').replaceAll('export ','');
const scope=(selected='a',taskTab='planner')=>({page:'history',historyDetail:true,selected,taskTab});
function form(value,id='worker-form-help',worker='worker'){
  const content={id:id+'-content',value,dataset:{}},kind={id:id+'-kind',value:'reply',dataset:{}},reply={value:'help'},feedback={};
  const result={id,dataset:{worker},content,kind,reply,feedback,
    querySelector(selector){return selector==='#'+content.id||selector==='[name="content"]'?content:selector==='#'+kind.id||selector==='[name="kind"]'?kind:selector==='[name="reply_to"]'?reply:selector==='[data-form-feedback]'?feedback:null},
    querySelectorAll(){return [content,kind]}
  };
  content.form=result;kind.form=result;return result;
}
function harness(){
  const nodes=new Map(),ctx=vm.createContext({document:{getElementById:id=>nodes.get(id)||null}});
  vm.runInContext(source,ctx);
  return {
    churn(){vm.runInContext("for(let i=0;i<10000;i++){views.set('old-'+i,{top:i,details:[],scrolls:[]});feedback.set('old-'+i,{message:'old',error:false})}if(views.size>64||feedback.size>128)throw Error('unbounded presentation cache')",ctx)},
    mount(s,f){nodes.clear();if(f)nodes.set(f.id,f);ctx.scope=s;vm.runInContext('renderedScope=viewScope(scope)',ctx)},
    remember(s,f,field='content'){ctx.scope=s;ctx.field=f[field];vm.runInContext('rememberInput(scope,field)',ctx)},
    draft(s,f,field='content'){ctx.scope=s;ctx.field=f[field];return vm.runInContext('drafts.get(fieldKey(scope,field))?.value',ctx)},
    capture(s,f){ctx.scope=s;ctx.form=f;return vm.runInContext('captureWorkerForm(scope,form)',ctx)},
    captureMessage(s,f){ctx.scope=s;ctx.form=f;return vm.runInContext('captureForm(scope,form)',ctx)},
    ackMessage(s,f,revision){ctx.scope=s;ctx.form=f;ctx.revision=revision;return vm.runInContext('acknowledgeForm(scope,form,revision,[form.content.id])',ctx)},
    ack(s,f,revision){ctx.scope=s;ctx.form=f;ctx.revision=revision;vm.runInContext('acknowledgeWorkerForm(scope,form,revision)',ctx)}
  };
}

// In-place acknowledgement clears only the submitted content, not relation/type.
for(const edit of [false,true]){
  const h=harness(),a=scope('a','activity'),old=form('sent','message-form');h.mount(a,old);h.remember(a,old);const revision=h.captureMessage(a,old);
  h.mount(scope('b','activity'),form('other run','message-form'));h.churn();
  const current=form(edit?'new draft':'sent','message-form');h.mount(a,current);if(edit)h.remember(a,current);
  assert.equal(h.ackMessage(a,old,revision),!edit);assert.equal(h.draft(a,current),edit?'new draft':undefined);
}
{
  const h=harness(),a=scope(),f=form('unsent');h.mount(a,f);h.remember(a,f);const sent=h.capture(a,f);
  h.churn();assert.equal(h.draft(a,f),'unsent');assert.equal(h.capture(a,f).operationID,sent.operationID);
  f.content.value='new revision';h.remember(a,f);h.churn();h.ack(a,f,sent);assert.equal(h.draft(a,f),'new revision');
}
{
  const h=harness(),a=scope(),f=form('sent');h.mount(a,f);h.remember(a,f);h.remember(a,f,'kind');
  const revision=h.capture(a,f);h.ack(a,f,revision);
  assert.equal(f.content.value,'');assert.equal(h.draft(a,f),undefined);
  assert.equal(f.kind.value,'reply');assert.equal(h.draft(a,f,'kind'),'reply');assert.equal(f.reply.value,'help');
}

// The response arrives after A -> B -> A: acknowledge the remounted A, not the
// detached original form. A second render must not restore the acknowledged text.
{
  const h=harness(),a=scope(),b=scope('b'),old=form('sent');
  h.mount(a,old);h.remember(a,old);
  const revision=h.capture(a,old);
  const other=form('B draft');h.mount(b,other);h.remember(b,other);
  const current=form(h.draft(a,old));h.mount(a,current);h.ack(a,old,revision);
  assert.equal(current.content.value,'');assert.equal(h.draft(a,current),undefined);
  assert.equal(other.content.value,'B draft');assert.equal(h.draft(b,other),'B draft');
  assert.equal(old.content.value,'sent','the detached form is not the acknowledgement target');
  assert.equal(form(h.draft(a,current)||'').content.value,'');
}

// A's response while B is mounted must not clear B, even with identical form IDs
// and text. Only the submitted scope's cached content is acknowledged.
{
  const h=harness(),a=scope(),b=scope('b'),old=form('same'),other=form('same');
  h.mount(a,old);h.remember(a,old);const revision=h.capture(a,old);
  h.mount(b,other);h.remember(b,other);h.ack(a,old,revision);
  assert.equal(other.content.value,'same');assert.equal(h.draft(b,other),'same');assert.equal(h.draft(a,old),undefined);
}

// Later edits in the same scope survive both in the draft cache and live DOM.
{
  const h=harness(),a=scope(),old=form('sent');h.mount(a,old);h.remember(a,old);
  const revision=h.capture(a,old);
  const current=form('next draft');current.kind.value='resolved';h.mount(a,current);h.remember(a,current);h.remember(a,current,'kind');h.ack(a,old,revision);
  assert.equal(current.content.value,'next draft');assert.equal(h.draft(a,current),'next draft');
  assert.equal(current.kind.value,'resolved');assert.equal(h.draft(a,current,'kind'),'resolved');
}

// An unsignalled live edit also blocks acknowledgement of the cached draft.
{
  const h=harness(),a=scope(),f=form('sent');h.mount(a,f);h.remember(a,f);const revision=h.capture(a,f);
  f.content.value='live edit';h.ack(a,f,revision);
  assert.equal(f.content.value,'live edit');assert.equal(h.draft(a,f),'sent');
}

// Matching run but a different tab is a different scope; do not touch that DOM.
{
  const h=harness(),a=scope(),tab=scope('a','activity'),old=form('sent'),current=form('sent');
  h.mount(a,old);h.remember(a,old);const revision=h.capture(a,old);
  h.mount(tab,current);h.remember(tab,current);h.ack(a,old,revision);
  assert.equal(current.content.value,'sent');assert.equal(h.draft(tab,current),'sent');assert.equal(h.draft(a,old),undefined);
}

// Another form or another field identity in the original scope is not the target.
{
  const h=harness(),a=scope(),old=form('sent'),other=form('sent','worker-form-other');
  h.mount(a,old);h.remember(a,old);const revision=h.capture(a,old);
  h.mount(a,other);h.remember(a,other);h.ack(a,old,revision);
  assert.equal(other.content.value,'sent');assert.equal(h.draft(a,other),'sent');
  const replaced=form('sent');replaced.content.id='different-content';h.mount(a,replaced);h.ack(a,old,revision);
  assert.equal(replaced.content.value,'sent');
}

// If the form disappeared (resolved help/loading/navigation), clear only its draft.
{
  const h=harness(),a=scope(),f=form('sent');h.mount(a,f);h.remember(a,f);const revision=h.capture(a,f);
  h.mount(a,null);h.ack(a,f,revision);
  assert.equal(h.draft(a,f),undefined);
}

// A failed POST never reaches acknowledgement: original and remounted drafts stay.
{
  const h=harness(),a=scope(),f=form('retry me');h.mount(a,f);h.remember(a,f);
  const revision=h.capture(a,f);
  await assert.rejects((async()=>{await Promise.reject(Error('offline'));h.ack(a,f,revision)})(),/offline/);
  assert.equal(h.draft(a,f),'retry me');assert.equal(f.content.value,'retry me');
  const remounted=form(h.draft(a,f));h.mount(a,remounted);assert.equal(remounted.content.value,'retry me');
}

// Same text plus a later kind edit is a new revision, even after A -> B -> A.
{
  const h=harness(),a=scope(),b=scope('b'),old=form('sent');h.mount(a,old);h.remember(a,old);h.remember(a,old,'kind');
  const revision=h.capture(a,old);h.mount(b,form('B'));
  const current=form('sent');h.mount(a,current);current.kind.value='resolved';h.remember(a,current,'kind');h.ack(a,old,revision);
  assert.equal(current.content.value,'sent');assert.equal(h.draft(a,current),'sent');
  assert.equal(current.kind.value,'resolved');assert.equal(h.draft(a,current,'kind'),'resolved');
}

// A changed-then-restored value has a different draft object, despite equal text.
for(const field of ['content','kind']){
  const h=harness(),a=scope(),f=form('sent');h.mount(a,f);h.remember(a,f);h.remember(a,f,'kind');
  const revision=h.capture(a,f),original=f[field].value;f[field].value=field==='kind'?'resolved':'changed';h.remember(a,f,field);
  f[field].value=original;h.remember(a,f,field);h.ack(a,f,revision);
  assert.equal(f.content.value,'sent');assert.equal(h.draft(a,f),'sent');assert.equal(h.draft(a,f,field),original);
}

// Later A revisions survive an ACK received while B is mounted.
{
  const h=harness(),a=scope(),b=scope('b'),f=form('sent');h.mount(a,f);h.remember(a,f);const revision=h.capture(a,f);
  f.content.value='changed';h.remember(a,f);f.content.value='sent';h.remember(a,f);
  const other=form('B');h.mount(b,other);h.remember(b,other);h.ack(a,f,revision);
  assert.equal(h.draft(a,f),'sent');assert.equal(h.draft(b,other),'B');assert.equal(other.content.value,'B');
}

// Current type/relation values are checked even without an input/change event.
for(const field of ['kind','reply']){
  const h=harness(),a=scope(),f=form('sent');h.mount(a,f);h.remember(a,f);const revision=h.capture(a,f);
  f[field].value='unsignalled edit';h.ack(a,f,revision);
  assert.equal(f[field].value,'unsignalled edit');assert.equal(f.content.value,'sent');assert.equal(h.draft(a,f),'sent');
}

// A fresh form without draft objects still acknowledges; remounting is not editing.
{
  const h=harness(),a=scope(),f=form('sent');h.mount(a,f);const revision=h.capture(a,f);
  const current=form('sent');h.mount(a,current);h.ack(a,f,revision);assert.equal(current.content.value,'');
}

// A changed field identity or worker is not the submitted form contract.
for(const replace of [f=>f.content.id='different-content',f=>f.dataset.worker='different-worker']){
  const h=harness(),a=scope(),old=form('sent');h.mount(a,old);h.remember(a,old);const revision=h.capture(a,old);
  const current=form('sent');replace(current);h.mount(a,current);h.ack(a,old,revision);
  assert.equal(current.content.value,'sent');assert.equal(h.draft(a,old),'sent');
}

// Lost ACK retries retain identity across remounts; edits and successful sends do not.
{
  const h=harness(),a=scope(),f=form('sent');h.mount(a,f);h.remember(a,f);
  const first=h.capture(a,f),retry=h.capture(a,f);assert.equal(first.operationID,retry.operationID);
  const remounted=form('sent');h.mount(a,remounted);assert.equal(h.capture(a,remounted).operationID,first.operationID);
  h.remember(a,remounted);const edited=h.capture(a,remounted);assert.notEqual(edited.operationID,first.operationID);
  h.ack(a,f,first);assert.equal(h.capture(a,remounted).operationID,edited.operationID,'old ACK deleted new send');
  h.ack(a,remounted,edited);assert.notEqual(h.capture(a,remounted).operationID,edited.operationID);
  assert.notEqual(h.capture(scope('b'),form('sent')).operationID,first.operationID);
}
console.log('Worker acknowledgement: retry identity, revision identity, same-text edits, A/B/A, scoped drafts and failure retention passed.');
