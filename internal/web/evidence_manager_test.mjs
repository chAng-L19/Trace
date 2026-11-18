import {boundedCache} from './static/interactions.js';
const createContext=values=>vm.createContext(Object.assign(values,{boundedCache,delivery:values.delivery||{close(){}},scopedNotice(){return (...args)=>values.notice?.(...args)}}));
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import {catalogVisible,evidenceKey,evidenceManager,catalogDetail,runDeleteForm,evidenceDeleteForm} from './static/evidence-manager.js';
import {evidenceReading,evidenceCard,evidenceDetail} from './static/evidence.js';
import {renderPage} from './static/views.js';

const entry=(run,deleted=false)=>({run_id:run,run_goal:'Goal '+run,run_deleted:deleted,created_at:'2026-10-16T00:00:00Z',evidence:{id:'same-evidence',type:'tool_execution',call_id:'call',request_id:'request',verification:deleted?'invalid':'verified',content:'{"stdout":"observed output","exit_code":0}'},report_links:[{kind:'answer',id:'answer:1',title:'响应内容核验',summary:'报告引用本次响应结果',status:'unverified'}],action:{name:'opaque-id',local_name:'Readable Analyzer',remote_name:'remote.inspect',call_id:'call',request_id:'request',arguments:'{"path":"/source"}',result:{stdout:'observed output'}}});
const a=entry('run-a'),b=entry('run-b',true),base={catalogMode:'records',catalog:[a,b],catalogSelected:[evidenceKey(a),evidenceKey(b)]};
assert.notEqual(evidenceKey(a),evidenceKey(b));
const unlinked={...entry('run-c'),report_links:[]};
assert.deepEqual(catalogVisible({catalog:[a,unlinked]}),[a]);
assert.deepEqual(catalogVisible({catalog:[a,unlinked],catalogRelation:'unlinked'}),[unlinked]);
assert.equal(catalogVisible({catalog:[a,unlinked],catalogRelation:'all'}).length,2);
assert.match(evidenceManager(base),/响应内容核验/);
assert.match(evidenceManager(base),/报告引用本次响应结果/);
assert.equal(catalogVisible({...base,catalogQuery:' readable analyzer '}).length,2);
assert.equal(catalogVisible({...base,catalogQuery:'remote.inspect'}).length,2);
assert.deepEqual(catalogVisible({...base,catalogRun:'run-a',catalogStatus:'verified'}),[a]);
assert.deepEqual(catalogVisible({...base,catalogSource:'retained'}),[b]);
const filtered=evidenceManager({...base,catalogRun:'run-a'});
assert.match(filtered,/其中 1 条不在当前筛选中/);
assert.match(filtered,/匹配 1 \/ 2 条/);
assert.match(filtered,/id="catalog-all"[^>]*checked/);
assert.match(evidenceManager({...base,catalogQuery:'no-match'}),/id="catalog-all"[^>]*disabled/);
assert.match(evidenceManager({catalogMode:'records',catalog:[]}),/data-action="catalog-delete-selected"[^>]*disabled/);
assert.match(evidenceDeleteForm([]),/type="submit"[^>]*disabled/);
assert.match(runDeleteForm({id:'run-a',goal:'<script>bad()</script>'},2),/option value="permanent"/);
assert.doesNotMatch(runDeleteForm({id:'run-a',goal:'<script>bad()</script>'}),/<script>/);
assert.match(catalogDetail(a),/data-action="catalog-action"[^>]*data-run="run-a"/);
assert.match(catalogDetail(a),/data-action="dialog-cancel"/);
assert.doesNotMatch(catalogDetail(b),/data-action="catalog-open-run"/);
assert.match(renderPage({page:'evidence',selected:'',snap:null,...base}),/原始操作记录/);

// The global route must not replace the existing in-task evidence/artifact tabs.
const run={id:'run-a',goal:'Existing task',status:'created',usage:{}},snap={run,messages:[],actions:[a.action],evidence:[a.evidence],result:{evidence_ids:[a.evidence.id]},plan:{},events:[]};
const task={catalogMode:'records',page:'history',selected:run.id,runs:[run],snap,historyDetail:true,historyFilter:'',historyStatus:'',evidenceQuery:'',evidenceStatus:'',tools:[]};
assert.match(renderPage({...task,taskTab:'evidence'}),/阅读完整证据/);
assert.match(renderPage({...task,taskTab:'artifacts'}),/id="artifact-records"/);

