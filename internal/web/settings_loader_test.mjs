import assert from 'node:assert/strict';
import {createSettingsLoader,settingsFields} from './static/settings-loader.js';
import {settings} from './static/settings.js';
import {toolIndex,mcpForm,mcpDetail,mcpTools,toolDetail} from './static/workforms.js';
import {rememberInput,redraw} from './static/interactions.js';

const config={provider:{url:'',model:'',api_key:''},context:{},mcp:[{name:'fixture',command:'fixture'}],local_tools:[],skill_paths:[]};
const makeTool=(name,adapter='mcp')=>({adapter,server_name:adapter==='mcp'?'fixture':'',remote_name:name,definition:{function:{name,description:'用途 '+name,parameters:{type:'object',properties:{value:{type:'string'}}}}}});
const tools=[makeTool('builtin','builtin'),makeTool('alpha'),makeTool('beta'),makeTool('local','local')];
const state={page:'settings',settingTab:'provider',config:null,tools:[],skills:[],runs:[],selected:'run'};
const queue=[];
const loader=createSettingsLoader({state,api:path=>new Promise((resolve,reject)=>queue.push({path,resolve,reject}))});
const next=path=>{const index=queue.findIndex(request=>request.path===path);assert.notEqual(index,-1,`missing request ${path}`);return queue.splice(index,1)[0]};
const settle=async()=>{await Promise.resolve();await Promise.resolve()};
assert.deepEqual(settingsFields('provider'),['config']);
const boot=loader.load();assert.deepEqual(queue.map(row=>row.path),['/config']);next('/config').resolve(config);await boot;
await loader.load();assert.equal(queue.length,0);

state.settingTab='tools';
const first=loader.load('tools'),duplicate=loader.load('tools');
assert.deepEqual(queue.map(row=>row.path),['/tools','/mcp/status']);
assert.match(settings(state),/正在读取本页设置/);
state.settingTab='skills';const skills=loader.load('skills');
assert.deepEqual(queue.map(row=>row.path),['/tools','/mcp/status','/skills']);
next('/tools').resolve(tools);next('/mcp/status').reject(new Error('status unavailable'));next('/skills').resolve([{name:'fixture-skill'}]);
await Promise.all([first,duplicate,skills]);
assert.equal(state.settingsError,'');assert.doesNotMatch(settings(state),/status unavailable/);
state.settingTab='tools';assert.match(settings(state),/status unavailable/);
const retry=loader.load('tools');assert.deepEqual(queue.map(row=>row.path),['/mcp/status']);next('/mcp/status').resolve([]);await retry;
assert.equal(state.settingsError,'');

// Provider-only save still replaces Agent, but does not reread unrelated skills.
const updated={...config,provider:{...config.provider,model:'changed'}};
loader.saved(updated);assert.equal(state.config,updated);assert.equal(state.settingsResources.skills.loaded,true);
state.settingTab='provider';const saved=loader.load('provider');assert.deepEqual(queue.map(row=>row.path),['/config']);
next('/config').resolve(updated);await saved;
state.settingTab='tools';const rebuilt=loader.load('tools');assert.deepEqual(queue.map(row=>row.path),['/tools','/mcp/status']);
next('/tools').resolve(tools);next('/mcp/status').resolve([]);await rebuilt;

// An in-flight read invalidated by a save must never overwrite the newer config.
const oldRead=loader.load('provider',true),oldRequest=next('/config');
const newer={...updated,provider:{...updated.provider,model:'newer'}};loader.saved(newer);
oldRequest.resolve(config);await settle();assert.equal(state.config,newer);
assert.deepEqual(queue.map(row=>row.path),['/config']);next('/config').resolve(newer);await oldRead;
loader.saved(newer,{skills:true});assert.equal(state.settingsResources.skills.loaded,false);
const sameRoots=loader.load('skills');assert.deepEqual(queue.map(row=>row.path),['/config','/skills']);next('/config').resolve(newer);next('/skills').resolve([]);await sameRoots;
loader.saved({...newer,disabled_skills:['fixture-skill']});assert.equal(state.settingsResources.skills.loaded,false);

// Refresh failures retain prior catalogs and editable forms, rather than fake emptiness.
state.settingTab='provider';const reload=loader.load('provider');
next('/config').reject(new Error('offline'));await reload;assert.match(settings(state),/id="config-form"/);assert.match(settings(state),/offline/);
const recover=loader.load('provider');next('/config').resolve(newer);await recover;assert.equal(state.settingsError,'');

const index=toolIndex(tools);assert.equal(toolIndex(tools),index);assert.notEqual(toolIndex([...tools]),index);
assert.deepEqual(index.mcp.get('fixture').map(row=>row.i),[1,2]);assert.equal(index.external.length,3);
const cards=mcpForm(config,tools);assert.doesNotMatch(cards,/mcp-tool-list|data-action="tool-detail"|data-action="mcp-tools"/);
assert.match(cards,/data-action="mcp-detail"/);assert.match(cards,/2 个工具接口/);assert.match(cards,/用途 alpha/);
assert.match(cards,/data-action="mcp-detail" data-index="0" data-service="fixture"/);
assert.match(cards,/data-action="mcp-toggle" data-index="0" data-service="fixture"/);
assert.match(mcpDetail(config,tools,0),/data-action="mcp-tools"/);
for(const action of ['mcp-edit','mcp-toggle','mcp-tools'])assert.match(mcpDetail(config,tools,0),new RegExp(`data-action="${action}" data-index="0" data-service="fixture"`));
assert.match(mcpTools(config,tools,0),/data-action="tool-detail" data-index="1"/);
assert.match(mcpTools(config,tools,0),/data-mcp-index="0" data-service="fixture" data-tool-name="alpha"/);
assert.match(mcpTools(config,tools,0),/data-action="mcp-detail" data-index="0" data-service="fixture"/);
assert.match(mcpForm({...config,mcp:[{name:'fixture"quoted'}]},[]),/data-service="fixture&quot;quoted"/);
assert.match(toolDetail(tools[1],'data-index="0"'),/输入参数/);
const view={...state,settingsResources:undefined,settingsError:'',config,tools,settingTab:'tools',snap:{run:{id:'run',goal:'fixture goal'}}};
assert.match(settings(view),/自动目录（超过上下文预算时转为按需加载）/);
view.snap.run.tool_selection={ids:[]};assert.match(settings(view),/无外部 schema/);
view.snap.run.tool_selection={ids:['alpha']};assert.match(settings(view),/data-tool-id="alpha" checked/);
assert.equal((settings(view).match(/data-tool-id="alpha"/g)||[]).length,1);

// Page changes and late background loads reuse, rather than clear, form drafts.
const input={id:'provider-model',value:'unsaved-model',checked:false,readOnly:false,dataset:{},tagName:'INPUT'};
const page={innerHTML:'',querySelectorAll:selector=>selector.startsWith('input[id]')?[input]:[]};
const workspace={scrollTop:0,scrollTo(){}};
globalThis.document={getElementById:id=>id==='page'?page:id==='workspace'?workspace:null,querySelector:()=>null,querySelectorAll:()=>[]};
try{
  const providerState={page:'settings',settingTab:'provider'};rememberInput(providerState,input);
  redraw(providerState,'provider');redraw({...providerState,settingTab:'skills'},'skills');
  input.value='saved-model';redraw(providerState,'provider-after-load');assert.equal(input.value,'unsaved-model');
}finally{delete globalThis.document}
console.log('Settings loader checks passed: page reads, single flight, stale invalidation, retry, MCP path and run selection states.');
