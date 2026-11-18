import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import {conversation} from './static/conversation.js';
import {redraw} from './static/interactions.js';

const thinking={run:{id:'reasoning-lifecycle',status:'running'},messages:[],requests:[{id:'request'}],responses:[],actions:[],events:[{type:'model_stream',data:{request_id:'request',delta:{kind:'thinking',text:'保留完整思考'}}}]};
const body={type:'model_stream',data:{request_id:'request',delta:{kind:'text',text:'开始回答'}}};
const tool={type:'model_stream',data:{request_id:'request',delta:{kind:'tool_call',index:0,id:'call',name:'exec'}}};
const renderers=[snap=>conversation(snap)];
const disclosures=html=>[...html.matchAll(/<details[^>]*class="reasoning-disclosure"[^>]*>/g)].map(match=>match[0]);
for(const render of renderers){
  const active=disclosures(render(thinking));assert.ok(active.length);
  assert.ok(active.every(tag=>tag.includes('data-reasoning-phase="active"')&&tag.includes(' open')));
  for(const snap of [
    {...thinking,events:[...thinking.events,body]},
    {...thinking,events:[...thinking.events,tool]},
    {...thinking,actions:[{call_id:'call',request_id:'request',name:'exec',status:'started'}]},
    {...thinking,responses:[{request_id:'request',response:{thinking:'保留完整思考'}}]},
    {...thinking,run:{...thinking.run,status:'paused'}},
    {...thinking,events:[...thinking.events,{type:'model_error',data:{request_id:'request',error:'中断'}}]},
  ]){
    const html=render(snap),done=disclosures(html);assert.ok(done.length);
    assert.ok(done.every(tag=>tag.includes('data-reasoning-phase="done"')&&!tag.includes(' open')));
    assert.ok(html.includes('保留完整思考'),'folding must retain the full text');
  }
}

// Exercise the actual keyed reconciler used by patchLive, including its phase
// transition. A settled disclosure remains reader-owned after the first fold.
{
  const source=fs.readFileSync(new URL('./static/interactions.js',import.meta.url),'utf8');
  const context=vm.createContext({});
  vm.runInContext(source.slice(source.indexOf('const nodeKey='),source.indexOf('export function patchLive(')).replaceAll('export function ','function '),context);
  const detail=(phase,open)=>{
    const attrs=new Map([['data-reasoning-phase',phase],...(open?[['open','']]:[])]);
    return {id:'same-thought',nodeType:1,nodeName:'DETAILS',tagName:'DETAILS',childNodes:[],firstChild:null,nextSibling:null,
      get attributes(){return [...attrs].map(([name,value])=>({name,value}))},
      get open(){return attrs.has('open')},set open(value){if(value)attrs.set('open','');else attrs.delete('open')},
      hasAttribute:name=>attrs.has(name),getAttribute:name=>attrs.get(name)??null,setAttribute:(name,value)=>attrs.set(name,value),removeAttribute:name=>attrs.delete(name)};
  };
  const old=detail('active',true),parent={childNodes:[old],firstChild:old};
  context.parent=parent;context.next={childNodes:[detail('done',false)]};
  vm.runInContext('reconcile(parent,next)',context);assert.equal(old.open,false,'stream completion folds');
  old.open=true;
  vm.runInContext('reconcile(parent,next)',context);assert.equal(old.open,true,'manual reopen survives later frames');
}

