import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import {boundedCache,viewScope} from './static/interactions.js';
import {createRunManagement} from './static/run-management.js';
import {createDelivery} from './static/delivery.js';
const app=fs.readFileSync(new URL('./static/app.js',import.meta.url),'utf8');
const notices=[],state={page:'history',historyDetail:true,taskTab:'activity',selected:'a',runs:[]};
const ctx=vm.createContext({navigation:0,state,viewScope,notice:(...args)=>notices.push(args)});
vm.runInContext(app.slice(app.indexOf('function scopedNotice('),app.indexOf('function connection(')),ctx);
const capture=()=>vm.runInContext('scopedNotice()',ctx);
const late=capture();late('current');assert.equal(notices.length,1);
state.selected='b';late('stale success');late('stale failure',true);assert.equal(notices.length,1);
ctx.navigation++;state.selected='a';late('stale after return');assert.equal(notices.length,1);
const tab=capture();state.taskTab='events';tab('stale tab');assert.equal(notices.length,1);
state.taskTab='activity';
let resolve;const manager=createRunManagement({state,api:()=>new Promise(r=>resolve=r),refresh:async()=>{},notice:ctx.notice,captureNotice:capture});
const pending=manager.dispatch('session-archive',{dataset:{}});ctx.navigation++;state.selected='b';resolve({});await pending;assert.equal(notices.length,1,'real action captures before awaiting');
const cache=boundedCache(3,200);cache.set('a','one').set('b','two').set('c','three');cache.get('a');cache.set('d','four');assert.equal(cache.has('b'),false);
for(let i=0;i<10000;i++)cache.set('run-'+i,{text:'x'.repeat(20)});assert.ok(cache.size<=3);
cache.set('oversize','x'.repeat(1000));assert.equal(cache.has('oversize'),false);cache.clear();assert.equal(cache.size,0);cache.set('new','ok');assert.equal(cache.get('new'),'ok');
cache.set('undefined',undefined);assert.equal(cache.has('undefined'),false);
// Production request-detail flights are independently bounded and cancelled.
const flights=[],requestContext=vm.createContext({state:{selected:'a',snap:{responses:[]}},revision:0,navigation:0,detailRevision:0,scopedNotice:()=>()=>{},requestDetails:boundedCache(24),requestLoads:new Map(),AbortController,
  api:(path,method,body,options)=>new Promise(resolve=>flights.push({path,options,resolve})),showDetail(){},rawDetails:()=>''});
vm.runInContext(app.slice(app.indexOf('async function dispatch('),app.indexOf('async function submit(')),requestContext);
const jobs=[];for(let i=0;i<10;i++){requestContext.el={dataset:{request:'r'+i}};jobs.push(vm.runInContext("dispatch('request-detail',el)",requestContext))}
assert.equal(requestContext.requestLoads.size,4);assert.ok(flights.slice(0,6).every(f=>f.options.signal.aborted));
let capacityResets=0;requestContext.workerCapacity={reset(){capacityResets++}};
const workspaceAbort=new AbortController();
requestContext.delivery={close(){}};requestContext.conversationHistory={cancel(){}};requestContext.runControllers=new Map([['a',workspaceAbort]]);requestContext.runLoads=new Map([['a',Promise.resolve()]]);requestContext.dirtyRuns=new Set(['a']);
vm.runInContext(app.slice(app.indexOf('function cancelRunReads('),app.indexOf('let listLoad,')),requestContext);vm.runInContext('cancelRunReads()',requestContext);
assert.equal(requestContext.requestLoads.size,0);assert.ok(flights.every(f=>f.options.signal.aborted));assert.equal(capacityResets,1,'navigation cancels tree-capacity reads too');
assert.equal(workspaceAbort.signal.aborted,true,'navigation aborts the workspace transport');assert.equal(requestContext.runControllers.size,0);assert.equal(requestContext.runLoads.size,0,'cancelled workspace reads release single-flight ownership immediately');assert.equal(requestContext.dirtyRuns.size,0);
for(const flight of flights)flight.resolve({id:flight.path});await Promise.all(jobs);assert.equal(requestContext.requestDetails.size,0);
assert.equal(app.includes('Object.assign(request,detail'),false,'detail bodies do not escape cache into long-lived snapshots');
const interaction=fs.readFileSync(new URL('./static/interactions.js',import.meta.url),'utf8');
assert.ok(interaction.includes('drafts=new Map(),views=boundedCache'));assert.ok(interaction.includes('workerSends=new Map()'));
// Real delivery controller, deterministic delayed transport: this is a component
// test, not the Store/HTTP end-to-end test in media_store_http_test.go.
const oldFetch=globalThis.fetch,oldURL=globalThis.URL;let serial=0,created=[],revoked=[],views=[],wait;
globalThis.URL={createObjectURL:()=>{const u='blob:'+ ++serial;created.push(u);return u},revokeObjectURL:u=>revoked.push(u)};
globalThis.fetch=()=>new Promise(r=>wait=r);
const media={snap:{actions:[{request_id:'r1',call_id:'c1',result:{artifacts:[{hash:'same',mime_type:'image/png',bytes:3}]}},{request_id:'r2',call_id:'c2',result:{artifacts:[{hash:'same',mime_type:'image/png',bytes:3}]}}]}};
let epoch=0;const delivery=createDelivery({state:media,showDetail:(title,html)=>views.push({title,html}),notice(){},token:()=>epoch,isCurrent:t=>t===epoch});
try{
  let job=delivery.dispatch('artifact',{dataset:{hash:'same'}});epoch++;wait({ok:true,blob:async()=>new Blob(['abc'])});await job;assert.equal(created.length,0);
  job=delivery.dispatch('artifact',{dataset:{hash:'same'}});wait({ok:true,blob:async()=>new Blob(['abc'])});await job;assert.equal(created.length,1);assert.match(views.at(-1).html,/r1.*c1/);assert.match(views.at(-1).html,/r2.*c2/);
  delivery.close();assert.deepEqual(revoked,created);
  job=delivery.dispatch('artifact',{dataset:{hash:'same'}});delivery.close();wait({ok:true,blob:async()=>new Blob(['abc'])});await job;assert.equal(created.length,1,'abort fences transports that ignore AbortSignal');
  job=delivery.dispatch('artifact',{dataset:{hash:'same'}});wait({ok:false,status:404});await job;assert.match(views.at(-1).html,/404/);assert.match(views.at(-1).html,/download/);
}finally{delivery.close();globalThis.fetch=oldFetch;globalThis.URL=oldURL}
console.log('UI lifetime: scope, bounded reloadable caches, media sources/abort/revoke/failure PASS');
