import {boundedCache} from './static/interactions.js';
import {configHeaders} from './static/config-revision.js';
const createContext=values=>vm.createContext(Object.assign(values,{boundedCache,configHeaders,delivery:values.delivery||{close(){}},scopedNotice(){return (...args)=>values.notice?.(...args)}}));
import fs from 'node:fs';
import vm from 'node:vm';
import assert from 'node:assert/strict';
import {integrationResult} from './static/integrations.js';

// Execute the actual async handlers with controllable responses; no duplicate
// implementation of navigation, request identity, or integration dispatch.
const source=fs.readFileSync(new URL('./static/app.js',import.meta.url),'utf8');
const section=(start,end)=>source.slice(source.indexOf(start),source.indexOf(end,source.indexOf(start)));
function deferred(){let resolve,reject;const promise=new Promise((yes,no)=>{resolve=yes;reject=no});return {promise,resolve,reject}}
function harness(){
  const dialog={open:true,showModal(){this.open=true}},title={textContent:'shared'},content={innerHTML:'original',prepend(){}};
  const elements={'#detail-dialog':dialog,'#dialog-title':title,'#dialog-content':content};
  const state={page:'settings',settingTab:'tools',config:{mcp:[{name:'shared'}]},tools:[],skills:[{name:'shared'}]};
  const context={chainEvents:{action:async()=>false},state,detailRevision:0,navigation:0,structuredClone,TextDecoder,Uint8Array,atob,$:selector=>elements[selector],document:{createElement:()=>({setAttribute(){}})},render(){},notice(){},esc:String,
    mcpStatus:{close(){},open(){}},mcpDetail:()=>'<mcp-detail/>',sourceLinks:()=>'<source/>',integrationActions:()=>'',integrationResult,applyPending(){},loadSettings:async()=>{},settingsLoader:{saved(){}},api:async()=>({content:'body'}),viewScope:()=> 'settings:tools'};
  createContext(context);
  vm.runInContext(section('function showDetail(','async function mutate'),context);
  vm.runInContext(section('async function dispatch(','async function submit'),context);
  vm.runInContext(section('async function runIntegration(','function loadSettings('),context);
  vm.runInContext(section('function operationKey(','function applyPending'),context);
  return {context,dialog,title,content,run:code=>vm.runInContext(code,context)};
}

