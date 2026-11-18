import {measureUI} from './static/performance.js';
import {configConflictMessage} from './static/config-revision.js';
import fs from 'node:fs';
import vm from 'node:vm';
import assert from 'node:assert/strict';
import {activeProvider} from './static/provider-profiles.js';
import {mcpTransport} from './static/interactions.js';
import {renderPage} from './static/views.js';

const read=name=>fs.readFileSync(new URL(`./static/${name}`,import.meta.url),'utf8');
const source=read('app.js');

// Connection severity is independent from model connectivity.
{
  const status={setAttribute(name,value){this[name]=value}},dot={classList:{toggle(name,value){this[name]=value}}},text={};
  const context=vm.createContext({$:selector=>({'#connection':text,'#connection-dot':dot,'#connection-status':status})[selector]});
  vm.runInContext(section(source,'function connection(','function modelConnection('),context);
  for(const [message,online,severity] of [['工作台已连接',true,'online'],['工作台重连中',false,'connecting'],['工作台连接失败',false,'error']]){
    context.message=message;context.online=online;vm.runInContext('connection(message,online)',context);
    assert.equal(text.textContent,message);assert.equal(status['data-status'],severity);assert.equal(dot.classList.online,online);
  }
}
assert.ok(read('index.html').includes('href="/polish.css"'));
assert.ok(read('polish.css').includes('@media(prefers-reduced-motion:reduce)'));

