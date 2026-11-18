import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import {modelSettings,readModelSettings,addModelRow,resetModelRows,modelChoices} from './static/model-settings.js';
import {configRevision,rememberConfigRevision,forgetConfigRevision,configHeaders,mergeToolSelection} from './static/config-revision.js';
import {settings} from './static/settings.js';
import {settingsFields,createSettingsLoader} from './static/settings-loader.js';
import {localToolGroups} from './static/local-tools.js';
import {sourceLinks} from './static/integrations.js';
import {conversation,toolDisclosureBody} from './static/conversation.js';
import {messageDeliveryDetail,runDiagnosticsDetail} from './static/run-diagnostics.js';
import {runLink,parseRunRoute} from './static/run-management.js';
import {createChainEvents} from './static/evidence-chain-events.js';
import {performanceDiagnostics,performanceSample,uiNow} from './static/performance.js';
import {captureScroll,restoreScroll} from './static/interactions.js';

const config={revision:'r1',provider:{url:'https://provider.invalid',model:'main',api_key:'[stored secret]',reasoning_efforts:['low'],model_capabilities:{old:{image_input:true,reasoning_efforts:['high']}}},providers:{research:{url:'https://research.invalid',model:'research',api_key:'[stored secret]',reasoning_efforts:['high'],model_capabilities:{retained:{image_input:true}}}},role_models:{worker:{provider:'research',model:'research',reasoning_effort:'high',provider_id:'pinned-id',image_input:true},stage_routes:[{provider:'research',model:'research'}]},costs:{currency:'USD',rates:[{model:'main',input_per_million:'1.123456789123456789',output_per_million:'3',currency:'USD',source:'fixture-price',effective_at:'2026-11-10T00:00:00Z'}]},context:{},mcp:[{name:'mcp'}],local_tools:[{name:'local',command:'/env/bin/python',dir:'/tmp/task'}]};
const decode=text=>text.replace(/&quot;/g,'"').replace(/&#39;/g,"'").replace(/&lt;/g,'<').replace(/&gt;/g,'>').replace(/&amp;/g,'&');
function controls(html){
 const nodes={};
 for(const match of html.matchAll(/<select\b([^>]*id="([^"]+)"[^>]*)>([\s\S]*?)<\/select>/g)){
  const [,attrs,id,body]=match,base=decode(attrs.match(/data-config-base="([^"]*)"/)?.[1]||'');
  const selected=body.match(/<option value="([^"]*)" selected/)?.[1]??body.match(/<option value="([^"]*)"/)?.[1];
  nodes[id]={value:decode(selected||''),dataset:{configBase:base}};
 }
 // Inputs are void elements; parse separately rather than spanning their siblings.
 for(const match of html.matchAll(/<input\b([^>]*id="([^"]+)"[^>]*)>/g)){const [,attrs,id]=match;nodes[id]={value:decode(attrs.match(/value="([^"]*)"/)?.[1]||''),dataset:{configBase:decode(attrs.match(/data-config-base="([^"]*)"/)?.[1]||'')}}}
 const query=selector=>{
  const match=selector.match(/^\[([^=\]]+)(?:="([^"]+)")?\]$/),attribute=match[1],wanted=match[2],key=attribute.slice(5).replace(/-([a-z])/g,(_,c)=>c.toUpperCase());
  return [...html.matchAll(new RegExp('<[^>]+\\b'+attribute+'="([^\\"]+)"[^>]*>','g'))].filter(m=>wanted===undefined||m[1]===wanted).map(m=>({dataset:{[key]:m[1],...(m[0].includes('data-row=')?{row:m[0].match(/data-row="([^"]+)"/)[1]}:{})}}));
 };
 return {nodes,get:id=>nodes[id],query};
}
resetModelRows();const initial=settings({config,settingTab:'provider'}),form=controls(initial);
assert.match(initial,/data-model-role="orchestrator"/);assert.match(initial,/data-model-role="recon"/);
assert.match(initial,/逐模型能力.*配置声明/);assert.match(initial,/缓存读取价格/);
assert.deepEqual(readModelSettings(config,form.get,form.query),config,'unchanged row forms preserve all supported fields and secret placeholders');
form.nodes['model-role-worker-effort'].value='';
let saved=readModelSettings(config,form.get,form.query);
assert.equal(saved.role_models.worker.reasoning_effort,'');assert.equal(saved.role_models.worker.provider_id,'pinned-id');assert.equal(saved.providers.research.api_key,'[stored secret]');
form.nodes['model-role-worker-effort'].value='ultra';assert.throws(()=>readModelSettings(config,form.get,form.query),/未声明支持/);
form.nodes['model-role-worker-effort'].value='high';form.nodes['model-role-worker-mode'].value='inherit';saved=readModelSettings(config,form.get,form.query);assert.equal(saved.role_models.worker,undefined);assert.deepEqual(saved.role_models.stage_routes,config.role_models.stage_routes);
form.nodes['model-role-worker-mode'].value='custom';form.nodes['model-provider-0-key'].value='';saved=readModelSettings(config,form.get,form.query);assert.equal(saved.providers.research.api_key,'');assert.equal(config.providers.research.api_key,'[stored secret]');
const renameForm=controls(initial),renameBaseline=JSON.stringify(config);renameForm.nodes['model-provider-0-name'].value='renamed';
assert.throws(()=>readModelSettings(config,renameForm.get,renameForm.query),/更名后请重新填写 API Key/);
assert.equal(JSON.stringify(config),renameBaseline);assert.equal(renameForm.nodes['model-provider-0-name'].value,'renamed');assert.equal(renameForm.nodes['model-provider-0-key'].value,'[stored secret]');
for(const key of ['','replacement-fixture-key']){renameForm.nodes['model-provider-0-key'].value=key;const renamed=readModelSettings(config,renameForm.get,renameForm.query);assert.equal(renamed.providers.research,undefined);assert.equal(renamed.providers.renamed.api_key,key);assert.deepEqual(renamed.providers.renamed.model_capabilities,config.providers.research.model_capabilities)}
const advancedRename=structuredClone(config);advancedRename.providers.research.api_key='advanced-fixture-key';renameForm.nodes['model-provider-0-key'].value='[stored secret]';
assert.equal(readModelSettings(advancedRename,renameForm.get,renameForm.query).providers.renamed.api_key,'advanced-fixture-key','unchanged secret form preserves the explicit advanced JSON key');
form.nodes['model-price-0-cache_read_per_million'].value='0.000000000000001';form.nodes['model-price-0-cache_write_per_million'].value='0';form.nodes['model-price-0-billing_basis'].value='disjoint_cache_v1';saved=readModelSettings(config,form.get,form.query);assert.equal(saved.costs.rates[0].input_per_million,'1.123456789123456789');assert.equal(saved.costs.rates[0].cache_write_per_million,'0');
assert.deepEqual(modelChoices(config,'default','unlisted').efforts,[]);
addModelRow('routes');assert.equal((modelSettings(config).match(/data-model-route=/g)||[]).length,3);resetModelRows();
// Advanced JSON is the base; untouched ordinary fields never revert it.
const mixed=controls(initial),advanced=structuredClone(config);advanced.role_models.worker.model='retained';mixed.nodes['model-role-worker-effort'].value='';
assert.equal(readModelSettings(advanced,mixed.get,mixed.query).role_models.worker.model,'retained');
mixed.nodes['model-role-worker-effort'].value='high';
assert.equal(readModelSettings(advanced,mixed.get,mixed.query).role_models.worker.model,'retained','advanced-only role change stays intact');
const pricePair=structuredClone(config);pricePair.costs.rates.push({...pricePair.costs.rates[0],model:'other'});const priceRows=controls(modelSettings(pricePair)),reordered=structuredClone(pricePair);reordered.costs.rates.reverse();const unchanged=JSON.stringify(reordered);
priceRows.nodes['model-price-0-input_per_million'].value='99';assert.throws(()=>readModelSettings(reordered,priceRows.get,priceRows.query),/价格 JSON 已改变行身份或顺序/);assert.equal(JSON.stringify(reordered),unchanged);
priceRows.nodes['model-price-0-input_per_million'].value=config.costs.rates[0].input_per_million;assert.equal(readModelSettings(reordered,priceRows.get,priceRows.query).costs.rates[0].model,'other','advanced-only price order is retained');
const rerouted=structuredClone(config);rerouted.role_models.stage_routes[0].model='retained';mixed.nodes['model-route-0-model'].value='new-row-model';assert.throws(()=>readModelSettings(rerouted,mixed.get,mixed.query),/阶段路由 JSON 已改变行结构/);
mixed.nodes['model-route-0-model'].value='research';assert.equal(readModelSettings(rerouted,mixed.get,mixed.query).role_models.stage_routes[0].model,'retained');

const state={settingTab:'provider',config:structuredClone(config)},draft={id:'config-form',dataset:{configRevision:'r1'}};
rememberConfigRevision(state,draft);state.config.revision='r2';draft.dataset.configRevision='r2';
assert.equal(configRevision(state,draft),'r1','reload must not advance a dirty form baseline');
assert.deepEqual(configHeaders(configRevision(state,draft)),{'If-Match':'"r1"'});
state.settingTab='local';assert.equal(configRevision(state,draft),'r2');state.settingTab='provider';assert.equal(configRevision(state,draft),'r1');forgetConfigRevision(state,draft);assert.equal(configRevision(state,draft),'r2');
const tools=['mcp','local'].map(adapter=>({adapter,local_name:adapter,definition:{function:{name:adapter+'_tool',description:adapter}}}));
assert.deepEqual(mergeToolSelection(['mcp_tool','local_tool','unavailable-id'],tools,'mcp',[]),['local_tool','unavailable-id']);
assert.deepEqual(mergeToolSelection(undefined,tools,'local',[]),['mcp_tool']);
const view={config,tools,skills:[],runs:[],snap:{run:{id:'run',goal:'fixture',tool_selection:{ids:['mcp_tool','local_tool']}}}};
assert.doesNotMatch(settings({...view,settingTab:'tools'}),/data-action="local-detail"|data-tool-id="local_tool"|id="local-json"/);
assert.doesNotMatch(settings({...view,settingTab:'local'}),/data-action="mcp-new"|data-tool-id="mcp_tool"|id="mcp-json"/);
assert.deepEqual(settingsFields('local'),['config','tools']);
const loads=[],loaderState={settingTab:'local',config:null};await createSettingsLoader({state:loaderState,api:async p=>{loads.push(p);return p==='/config'?config:tools}}).load('local');assert.deepEqual(loads,['/config','/tools']);
const groups=localToolGroups({local_tools:[{name:'one',command:'/env/bin/python'},{name:'two',command:'/env/bin/python'},{name:'true-bin',command:'/project/bin/tool',local_path:'/project/bin'}]});
assert.deepEqual(groups.map(g=>g.name),['one','two','bin']);assert.match(sourceLinks({command:'/env/bin/python',dir:'/tmp/task'}),/执行工作目录/);assert.doesNotMatch(sourceLinks({dir:'/tmp/task'}),/<span>项目目录/);

const call={call_id:'same',request_id:'r1',name:'read',arguments:'{"path":"a"}',status:'completed',result:{stdout:'LAZY_LARGE_OUTPUT',artifacts:[{hash:'h',kind:'file'}]}},snap={run:{id:'run',status:'completed'},messages:[],actions:[call],events:[]};
const collapsed=conversation(snap,{lazy:true});assert.doesNotMatch(collapsed,/LAZY_LARGE_OUTPUT/);assert.match(collapsed,/data-tool-key="r1:same"/);
const element={dataset:{toolRun:'run',toolKey:'r1:same',toolPrefix:'chat'}};assert.match(toolDisclosureBody(element,snap),/LAZY_LARGE_OUTPUT/);assert.match(toolDisclosureBody(element,snap),/调用链接/);assert.equal(toolDisclosureBody(element,{...snap,run:{id:'other'}}),null);
assert.equal(toolDisclosureBody({...element,dataset:{...element.dataset,toolKey:'r2:same'}},snap),null);
const pane={dataset:{scroll:'nested-tool'},scrollTop:75,scrollLeft:12,scrollHeight:1000,clientHeight:200};const anchor=captureScroll(pane);pane.scrollHeight=2000;restoreScroll(pane,anchor);assert.equal(pane.scrollTop,75);assert.equal(pane.scrollLeft,12);assert.equal(captureScroll(pane).follow,false);
// Appended content follows the live edge without touching historical layout;
// once the reader scrolls up, insertion above the same message keeps its offset.
let geometry=0,messagePosition=100;
const scrollBox={dataset:{scroll:'conversation'},scrollLeft:9,scrollHeight:1000,clientHeight:200,_top:800,get scrollTop(){return this._top},set scrollTop(v){this._top=Math.max(0,Math.min(v,this.scrollHeight-this.clientHeight))},getBoundingClientRect(){geometry++;return {top:100}},querySelectorAll(){geometry++;return [{dataset:{messageId:'reading-anchor'},getBoundingClientRect:()=>{geometry++;return {top:100+messagePosition-scrollBox.scrollTop,bottom:200+messagePosition-scrollBox.scrollTop}}}]}};
const following=captureScroll(scrollBox);assert.equal(following.follow,true);assert.equal(geometry,0,'live-edge streaming does not lay out historical message anchors');
scrollBox.scrollHeight=1400;restoreScroll(scrollBox,following);assert.equal(scrollBox.scrollTop,1200);assert.equal(scrollBox.scrollLeft,9);
scrollBox.scrollHeight=1600;const stillFollowing=captureScroll(scrollBox);assert.equal(stillFollowing.follow,true,'content growth without reader movement keeps follow intent');assert.equal(geometry,0);restoreScroll(scrollBox,stillFollowing);assert.equal(scrollBox.scrollTop,1400);
scrollBox.scrollTop=100;const reading=captureScroll(scrollBox);assert.equal(reading.follow,false);assert.equal(reading.id,'reading-anchor');assert.equal(reading.offset,0);assert.ok(geometry>0);
messagePosition+=75;scrollBox.scrollHeight+=75;restoreScroll(scrollBox,reading);assert.equal(scrollBox.scrollTop,175);assert.equal(captureScroll(scrollBox).offset,0,'new content above the reader preserves the same message offset');assert.equal(captureScroll(scrollBox).follow,false);

const receipt={key:'k',message_id:'m',source:'worker',inbox_status:'persisted',history_status:'coalesced',request_status:'archived',identity_status:'pinned',recipient:{run_id:'child',worker_id:'w',task_id:'t',visit:2},received_by:{run_id:'child',worker_id:'w',visit:2},representative_id:'first',archived_requests:[{request_id:'req',purpose:'context_summary'}],request_count:1};
const receiptHTML=messageDeliveryDetail({items:[receipt],total:2,next_before:'opaque +/'},'run');assert.match(receiptHTML,/合并/);assert.match(receiptHTML,/不代表 Provider 收到/);assert.match(receiptHTML,/#history\/child\?request=req/);assert.match(receiptHTML,/context_summary/);assert.match(receiptHTML,/data-before="opaque \+\/"/);
const diagnostics=runDiagnosticsDetail({total_requests:1,requests:[{request_id:'req',timing:{first_byte_ms:0},cost:{status:'unknown'}}]});assert.match(diagnostics,/首字节<\/dt><dd>0 ms/);assert.match(diagnostics,/首语义输出<\/dt><dd>未知/);assert.doesNotMatch(diagnostics,/本地费用估计<\/dt><dd>0/);
const link=runLink('source',{tab:'evidence',chain:'chain',chain_run:'source',chain_hash:'fixed'});assert.equal(parseRunRoute(link.slice(1)).deepLink.chain_hash,'fixed');
let detailPath='';const chain={id:'chain',content_hash:'fixed',steps:[]};const controller=createChainEvents({state:{selected:'run'},api:async path=>{detailPath=path;return {run_id:'source',chain}},beginDetail:()=>1,isCurrent:()=>true,showDetail(){},notice(){}});await controller.action({dataset:{action:'chain-detail',run:'source',id:'chain',hash:'fixed'}});assert.match(detailPath,/\?hash=fixed$/);
// The actual asynchronous dispatcher rejects late dialog/run/navigation replies.
const app=fs.readFileSync(new URL('./static/app.js',import.meta.url),'utf8'),dispatch=app.slice(app.indexOf('async function dispatch('),app.indexOf('async function submit('));
for(const change of ['close','navigate','run','none']){let resolve,shown='';const context=vm.createContext({URLSearchParams,state:{selected:'run'},detailRevision:0,navigation:0,scopedNotice:()=>()=>{},messageDeliveryDetail,runDiagnosticsDetail,api:()=>new Promise(r=>resolve=r),showDetail:(_,body)=>shown=body});vm.runInContext(dispatch,context);const pending=vm.runInContext("dispatch('message-deliveries',{dataset:{}})",context);if(change==='close')context.detailRevision++;if(change==='navigate')context.navigation++;if(change==='run')context.state.selected='other';resolve({items:[receipt],total:1});await pending;assert.equal(!!shown,change==='none')}
// Execute the app render entry and its real settings actions. Zero-argument
// controller callbacks use ordinary reconciliation; only explicit resets force it.
{
  const forces=[],context=vm.createContext({
    state:{page:'overview'},pageInfo:{overview:['任务中枢','01 / OPERATIONS']},$:()=>({}),
    document:{querySelectorAll:()=>[]},measureUI:(_,fn)=>fn(),renderPage:()=>'<page/>',
    redraw:(_,html,force)=>{forces.push(force);return false},renderRunCount(){},renderProviderLabel(){},
    applyPending(){},providerProbe:{sync(){}},workerCapacity:{paint(){}},searchSourceProbe:{sync(){}},
    scopedNotice:()=>()=>{},rememberConfigRevision(){},addModelRow(){},loadSettings:async()=>{},
    resetModelRows(){},forgetConfigRevision(){},forgetForm(){},providerEditorSelection:()=> 'default',formFeedback(){},
  });
  vm.runInContext(app.slice(app.indexOf('function render('),app.indexOf('function renderProviderLabel(')),context);
  vm.runInContext(dispatch,context);
  context.render();context.render(true);
  assert.deepEqual(forces,[false,true]);
  for(const action of ['model-row-add','reload-settings','discard-config']){
    await context.dispatch(action,{dataset:{group:'providers'}});
    assert.equal(forces.at(-1),true,action+' must force reconciliation');
  }
  context.render();assert.equal(forces.at(-1),false,'subsequent callbacks do not retain force');
}
performanceDiagnostics(true);for(let i=0;i<1100;i++)performanceSample('fixture',uiNow(),{sequence:i,credential:'NEVER_RECORD'});const metrics=performanceDiagnostics(false);assert.equal(metrics.samples.length,1000);assert.doesNotMatch(JSON.stringify(metrics),/NEVER_RECORD/);
assert.match(fs.readFileSync(new URL('./static/index.html',import.meta.url),'utf8'),/<dialog id="detail-dialog" aria-labelledby="dialog-title">/);
for(const name of ['asc','frida','web-analysis','cloakbrowser']){const source=fs.readFileSync(new URL('../../.trace/skills/'+name+'/SKILL.md',import.meta.url),'utf8'),description=source.match(/^description: (.+)$/m)?.[1];assert.match(description,/[\u4e00-\u9fff]/);assert.match(description,/[A-Za-z]{5}/)}
console.log('P1 frontend: row preservation/validation, revision drafts, adapter isolation, true sources, lazy detail identity, scroll, receipt semantics, fixed hashes and late replies.');
