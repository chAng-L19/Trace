import {boundedCache} from './static/interactions.js';
const createContext=values=>vm.createContext(Object.assign(values,{boundedCache,AbortController,scopedNotice(){return (...args)=>values.notice?.(...args)}}));
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import {renderPage,resolveRoute} from './static/views.js';
import {runDeleteForm} from './static/evidence-manager.js';
import {createRunManagement} from './static/run-management.js';

const run={id:'selected',goal:'正在看的会话',status:'paused'};
const target={id:'other',goal:'另一会话',status:'created'};
const child={id:'child',goal:'内部Worker不单列',parent_task_id:'task',status:'completed'};
const state={page:'history',historyDetail:false,runs:[run,target,child],selected:run.id,snap:{run,messages:[]},historyFilter:''};
const html=renderPage(state);
assert.match(html,/<details id="run-archive-list" class="archive-list" open>/);
assert.doesNotMatch(html,/id="message-form"|内部Worker不单列/);
assert.match(html,/data-action="archive-run" data-id="other"/);
assert.match(html,/data-action="archive-delete" data-id="other"/);
assert.equal(resolveRoute('#history').historyDetail,false);
assert.match(renderPage({...state,historyDetail:true}),/id="message-form"/);
assert.doesNotMatch(renderPage({...state,historyDetail:true}),/id="run-archive-list"/);
// Detail/loading/error paints must not project the invisible archive on every SSE refresh.
for(const detail of [{},{runLoading:true},{runError:'fixture load failure'}]){
  const current={...state,historyDetail:true,...detail,runs:[{get parent_task_id(){throw Error('inactive archive was read')}}]};
  assert.doesNotThrow(()=>renderPage(current));
}
const hidden=renderPage({...state,historyHidden:true,hiddenRuns:[{id:'hidden',goal:'旧隐藏记录',deleted:true,status:'paused'}]});
assert.match(hidden,/旧隐藏记录/);
assert.match(hidden,/data-action="archive-delete" data-id="hidden"/);
assert.doesNotMatch(hidden,/data-action="archive-run"/);
assert.match(hidden,/永久删除/);
const overview=renderPage({...state,page:'overview'});
assert.match(overview,/<strong>2<\/strong><span>会话档案/);
assert.match(overview,/2 个未归档会话/);
assert.match(overview,/另含 1 个 Worker 子运行/);
assert.doesNotMatch(overview,/内部Worker不单列/);

// Quick controls must address their row, even while a different run is selected.
const source=fs.readFileSync(new URL('./static/app.js',import.meta.url),'utf8');
const code=source.slice(source.indexOf('async function dispatch('),source.indexOf('async function submit('));
const calls=[];let refreshed=0;
const dialog={open:false,content:'',close(){this.open=false}};
const context=createContext({chainEvents:{action:async()=>false},state,api:async(...args)=>calls.push(args),refresh:async()=>refreshed++,notice(){},runDeleteForm,
  showDetail(title,content){dialog.open=true;dialog.title=title;dialog.content=content},$:selector=>selector==='#detail-dialog'?dialog:null});
vm.runInContext(code,context);
context.el={dataset:{id:'other'}};
await vm.runInContext("dispatch('archive-run',el)",context);
assert.equal(calls[0][0],'/runs/other/run');
assert.equal(refreshed,1);
target.status='paused';await vm.runInContext("dispatch('archive-run',el)",context);
assert.equal(calls[1][0],'/runs/other/resume');
target.status='running';await vm.runInContext("dispatch('archive-pause',el)",context);
assert.equal(calls[2][0],'/runs/other/pause');
await vm.runInContext("dispatch('archive-delete',el)",context);
assert.equal(dialog.open,true);
assert.match(dialog.content,/id="run-delete-form" data-run="other"/);
assert.match(dialog.content,/option value="permanent"/);
assert.match(dialog.content,/option value="hide"/);
assert.equal(calls.length,3,'opening confirmation must not delete');
await vm.runInContext("dispatch('dialog-cancel',el)",context);
assert.equal(dialog.open,false);
assert.equal(calls.length,3,'cancelled confirmation must not delete');
console.log('Archive list, selected detail, row routing and delete cancellation passed.');

// View filters use the already loaded summary, including archived+hidden rows.
{
  let paints=0;
  const filtered={runs:[run,{...target,archived:true}],hiddenRuns:[{...target,deleted:true,archived:true}],historyDetail:false};
  const noNetwork=()=>{throw Error('view-only toggle started a request')};
  const manager=createRunManagement({state:filtered,api:noNetwork,refresh:noNetwork,render(){paints++}});
  await manager.dispatch('archive-archived',{dataset:{}});
  assert.equal(filtered.historyArchived,true);assert.equal(paints,1);
  const ctx=createContext({state:filtered,render(){paints++},refreshList:noNetwork});vm.runInContext(code,ctx);
  await vm.runInContext("dispatch('archive-hidden',{dataset:{}})",ctx);
  assert.equal(filtered.historyHidden,true);assert.equal(filtered.historyArchived,false);
  assert.equal(filtered.hiddenRuns.length,1);assert.equal(paints,2);
}

// A response started before permanent deletion must not repopulate its cache.
{
 let resolve;
 const details=new Map(),pending=new Promise(done=>resolve=done);
 const scope=createContext({chainEvents:{action:async()=>false},state:{selected:'gone',snap:{requests:[{id:'req'}]}},revision:1,navigation:1,detailRevision:0,requestDetails:details,requestLoads:new Map(),
 api:()=>pending,render(){throw Error('stale response rendered')}});
 vm.runInContext(code,scope);
 const request=vm.runInContext("dispatch('request-detail',{dataset:{request:'req'}})",scope);
 await Promise.resolve();await Promise.resolve();
 scope.revision++;scope.state.selected='';details.clear();
 resolve({request:{id:'req',body:'deleted body'}});await request;
 assert.equal(details.size,0,'late response cached deleted request');
}
