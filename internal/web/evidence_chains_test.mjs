import {boundedCache} from './static/interactions.js';
const createContext=values=>vm.createContext(Object.assign(values,{boundedCache,scopedNotice(){return (...args)=>values.notice?.(...args)}}));
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import {chainPanel,chainDetail,chainVisible,chainComposeForm} from './static/evidence-chains.js';
import {evidenceManager,evidenceKey} from './static/evidence-manager.js';
import {createChainEvents} from './static/evidence-chain-events.js';
const chain={id:'chain-one',name:'入口→响应核对',summary:'保留两步可复用路径',content_hash:'hash',steps:[{run_id:'one',evidence_id:'same',content_hash:'a'},{run_id:'two',evidence_id:'same',content_hash:'b'}]};
const entry={run_id:'one',run_goal:'source',verification:'unverified',chain,operations:chain.steps.map((step,i)=>({...step,verification:i?'unverified':'verified',action:{name:'exec',arguments:JSON.stringify({command:'step-'+i})},evidence:{id:'same',type:'tool_execution',verification:i?'unverified':'verified',content:JSON.stringify({stdout:'output-'+i,stderr:'',exit_code:i}),content_hash:step.content_hash}}))};
const state={chains:[entry],runs:[],snap:{run:{id:'target'},evidence_chain_refs:[{source_run_id:'one',chain_id:'chain-one',chain_hash:'hash',purpose:'支撑目标结论'}]}};
assert.equal(chainVisible(state,'target').length,1);
assert.equal(chainVisible(state,'target')[0].verification,'unverified');
assert.match(chainPanel(state,'target'),/支撑目标结论/);
assert.equal(chainVisible(state,'one').length,1,'own chain and matching citation share a row');
assert.equal(chainVisible({...state,snap:{run:{id:'target'}}},'target').length,0);
assert.equal(chainVisible({...state,chainQuery:'响应'}).length,1);
const purposePage={...state,chains:[{...entry,purpose:'purpose-only-hit',reference:true}],chainsPage:{total:1},chainsScope:'target',chainQuery:'purpose-only-hit'};
assert.equal(chainVisible(purposePage,'target').length,1,'server query matching reference purpose survives page rendering');
assert.match(chainPanel(purposePage,'target'),/purpose-only-hit/);
const list=evidenceManager(state);assert.match(list,/操作证据链/);assert.match(list,/2 个操作/);assert.doesNotMatch(list,/output-0/);
const detail=chainDetail(entry);assert.match(detail,/id="chain-step-0"/);assert.match(detail,/id="chain-step-1"/);assert.doesNotMatch(detail,/id="chain-step-[01]"[^>]*open/);assert.match(detail,/step-0/);assert.match(detail,/output-1/);
assert.match(chainPanel({chains:[]}),/暂无操作证据链/);
assert.doesNotMatch(chainDetail({...entry,chain:{...chain,name:'<script>bad</script>'}}),/<script>/);
const rows=chain.steps.map((step,i)=>({run_id:step.run_id,run_goal:'source',evidence:{id:step.evidence_id,content_hash:step.content_hash,source_event_sequence:i},action:{name:'exec'}}));
assert.match(chainComposeForm(rows,[{id:'one',goal:'source'}],['two/same']),/value="two\/same"[^>]*checked/);
const evolved=chainComposeForm(rows,[{id:'one',goal:'source'}],['two/same','one/same'],[{...chain.steps[1],label:'先检查响应'},{...chain.steps[0],label:'再核对入口'}]);
assert.ok(evolved.indexOf('value="two/same"')<evolved.indexOf('value="one/same"'));
assert.match(evolved,/name="chain-label" value="先检查响应"/);
assert.match(chainPanel({...state,chains:[{...entry,chain:{...chain,variant:'浏览器方法',applicability:'命令行受限'}}]}),/浏览器方法.*命令行受限/);
console.log('Operation evidence chains: one row per path, independent steps, run-qualified reuse, escaped summaries and raw-record separation passed.');
assert.equal(chainVisible({...state,chains:[]},'target')[0].verification,'invalid');
assert.equal(chainVisible({...state,snap:{...state.snap,evidence_chain_refs:[{source_run_id:'one',chain_id:'chain-one',chain_hash:'changed'}]}},'target')[0].verification,'invalid');