// Reusing a projection within a render must not retain stale report links or
// source identities on the next render, even when the record IDs are unchanged.
{
  const record=entry('run-a'),catalog={catalogMode:'records',catalog:[record],catalogSelected:[evidenceKey(record)]};
  assert.match(evidenceManager(catalog),/报告引用本次响应结果/);
  record.report_links=[];
  assert.deepEqual(catalogVisible(catalog),[]);
  assert.doesNotMatch(evidenceManager(catalog),/class="catalog-row"/);
  assert.deepEqual(catalog.catalogSelected,[evidenceKey(record)],'filter changes preserve selected identities');
  record.report_links=[{kind:'answer',id:'answer:1',title:'更新的报告标题',summary:'更新的报告摘要',status:'unverified'}];
  catalog.catalogQuery='更新的报告摘要';
  assert.deepEqual(catalogVisible(catalog),[record]);
  assert.match(evidenceManager(catalog),/更新的报告标题/);
  catalog.catalogQuery='报告引用本次响应结果';
  assert.deepEqual(catalogVisible(catalog),[],'search uses only the current projection');
}
{
  const current=structuredClone(task);
  current.taskTab='evidence';current.evidenceQuery='研究结论';
  assert.match(renderPage(current),/阅读完整证据/,'search includes current report presentation');
  current.snap.result={};
  assert.doesNotMatch(renderPage(current),/阅读完整证据/,'removed citations disappear on the next render');
  current.evidenceRelation='all';current.evidenceQuery='';
  const record=current.snap.evidence[0];
  for(const renderer of [evidenceCard,evidenceDetail])assert.match(renderer(record,current.snap),/data-action="action-detail"[^>]*data-request="request"/);
  current.snap.actions.push({...current.snap.actions[0]});
  assert.doesNotMatch(renderPage(current),/data-action="action-detail"/,'duplicate request/call identities never gain a source link');
  for(const renderer of [evidenceCard,evidenceDetail])assert.doesNotMatch(renderer(record,current.snap),/data-action="action-detail"/);
  current.snap.actions.pop();record.run_id='other-run';
  for(const renderer of [evidenceCard,evidenceDetail])assert.doesNotMatch(renderer(record,current.snap),/data-action="action-detail"/,'matching request/call IDs cannot cross runs');
  assert.doesNotMatch(renderPage(current),/data-action="action-detail"/);
}

const source=fs.readFileSync(new URL('./static/app.js',import.meta.url),'utf8');
const section=(start,end)=>source.slice(source.indexOf(start),source.indexOf(end,source.indexOf(start)));
const deferred=()=>{let resolve,reject;const promise=new Promise((yes,no)=>{resolve=yes;reject=no});return {promise,resolve,reject}};
function harness(){
  const dialog={open:true,showModal(){this.open=true},close(){this.open=false;context.detailRevision++}},title={textContent:'old'},content={innerHTML:'old'};
  const elements={'#detail-dialog':dialog,'#dialog-title':title,'#dialog-content':content};
  const context={state:{page:'evidence',selected:'unrelated',...structuredClone(base)},navigation:0,detailRevision:0,$:selector=>elements[selector],api:async()=>a,
    chainEvents:{action:async()=>false,submit:async()=>false},mcpStatus:{close(){},open(){}},applyPending(){},render(){},catalogVisible,evidenceKey,catalogDetail,runDeleteForm,evidenceDeleteForm,esc:String,
    notice(){},readable:()=>'<arguments/>',toolOutput:()=>'<result/>',rawDetails:()=>'<raw/>',btnCatalogBack:()=>'<back/>'};
  createContext(context);
  vm.runInContext(section('function showDetail(','async function copyPath'),context);
  vm.runInContext(section('async function dispatch(','async function submit'),context);
  vm.runInContext(section('async function submit(','async function runIntegration'),context);
  return {context,dialog,title,content,run:code=>vm.runInContext(code,context)};
}

// Out-of-order fetches must follow the latest click, even if the older fetch
// returns first. Run identity must come from the card, never state.selected.
{
  const h=harness(),first=deferred(),second=deferred(),paths=[];
  h.context.api=path=>{paths.push(path);return paths.length===1?first.promise:second.promise};
  const p1=h.run("dispatch('catalog-detail',{dataset:{run:'run-a',id:'same-evidence'}})");
  const p2=h.run("dispatch('catalog-detail',{dataset:{run:'run-b',id:'same-evidence'}})");
  first.resolve(a);await p1;second.resolve(b);await p2;
  assert.deepEqual(paths,['/evidence/run-a/same-evidence','/evidence/run-b/same-evidence']);
  assert.match(h.content.innerHTML,/Goal run-b/);
  assert.doesNotMatch(h.content.innerHTML,/Goal run-a/);
}
for(const change of ['close','navigation','another-detail']){
  const h=harness(),reply=deferred();h.context.api=()=>reply.promise;
  const pending=h.run("dispatch('catalog-action',{dataset:{run:'run-a',id:'same-evidence'}})");
  if(change==='close')h.dialog.close();
  if(change==='navigation')h.context.navigation++;
  if(change==='another-detail')h.run("showDetail('new','new detail')");
  const original=h.content.innerHTML;
  reply.resolve(a);await pending;
  assert.equal(h.content.innerHTML,original,change+' must invalidate pending catalog detail');
}
for(const formID of ['run-delete-form','catalog-delete-form']){
  for(const change of ['close','navigation','another-detail']){
    const h=harness(),reply=deferred();
    h.context.api=()=>reply.promise;
    h.context.loadCatalog=async()=>{};h.context.refresh=async()=>{};h.context.forgetDeletedRun=()=>{};
    const previousSelector=h.context.$;
    h.context.$=selector=>selector==='#retain-run-evidence'?{checked:true}:previousSelector(selector);
    h.context.form={id:formID,dataset:{run:'run-a',keys:JSON.stringify([evidenceKey(a)])},isConnected:true,querySelector:()=>({value:'hide',checked:true})};
    const pending=h.run('submit(form)');
    if(change==='close')h.dialog.close();
    if(change==='navigation')h.context.navigation++;
    if(change==='another-detail')h.run("showDetail('new','new detail')");
    const original=h.content.innerHTML,open=h.dialog.open;
    reply.resolve({deleted:true});await pending;
    assert.equal(h.content.innerHTML,original,formID+' completion replaced '+change+' detail');
    assert.equal(h.dialog.open,open,formID+' completion closed '+change+' detail');
  }
}
for(const mode of ['permanent','hide']){
  const h=harness(),calls=[];
  h.context.api=async(...args)=>{calls.push(args);return {permanent:mode==='permanent',run_ids:['run-a','child-a']}};
  h.context.refresh=async()=>{};h.context.forgotten=[];h.context.forgetDeletedRun=id=>h.context.forgotten.push(id);
  h.context.form={id:'run-delete-form',dataset:{run:'run-a'},querySelector:()=>({value:mode})};
  await h.run('submit(form)');
  assert.equal(calls[0][0],'/runs/run-a');assert.equal(calls[0][1],'DELETE');assert.equal(calls[0][2].mode,mode);
  assert.deepEqual(Array.from(h.context.forgotten),['run-a','child-a']);
}
{
  const h=harness();h.context.api=async()=>{throw new Error('source not available')};
  await assert.rejects(h.run("dispatch('catalog-detail',{dataset:{run:'run-a',id:'same-evidence'}})"),/source not available/);
  assert.deepEqual(Array.from(h.context.state.catalogSelected),base.catalogSelected);
  h.run("dispatch('dialog-cancel',{dataset:{}})");
  assert.deepEqual(Array.from(h.context.state.catalogSelected),base.catalogSelected);
}

