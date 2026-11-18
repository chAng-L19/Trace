import {boundedCache} from './static/interactions.js';
import {measureUI} from './static/performance.js';
import {configConflictMessage} from './static/config-revision.js';
const createContext=values=>vm.createContext(Object.assign(values,{boundedCache,measureUI,configConflictMessage,scopedNotice(){return (...args)=>values.notice?.(...args)}}));
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import {createProviderProbe,providerProbeControls} from './static/provider-probe.js';

function deferred(){let resolve,reject;const promise=new Promise((yes,no)=>{resolve=yes;reject=no});return {promise,resolve,reject}}
function harness(url='https://fixture.invalid/v1'){
  const elements=Object.fromEntries(['provider-url','provider-model','provider-key','provider-probe-status','provider-probe-button','provider-probe-cancel'].map(id=>[id,{value:'',dataset:{}}]));
  elements['provider-url'].value=url;elements['provider-model'].value='fixture';elements['provider-key'].value='[stored secret]';
  const state={page:'settings',settingTab:'provider',config:{revision:'rev-1',provider:{url,model:'fixture',api_key:'[stored secret]'}}},calls=[];
  const probe=createProviderProbe({getState:()=>state,getElement:id=>elements[id],api:(...args)=>{const reply=deferred();calls.push({args,reply});return reply.promise}});
  return {probe,state,elements,calls,status:()=>elements['provider-probe-status']};
}
{
  const h=harness();h.probe.sync();h.probe.sync();assert.equal(h.calls.length,0);assert.equal(h.status().dataset.status,'unprobed');
  const pending=h.probe.start();assert.equal(h.calls.length,1);assert.equal(h.status().dataset.status,'probing');
  assert.deepEqual(h.calls[0].args.slice(0,3),['/provider/probe','POST',{url:'https://fixture.invalid/v1',model:'fixture',api_key:'[stored secret]'}]);
  assert.ok(h.calls[0].args[3].signal instanceof AbortSignal);
  h.calls[0].reply.resolve({status:'success',detail:'短请求成功',elapsed_ms:4});await pending;
  assert.equal(h.status().dataset.status,'success');assert.equal(h.elements['provider-key'].value,'[stored secret]');
  assert.equal(h.state.config.provider.model,'fixture');
  h.elements['provider-model'].value='changed';h.probe.sync();assert.equal(h.status().dataset.status,'unprobed');
}
for(const change of ['url','model','key','navigation','revision']){
  const h=harness(),pending=h.probe.start();
  if(change==='navigation')h.state.page='history';
  else if(change==='revision')h.state.config.revision='rev-2';
  else h.elements['provider-'+change].value='changed-draft';
  h.probe.sync();assert.equal(h.calls[0].args[3].signal.aborted,true);
  h.calls[0].reply.resolve({status:'success',detail:'stale success'});await pending;
  if(change==='navigation'){h.state.page='settings';h.probe.sync()}
  assert.notEqual(h.status().dataset.status,'success',change+' applied stale result');
  if(!['navigation','revision'].includes(change))assert.equal(h.elements['provider-'+change].value,'changed-draft');
}
{
  const h=harness(),pending=h.probe.start();
  h.state.config=structuredClone(h.state.config);h.probe.sync();
  assert.equal(h.calls[0].args[3].signal.aborted,false,'equivalent config reload must not discard a running probe');
  h.calls[0].reply.resolve({status:'success',detail:'same connection'});await pending;
  h.state.page='history';h.probe.sync();h.state.page='settings';h.probe.sync();
  assert.equal(h.status().dataset.status,'success','completed result survives navigation');
  h.state.config=structuredClone(h.state.config);h.probe.sync();
  assert.equal(h.status().dataset.status,'success','equivalent config reload retains result');
}
{
  const h=harness(),events=[];
  const probe=createProviderProbe({getState:()=>h.state,getElement:id=>h.elements[id],onChange:value=>events.push(value),api:async()=>({status:'success',detail:'draft verified'})});
  h.elements['provider-model'].value='new-model';h.elements['provider-key'].value='new-private-key';
  await probe.start();assert.equal(events.at(-1).matchesSavedConfig,false);assert.equal(events.at(-1).model,'new-model');
  const submitted={provider:{url:h.elements['provider-url'].value,model:'new-model',api_key:'new-private-key'}},stored={revision:'rev-2',provider:{...submitted.provider,api_key:'[stored secret]'}};
  probe.saved(submitted,stored);h.state.config=stored;h.elements['provider-key'].value='[stored secret]';probe.sync();
  assert.equal(events.at(-1).status,'success');assert.equal(events.at(-1).matchesSavedConfig,true);
  h.state.page='history';probe.sync();assert.equal(events.at(-1).status,'success');
  assert.doesNotMatch(JSON.stringify(events),/new-private-key|fixture.invalid|stored secret/);
  h.state.page='settings';h.elements['provider-model'].value='different';probe.saved({provider:{...stored.provider,model:'different'}},{provider:{...stored.provider,model:'different'}});h.state.config={provider:{...stored.provider,model:'different'}};probe.sync();
  assert.equal(events.at(-1).status,'unprobed','saving a different untested model invalidates the old observation');
}
{
  const h=harness(),pending=h.probe.start();h.probe.cancel();assert.equal(h.calls[0].args[3].signal.aborted,true);
  assert.match(h.status().textContent,/取消/);h.calls[0].reply.resolve({status:'success',detail:'late'});await pending;assert.match(h.status().textContent,/取消/);
  const retry=h.probe.start();h.calls[1].reply.reject(Error('SECRET echo'));await retry;
  assert.equal(h.status().dataset.status,'failed');assert.doesNotMatch(h.status().textContent,/SECRET/);
}
{
  const h=harness('');h.probe.sync();await h.probe.start();assert.equal(h.calls.length,0);assert.equal(h.status().dataset.status,'mock');assert.equal(h.elements['provider-probe-button'].disabled,true);
}
{
  const h=harness(),one=h.probe.start();await h.probe.start();assert.equal(h.calls.length,1);
  h.elements['provider-model'].value='second';h.probe.sync();const two=h.probe.start();
  h.calls[1].reply.resolve({status:'responded',detail:'仅端点响应'});await two;
  h.calls[0].reply.resolve({status:'success',detail:'old'});await one;assert.equal(h.status().dataset.status,'responded');
}
{
  const h=harness();await (async()=>{const pending=h.probe.start();h.calls[0].reply.resolve({status:'success',detail:'prior generation'});await pending})();
  h.state.config={...h.state.config,revision:'credential-generation-2'};h.probe.sync();
  assert.equal(h.status().dataset.status,'unprobed','same masked secret and endpoint do not preserve observations across config revisions');
  h.state.page='history';h.probe.sync();h.state.page='settings';h.probe.sync();assert.equal(h.status().dataset.status,'unprobed');
}
// Verify the actual app dispatch uses the controller and passes cancellation to fetch.
const source=fs.readFileSync(new URL('./static/app.js',import.meta.url),'utf8');
const section=(start,end)=>source.slice(source.indexOf(start),source.indexOf(end,source.indexOf(start)));
const calls=[];const ctx={chainEvents:{action:async()=>false},state:{selected:''},providerProbe:{start(){calls.push('start')},cancel(){calls.push('cancel')}},fetch:async(...args)=>{calls.push(args);return {ok:true,text:async()=>JSON.stringify({status:'success'})}}};
createContext(ctx);vm.runInContext(section('async function dispatch(','async function submit'),ctx);
await vm.runInContext("dispatch('provider-probe',{})",ctx);await vm.runInContext("dispatch('provider-probe-cancel',{})",ctx);assert.deepEqual(calls,['start','cancel']);
vm.runInContext(section('export async function api(','function notice(').replace('export ',''),ctx);
ctx.signal=new AbortController().signal;await vm.runInContext("api('/provider/probe','POST',{model:'m'},{signal})",ctx);assert.equal(calls.at(-1)[1].signal,ctx.signal);
assert.match(providerProbeControls,/role="status"/);assert.match(providerProbeControls,/不会保存草稿或创建任务/);
assert.match(providerProbeControls,/>检测连通性<\/button>/);assert.match(providerProbeControls,/>取消检测<\/button>/);assert.doesNotMatch(providerProbeControls,/探测当前草稿/);
assert.match(source,/applyPending\(\);providerProbe.sync\(\)/);
assert.match(source,/e.target.id.startsWith\('provider-'\)/);
console.log('Provider probe checks passed: explicit action, exact draft, cancel, stale/navigation/config races, Mock, and secret-free errors.');