{
  const h=harness(),reply=deferred();
  h.context.api=async()=>reply.promise;
  const pending=h.run("runIntegration('update',{kind:'mcp',name:'shared',id:'managed-1'})");
  h.run("showDetail('shared','<skill-detail/>')");
  reply.resolve({status:'updated'});await pending;
  assert.equal(h.content.innerHTML,'<skill-detail/>','same-name Skill must survive an older MCP update');
}
for(const change of ['navigation','another-detail','closed']){
  const h=harness(),reply=deferred();
  h.context.api=async()=>reply.promise;
  const pending=h.run("dispatch('skill-detail',{dataset:{name:'shared'}})");
  if(change==='navigation')h.context.navigation++;
  if(change==='another-detail')h.run("showDetail('new','<new-detail/>')");
  if(change==='closed'){h.dialog.open=false;h.context.detailRevision++}
  const original=h.content.innerHTML;
  reply.resolve({content:'stale'});await pending;
  assert.equal(h.content.innerHTML,original,`${change} must invalidate old Skill response`);
  if(change==='closed')assert.equal(h.dialog.open,false);
}
for(const change of ['closed','navigation','another-detail','unchanged']){
  const h=harness(),reply=deferred();let submitted;
  h.context.saveConfig=async next=>{submitted=next;await reply.promise;h.context.state.config=next};
  const pending=h.run("dispatch('mcp-toggle',{dataset:{index:'0'}})");
  assert.equal(submitted.mcp[0].disabled,true,'toggle must submit the updated configuration');
  assert.notEqual(submitted,h.context.state.config,'saving must not mutate the current configuration');
  assert.equal(h.context.state.config.mcp[0].disabled,undefined);
  if(change==='closed'){h.dialog.open=false;h.context.detailRevision++}
  if(change==='navigation'){h.context.navigation++;h.context.state.page='history'}
  if(change==='another-detail')h.run("showDetail('shared','<other-detail/>')");
  const original=h.content.innerHTML;
  reply.resolve();await pending;
  assert.equal(h.context.state.config.mcp[0].disabled,true,'completed save remains valid after leaving the detail');
  assert.equal(h.content.innerHTML,change==='unchanged'?'<mcp-detail/>':original,`${change} must only refresh the current MCP detail`);
  if(change==='closed')assert.equal(h.dialog.open,false,'completed toggle must not reopen a closed dialog');
}
{
  const h=harness();
  h.context.saveConfig=async()=>{throw Error('fixture save failed')};
  await assert.rejects(h.run("dispatch('mcp-toggle',{dataset:{index:'0'}})"),/fixture save failed/);
  assert.equal(h.content.innerHTML,'original');
  assert.equal(h.context.state.config.mcp[0].disabled,undefined);
}
{
  const h=harness(),reply=deferred(),skill=deferred();
  h.context.api=async path=>path==='/integrations/update'?reply.promise:skill.promise;
  const pending=h.run("runIntegration('update',{kind:'skill',name:'shared',id:'managed-1'})");
  reply.resolve({status:'updated'});
  await new Promise(resolve=>setImmediate(resolve));
  h.run("showDetail('shared','<other-detail/>')");
  skill.resolve({content:'updated but stale'});await pending;
  assert.equal(h.content.innerHTML,'<other-detail/>','Skill reload must check identity after its second await');
}
{
  const h=harness();
  let submitted;
  h.context.api=async(path,method,body)=>{submitted={path,method,body};return {status:'updated'}};
  await h.run("dispatch('integration-update',{dataset:{id:'managed-1',kind:'mcp',name:'shared'}})");
  assert.deepEqual(JSON.parse(JSON.stringify(submitted)),{path:'/integrations/update',method:'POST',body:{id:'managed-1',kind:'mcp',name:'shared'}});
  assert.equal(h.content.innerHTML,'<mcp-detail/>','current managed detail must reload after update');
  for(const id of ['config-form','mcp-form','integration-form'])assert.equal(h.run(`operationKey({id:'${id}',dataset:{}})`),'config-save');
  for(const action of ['mcp-edit','mcp-new','mcp-toggle','discard-config','integration-check','integration-update'])assert.equal(h.run(`operationKey({dataset:{action:'${action}'}})`),'config-save');
}
{
  const h=harness();
  h.context.api=async()=>{throw Error('fixture update failed')};
  await assert.rejects(h.run("runIntegration('update',{kind:'mcp',name:'shared'})"),/fixture update failed/);
  assert.equal(h.content.innerHTML,'original');
  assert.equal(h.context.state.integrationFeedback.error,true);
}
assert.match(integrationResult({status:'pinned'}),/固定/);
for(const change of ['navigation','another-detail','closed']){
  const h=harness(),reply=deferred();h.context.api=async()=>reply.promise;
  const pending=h.run("dispatch('artifact',{dataset:{hash:'a'}})");
  assert.match(h.content.innerHTML,/正在读取产物/);
  if(change==='navigation')h.context.navigation++;
  if(change==='another-detail')h.run("showDetail('new','<new-detail/>')");
  if(change==='closed'){h.dialog.open=false;h.context.detailRevision++}
  const original=h.content.innerHTML;
  reply.resolve({hash:'a',data_base64:btoa('stale'),bytes:5,offset:0,next_offset:5,eof:true});await pending;
  assert.equal(h.content.innerHTML,original,`${change} invalidates stale artifact response`);
  if(change==='closed')assert.equal(h.dialog.open,false);
}
{
  const h=harness(),slow=deferred();
  h.context.api=async path=>path.includes('/a?')?slow.promise:{hash:'b',data_base64:btoa('current'),bytes:7,offset:0,next_offset:7,eof:true};
  const first=h.run("dispatch('artifact',{dataset:{hash:'a'}})");
  await h.run("dispatch('artifact',{dataset:{hash:'b'}})");
  slow.resolve({hash:'a',data_base64:btoa('stale'),bytes:5,offset:0,next_offset:5,eof:true});await first;
  assert.match(h.content.innerHTML,/current/);assert.doesNotMatch(h.content.innerHTML,/stale/);
  h.context.api=async()=>{throw Error('read failed')};
  const preview=h.content.innerHTML;
  await assert.rejects(h.run("dispatch('artifact',{dataset:{hash:'b',offset:'65536'}})"),/read failed/);
  assert.equal(h.content.innerHTML,preview,'failed pagination preserves current preview and download');
  assert.notEqual(h.run("operationKey({dataset:{action:'artifact',hash:'a'}})"),h.run("operationKey({dataset:{action:'artifact',hash:'b'}})"));
}
console.log('Integration async checks passed: same-name identity, Skill and MCP toggle races, current-detail reload, shared lock, and errors.');

// A stale DOM index must follow stable service/tool identity after a directory refresh.
{
  const h=harness();h.context.state.config={mcp:[{name:'new'},{name:'shared'}]};let selected;
  h.context.saveConfig=async next=>{selected=next};
  await h.run("dispatch('mcp-toggle',{dataset:{index:'0',service:'shared'}})");
  assert.equal(selected.mcp[0].disabled,undefined);assert.equal(selected.mcp[1].disabled,true);
  h.context.state.settingsResources={tools:{loading:true}};
  for(const action of ['mcp-toggle','mcp-edit','mcp-detail','tool-detail'])await assert.rejects(h.run("dispatch('"+action+"',{dataset:{index:'0',service:'shared'}})"),/正在更新/);
  delete h.context.state.settingsResources;
  h.context.state.tools=[{definition:{function:{name:'new'}}},{local_name:'right',definition:{function:{name:'right'}}}];
  h.context.toolDetail=tool=>{assert.equal(tool.definition.function.name,'right');return '<right-tool/>'};
  await h.run("dispatch('tool-detail',{dataset:{index:'0',toolName:'right'}})");assert.equal(h.content.innerHTML,'<right-tool/>');
}