// A small DOM adapter exercises redraw without a browser or another renderer.
// Native select.value drops values absent from its current options. The real
// reconciler must replace options before restoring a baseline or user draft.
{
 const source=fs.readFileSync(new URL('./static/interactions.js',import.meta.url),'utf8'),context=vm.createContext({});
 vm.runInContext(source.slice(source.indexOf('const nodeKey='),source.indexOf('export function patchLive(')).replaceAll('export function ','function '),context);
 function select(base,values,current=base){
  const attrs=new Map([['id','effort'],...(base===null?[]:[['data-config-base',base]])]);let options=values.map(value=>({value,selected:value===current})),chosen=options.some(o=>o.value===current)?current:options[0]?.value||'';
  return {id:'effort',nodeType:1,nodeName:'SELECT',tagName:'SELECT',nextSibling:null,dataset:{},ownerDocument:{createElement:()=>({dataset:{}})},
   get value(){return chosen},set value(value){chosen=options.some(o=>o.value===value)?value:''},get options(){return options},
   get innerHTML(){return JSON.stringify(options)},set innerHTML(value){options=JSON.parse(value);chosen=options.find(o=>o.selected)?.value??options[0]?.value??''},appendChild(option){options.push(option)},
   get attributes(){return [...attrs].map(([name,value])=>({name,value}))},hasAttribute:name=>attrs.has(name),getAttribute:name=>attrs.get(name)??null,setAttribute:(name,value)=>attrs.set(name,value),removeAttribute:name=>attrs.delete(name)};
 }
 const reconcile=(old,fresh)=>{context.parent={firstChild:old};context.next={childNodes:[fresh]};vm.runInContext('reconcile(parent,next)',context)};
 const rebased=select('',['']);reconcile(rebased,select('high',['','high']));assert.equal(rebased.value,'high','new saved effort survives an old list that lacked it');assert.equal(rebased.dataset.configConflict,undefined);
 const draft=select('',['','low'],'low');reconcile(draft,select('high',['','low','high']));assert.equal(draft.value,'low','valid user draft survives a baseline update');assert.equal(draft.dataset.configConflict,'true');
 const removed=select('',['','high'],'high');reconcile(removed,select('',['','low']));assert.equal(removed.value,'high','removed draft is retained visibly rather than silently changed to default');assert.equal(removed.dataset.configConflict,'true');assert.equal(removed.options.at(-1).disabled,true);assert.match(removed.options.at(-1).textContent,/选项已移除/);
 const ephemeral=select(null,['old'],'old');reconcile(ephemeral,select(null,['new'],'new'));assert.equal(ephemeral.value,'new','non-config selectors retain the existing valid-default behavior');
}

// A small DOM adapter exercises redraw without a browser or another renderer.
// Tabs share outer scroll, while each tab restores its own inner position.
{
  let details=[],scrolls=[];
  const page={querySelectorAll:selector=>selector==='details'?details:selector==='[data-scroll]'?scrolls:[],
    set innerHTML(html){
      details=[...html.matchAll(/<details id="([^"]+)" data-reasoning-phase="([^"]+)"( open)?>/g)].map(m=>({id:m[1],dataset:{reasoningPhase:m[2]},open:!!m[3]}));
      scrolls=[...html.matchAll(/data-scroll="([^"]+)"/g)].map(m=>({dataset:{scroll:m[1]},scrollTop:0,scrollLeft:0,scrollHeight:1000,clientHeight:100}));
    }};
  const workspace={scrollTop:0,scrollTo({top}){this.scrollTop=top}};
  const previous=globalThis.document;
  globalThis.document={activeElement:null,getElementById:id=>id==='page'?page:id==='workspace'?workspace:null,querySelectorAll:()=>[],querySelector:()=>null};
  const state={page:'history',historyDetail:true,selected:'scroll-lifecycle',taskTab:'activity'};
  const chat=phase=>`<details id="thought" data-reasoning-phase="${phase}"${phase==='active'?' open':''}></details><div data-scroll="conversation"></div>`;
  try{
    redraw(state,chat('active'));assert.equal(details[0].open,true);
    workspace.scrollTop=180;scrolls[0].scrollTop=230;
    redraw(state,chat('done'));
    assert.equal(scrolls[0].scrollTop,230,'conversation retains reading position');
    assert.equal(details[0].open,false,'completed thought folds');
    details[0].open=true;redraw(state,chat('done')+'<p>new frame</p>');
    assert.equal(details[0].open,true,'manual reopen survives redraw');
    state.selected='another-run';redraw(state,'<div data-scroll="conversation"></div>');
    assert.equal(workspace.scrollTop,0,'a different run keeps its own view identity');
  }finally{if(previous===undefined)delete globalThis.document;else globalThis.document=previous}
}
console.log('Workspace state: active/settled reasoning, keyed completion, manual reopen, tab scroll memory and run isolation.');