// Execute the real checkbox handler: select-all only changes visible keys,
// leaving selections from another filter intact.
{
  let change;
  const state={...structuredClone(base),catalogRun:'run-a',catalogSelected:[evidenceKey(b)]};
  const context={state,Set,catalogVisible,evidenceKey,render(){},document:{addEventListener:(type,fn)=>{change=fn}}};
  createContext(context);
  vm.runInContext(section("document.addEventListener('change'","document.addEventListener('input'"),context);
  change({target:{id:'catalog-all',checked:true,dataset:{}}});
  assert.deepEqual(new Set(state.catalogSelected),new Set([evidenceKey(a),evidenceKey(b)]));
  change({target:{id:'catalog-all',checked:false,dataset:{}}});
  assert.deepEqual(Array.from(state.catalogSelected),[evidenceKey(b)]);
}

// A reopened confirmation is a new DOM form. Completion must release its
// pending controls too, not only the original (now detached) submitted form.
{
  const controls=[{tagName:'INPUT',type:'checkbox',disabled:false,checked:false,dataset:{},closest:()=>null},{tagName:'BUTTON',type:'submit',disabled:false,textContent:'确认删除',dataset:{},closest:()=>null},{tagName:'INPUT',disabled:true,dataset:{},closest:()=>null}];
  const form={id:'run-delete-form',tagName:'FORM',dataset:{run:'run-a'},attrs:{},setAttribute(k,v){this.attrs[k]=v},removeAttribute(k){delete this.attrs[k]},querySelectorAll(selector){return selector.includes('data-pending-disabled')?controls.filter(control=>control.dataset.pendingDisabled!==undefined):controls}};
  const pending=new Set(),context={pending,state:{selected:''},viewScope:()=> 'evidence',document:{querySelectorAll:selector=>selector.includes('form')?[form]:selector.includes('data-pending-disabled')?controls.filter(control=>control.dataset.pendingDisabled!==undefined):[]}};
  createContext(context);
  vm.runInContext(section('function operationKey(','async function saveConfig'),context);
  context.form=form;
  const key=vm.runInContext('operationKey(form)',context);pending.add(key);
  vm.runInContext('applyPending()',context);
  assert.ok(controls.every(control=>control.disabled));
  assert.equal(controls[1].textContent,'确认删除','pending must not replace button text or nested DOM');
  pending.delete(key);
  vm.runInContext('applyPending()',context);
  assert.equal(controls[0].disabled,false,'reopened failed confirmation must be retryable');
  assert.equal(controls[0].checked,false,'pending recovery must preserve retention choice');
  assert.equal(controls[1].textContent,'确认删除');
  assert.equal(controls[2].disabled,true,'pending recovery must preserve originally disabled fields');
  assert.equal(form.attrs['aria-busy'],undefined);
}
console.log('Evidence manager checks passed: global routing, task tabs, filtered selection, cross-run identity, asynchronous detail isolation, and retryable pending forms.');

assert.match(evidenceManager(base),/id="catalog-all" data-ephemeral="true"/);
assert.match(evidenceReading({type:'tool_execution',content:JSON.stringify({exit_code:0,artifacts:[{kind:'stdout',bytes:24001}]})}).summary,/输出已归档/);