// The live entry must not leak shell escapes or let an unsized SVG hide the theme.
const entry=read('index.html'),liveCSS=read('style-unified.css');
assert.doesNotMatch(entry,/`[nr]/);
assert.match(entry,/<svg class="theme-icon" width="16" height="16"/);
assert.match(liveCSS,/[.]theme-icon[{][^}]*width:16px[^}]*height:16px[^}]*flex-shrink:0/);
assert.match(liveCSS,/[.]theme-control select[{][^}]*min-width:76px[^}]*flex-shrink:0/);

function section(text,start,end){
  const from=text.indexOf(start),to=text.indexOf(end,from);
  assert.ok(from>=0&&to>from,`missing source boundary: ${start} / ${end}`);
  return text.slice(from,to);
}

// Exercise the actual theme function, including normalization and persistence.
{
  const select={},root={dataset:{}},saved=new Map();
  const context=vm.createContext({document:{documentElement:root},$:selector=>selector==='#theme-select'?select:null,localStorage:{setItem:(key,value)=>saved.set(key,value)}});
  vm.runInContext(section(source,'function applyTheme(','applyTheme(localStorage'),context);
  for(const theme of ['paper','white','black','invalid']){
    context.theme=theme;vm.runInContext('applyTheme(theme)',context);
    const expected=theme==='invalid'?'paper':theme;
    assert.equal(root.dataset.theme,expected);
    assert.equal(select.value,expected);
    assert.equal(saved.get('trace.theme'),expected);
  }
}

// List-only startup and recovery must leave the initial "connecting" state.
{
  const state={page:'overview',historyDetail:false,listError:'previous failure'},connections=[];
  const context=vm.createContext({state,listLoad:null,listDirty:false,api:async()=>[],render(){},connection:(...args)=>connections.push(args)});
  vm.runInContext(section(source,'async function refreshList(','async function refreshRun('),context);
  await vm.runInContext('refreshList()',context);
  assert.equal(state.runsLoaded,true);assert.equal(state.listError,'');
  assert.equal(connections.at(-1)[0],'工作台已连接');assert.equal(connections.at(-1)[1],true);
  state.historyDetail=true;await vm.runInContext('refreshList()',context);
  assert.equal(connections.length,1,'list reads must not overwrite active SSE status');
}

// Ctrl+Enter uses the browser submission path, including required-field validation.
{
  let keydown,submitted=0,prevented=0;
  const form={requestSubmit(){submitted++}},textarea={closest:()=>form};
  const document={addEventListener:(_,handler)=>keydown=handler,querySelector:()=>textarea,activeElement:{tagName:'TEXTAREA'}};
  vm.runInNewContext(read('keyboard-shortcuts.js'),{document});
  for(const modifier of ['ctrlKey','metaKey'])keydown({key:'Enter',[modifier]:true,preventDefault(){prevented++}});
  assert.equal(submitted,2);assert.equal(prevented,2);
}

// Run the production status renderer without bootstrapping the app or network.
{
  let keydown,target=null,focused=0,prevented=0;
  const document={addEventListener:(_,handler)=>keydown=handler,activeElement:{tagName:'BODY'},querySelector(selector){
    if(target)assert.ok(selector.includes('#'+target.id));
    return target;
  }};
  vm.runInNewContext(read('keyboard-shortcuts.js'),{document});
  const press=key=>keydown({key,ctrlKey:key==='k',preventDefault(){prevented++}});
  for(const id of ['catalog-query','chain-query','history-filter','evidence-query']){
    target={id,focus(){focused++},select(){}};press('k');press('/');
  }
  assert.equal(focused,8);assert.equal(prevented,8);
  target=null;press('k');press('/');assert.equal(prevented,8,'missing search target preserves browser defaults');
  target={id:'catalog-query',focus(){focused++}};document.activeElement.isContentEditable=true;press('/');
  assert.equal(focused,8,'typing slash inside contenteditable must not steal focus');
}

// Run the production status renderer without bootstrapping the app or network.
{
  const element={dataset:{}},state={config:{provider:{url:'https://fixture.invalid/v1',model:'configured-model'}}};
  const context=vm.createContext({state,activeProvider,$:selector=>selector==='#model-connection'?element:null});
  vm.runInContext(section(source,'function modelConnection(','function render('),context);
  const update=result=>{context.result=result;vm.runInContext('modelConnection(result)',context)};
  update({status:'success',matchesSavedConfig:true,detail:'saved configuration passed'});
  assert.equal(element.dataset.status,'success');
  assert.match(element.textContent,/连通性检测通过/);
  assert.match(element.textContent,/configured-model/);assert.match(element.title,/saved configuration passed/);
  for(const status of ['success','failed','responded','probing']){
    update({status,matchesSavedConfig:false,model:'draft-model',detail:'draft-only result'});
    assert.equal(element.dataset.status,status,'draft observations are explicitly labeled, not silently hidden');
    assert.match(element.textContent,/draft-model · 草稿/);assert.match(element.textContent,/未保存/);
    assert.doesNotMatch(element.textContent,/configured-model/);
  }
  update({status:'failed',matchesSavedConfig:true,detail:'检测已取消：用户操作'});
  assert.match(element.textContent,/检测已取消/);
  update({status:'responded',matchesSavedConfig:true});
  assert.match(element.textContent,/有响应 · 检测未通过/);
  state.config.provider.url='';update({});
  assert.match(element.textContent,/未配置 · Mock 模式/);
  state.config=null;update({});
  assert.match(element.textContent,/读取配置/);
}

// HTTP login pages and proxy errors must not expose JSON parser diagnostics.
{
  const context=vm.createContext({measureUI,configConflictMessage});
  vm.runInContext(section(source,'export async function api(','function notice(').replace('export ',''),context);
  for(const status of [200,502]){
    context.fetch=async()=>({ok:status===200,status,text:async()=>'<html>login or proxy error</html>'});
    await assert.rejects(vm.runInContext("api('/config')",context),error=>{
      assert.match(error.message,new RegExp(`HTTP ${status}`));
      assert.match(error.message,/检查连接或重新登录/);
      assert.doesNotMatch(error.message,/Unexpected|SyntaxError|<html>/);
      return true;
    });
  }
  context.fetch=async()=>({ok:false,status:400,text:async()=>JSON.stringify({error:'fixture validation error'})});
  await assert.rejects(vm.runInContext("api('/config')",context),/fixture validation error/);
  context.fetch=async()=>({ok:true,status:200,text:async()=>JSON.stringify({status:'success'})});
  assert.equal((await vm.runInContext("api('/config')",context)).status,'success');
  context.fetch=async()=>({ok:true,status:204,text:async()=>''});
  assert.equal(await vm.runInContext("api('/config')",context),null);
}

// Disabled hidden inputs are excluded from browser constraint validation.
{
  const transport={value:'http'},command={value:'node'},url={value:'invalid URL draft'},stdio={},http={};
  const elements={'mcp-transport':transport,'mcp-command':command,'mcp-url':url};
  const previous=Object.getOwnPropertyDescriptor(globalThis,'document');
  globalThis.document={getElementById:id=>elements[id],querySelectorAll:selector=>selector==='[data-mcp-stdio]'?[stdio]:selector==='[data-mcp-http]'?[http]:[]};
  try{
    for(const value of ['http','stdio','http']){
      transport.value=value;mcpTransport();const isHTTP=value==='http';
      assert.equal(stdio.hidden,isHTTP);assert.equal(http.hidden,!isHTTP);
      assert.equal(command.required,!isHTTP);assert.equal(command.disabled,isHTTP);
      assert.equal(url.required,isHTTP);assert.equal(url.disabled,!isHTTP,'hidden URL must not block stdio save');
      assert.equal(url.type,'url');assert.equal(url.value,'invalid URL draft','transport switching preserves the draft');
    }
  }finally{if(previous)Object.defineProperty(globalThis,'document',previous);else delete globalThis.document}
}

{
  const runs=[{id:'run-alpha',goal:'alpha',status:'completed'},{id:'run-beta',goal:'beta',status:'running'}];
  for(const [historyFilter,count]of [['',2],['alpha',1],['missing',0]]){
    const html=renderPage({page:'history',snap:null,runs,historyFilter,historyStatus:''});
    assert.match(html,/<details id="run-archive-list" class="archive-list"/,'archive identity must survive changing summary counts');
    assert.equal((html.match(/id="run-archive-list"/g)||[]).length,1);
    assert.ok(html.includes(`会话 ${count} / 2`));
  }
}

// Source contracts guard the desktop scroll owner without duplicating redraw.
const redraw=section(read('interactions.js'),'export function redraw(','export function locateGraph(');
assert.ok(redraw.includes("workspace=document.getElementById('workspace')"));
assert.ok(redraw.includes('top:workspace.scrollTop'));
assert.ok(redraw.includes("workspace.scrollTo({top:sameOuter?outerTop:view.top,left:0,behavior:'instant'})"));
assert.ok(redraw.includes("workspace.scrollTo({top:0,left:0,behavior:'instant'})"));
assert.ok(redraw.includes('input.focus({preventScroll:true})'));
assert.doesNotMatch(redraw,/window[.](?:scrollY|scrollTo)/);
const desktop=liveCSS.slice(liveCSS.lastIndexOf('@media(min-width:701px)'));
assert.match(desktop,/html,body[{][^}]*height:100%[^}]*overflow:hidden/);
assert.match(desktop,/[.]rail[{][^}]*overflow-y:auto/);
assert.match(desktop,/#workspace[{][^}]*overflow:auto/);
console.log('Desktop shell checks passed: saved-model status, readable API errors, inactive MCP fields, stable archive identity, and workspace scroll contracts.');