// Exercise actual asynchronous detail controller: latest click/close wins.
for(const mode of ['second','close','navigation']){
  let revision=0,shown='',resolveA;
  const pendingA=new Promise(resolve=>resolveA=resolve);
  const controller=createChainEvents({state:{chains:[],selected:'target'},api:path=>path.endsWith('/first')?pendingA:Promise.resolve({...entry,chain:{...chain,name:'latest'}}),showDetail:(_,html)=>{shown=html;revision++},render(){},refresh:async()=>{},notice(){},beginDetail:()=>++revision,isCurrent:token=>token===revision});
  const first=controller.action({dataset:{action:'chain-detail',run:'one',id:'first'}});
  if(mode==='second')await controller.action({dataset:{action:'chain-detail',run:'one',id:'second'}});else revision++;
  const before=shown;resolveA(entry);await first;assert.equal(shown,before,mode+' invalidates old response');
}
// Exercise production mutation -> refresh -> scoped/mode-specific load.
const app=fs.readFileSync(new URL('./static/app.js',import.meta.url),'utf8');
for(const page of ['evidence','history'])for(const mode of ['chains','records'])for(const mutation of ['unreference','chain-delete','catalog-delete']){
  const calls=[],s={page,selected:'target',historyDetail:page==='history',taskTab:'evidence',catalogMode:mode,runs:[],catalogSelected:[]};
  const api=async(path,method='GET')=>{calls.push({path,method});if(method!=='GET')return {};if(path.startsWith('/evidence-chains'))return [entry];if(path==='/evidence')return rows;return {run:{id:'target'}}};
  const ctx=createContext({state:s,api,catalogRevision:0,navigation:0,detailRevision:0,evidenceKey,render(){},notice(){},connection(){},refreshList:async()=>{},refreshRun:()=>api('/runs/target?view=workspace'),$:()=>({close(){}})});
  vm.runInContext(app.slice(app.indexOf('async function refresh(){'),app.indexOf('function forgetDeletedRun(')),ctx);
  const controller=createChainEvents({state:s,api,showDetail(){},render(){},refresh:()=>vm.runInContext('refresh()',ctx),notice(){},beginDetail:()=>0,isCurrent:()=>true});ctx.chainEvents=controller;
  if(mutation==='unreference')await controller.action({dataset:{action:'chain-unreference',target:'target',run:'one',id:'chain-one',hash:'hash'}});
  else if(mutation==='chain-delete')await controller.submit({id:'chain-delete-form',dataset:{run:'one',chain:'chain-one'}});
  else{vm.runInContext(app.slice(app.indexOf('async function submit('),app.indexOf('async function runIntegration(')),ctx);ctx.form={id:'catalog-delete-form',dataset:{keys:JSON.stringify(['one/same'])}};await vm.runInContext('submit(form)',ctx)}
  const gets=calls.filter(c=>c.method==='GET').map(c=>c.path);
  assert.equal(gets.filter(p=>p.startsWith('/evidence-chains')).length,mode==='chains'?1:0);
  if(mode==='chains')assert.ok(gets.includes('/evidence-chains?limit=40'+(page==='history'?'&run=target':'')));
  assert.equal(gets.filter(p=>p==='/evidence').length,mode==='records'&&page==='evidence'?1:0);
  assert.equal(calls.filter(c=>c.method==='DELETE').length,1);
}
// Concurrent reads are coalesced; a late Run A response never replaces Run B.
{
  let resolveA,resolveB;const calls=[],s={page:'history',selected:'A'};
  const pendingA=new Promise(r=>resolveA=r),pendingB=new Promise(r=>resolveB=r);
  const controller=createChainEvents({state:s,api:path=>{calls.push(path);return path.endsWith('A')?pendingA:pendingB},render(){},refresh(){},notice(){}});
  const a=controller.load(),again=controller.load();assert.equal(calls.length,1);
  s.selected='B';const b=controller.load();resolveB([{run_id:'B'}]);await b;resolveA([{run_id:'A'}]);await Promise.all([a,again]);
  assert.equal(s.chainsScope,'B');assert.equal(s.chains[0].run_id,'B');
  await controller.action({dataset:{action:'catalog-mode',mode:'records'}});assert.equal(calls.length,2,'Run records reuse snapshot');
}
console.log('Evidence refresh: scoped mode-specific reads, coalescing and late-navigation protection passed.');

// A mutation invalidates an in-flight scoped list and coalesces one follow-up.
{
  const calls=[],s={page:'history',selected:'A'};
  const controller=createChainEvents({state:s,api:path=>new Promise(resolve=>calls.push({path,resolve})),render(){},refresh(){},notice(){}});
  const first=controller.load(),second=controller.load(true);calls[0].resolve([{run_id:'stale'}]);await new Promise(r=>setImmediate(r));assert.equal(calls.length,2);assert.equal(s.chains,undefined);calls[1].resolve([{run_id:'fresh'}]);await Promise.all([first,second]);assert.equal(s.chains[0].run_id,'fresh');
}
