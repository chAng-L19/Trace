import assert from 'node:assert/strict';
import {activeProvider,providerProfiles,switchProviderProfile} from './static/provider-profiles.js';
import {modelSettings} from './static/model-settings.js';
import {readConfigForm} from './static/settings.js';
import {createSettingsLoader} from './static/settings-loader.js';
import fs from 'node:fs';
import vm from 'node:vm';

const config={revision:'r1',provider:{url:'https://default.invalid/v1',model:'legacy',api_key:'[stored secret]'},providers:{fast:{url:'https://fast.invalid/v1',model:'fast-model',api_key:'[stored secret]'},'quoted"<profile>':{url:'https://other.invalid/v1',model:'other'}},context:{},mcp:[],local_tools:[],role_models:{worker:{provider:'default',model:'legacy'}}};
const form=fields=>({querySelectorAll:()=>fields});
const switched=switchProviderProfile(config,'fast',form([]));
assert.equal(switched.active_provider,'fast');
assert.deepEqual({...switched,active_provider:undefined},{...config,active_provider:undefined});
assert.equal(config.active_provider,undefined);
assert.equal(activeProvider(switched).provider.model,'fast-model');
assert.equal(switchProviderProfile(switched,'default',form([])).active_provider,'');
assert.throws(()=>switchProviderProfile(config,'missing',form([])),/配置已变化/);
assert.throws(()=>switchProviderProfile(config,'fast',form([{dataset:{configBase:'old'},value:'draft'}])),/草稿/);
assert.throws(()=>switchProviderProfile(config,'fast',form([{dataset:{configChecked:'false'},checked:true}])),/草稿/);
assert.equal(switchProviderProfile(config,'fast',form([{dataset:{configBase:'same'},value:'same'},{dataset:{configChecked:'false'},checked:false}])).active_provider,'fast');
const html=providerProfiles(switched);
assert.match(html,/data-provider="fast" aria-pressed="true" disabled/);
assert.match(html,/quoted&quot;&lt;profile&gt;/);
assert.doesNotMatch(html,/api_key|stored secret|<profile>/);
const roles=modelSettings(switched);
assert.match(roles,/id="model-role-orchestrator-provider" data-config-base="fast"/);
assert.match(roles,/id="model-role-worker-provider" data-config-base="default"/);
assert.match(roles,/id="model-role-recon-provider" data-config-base="default"/);

// Rendered defaults must not become unrelated configuration edits.
const nodes={};const field=(id,value)=>nodes[id]={value,dataset:{configBase:value}};
for(const [id,value] of Object.entries({'provider-url':config.provider.url,'provider-model':'legacy','provider-key':'[stored secret]','prompt-path':'','context-window':'250000','model-window':'0','context-reserve':'16384','compaction-mode':'semantic','role-models':JSON.stringify(config.role_models),'named-providers':JSON.stringify(config.providers),'provider-reasoning':'[]','provider-model-capabilities':'{}','model-default-efforts':'','cost-currency':'','cost-pool':'','cost-rates':'[]'}))field(id,value);
nodes['provider-image-input']={checked:false,dataset:{configChecked:'false'}};
for(const source of ['fofa','quake','shodan'])for(const [key,value] of Object.entries({enabled:'false',api_key:'',email:'',base_url:'',max_concurrency:'1',min_interval_ms:'1000',max_pages:'5',max_requests:'20',max_results:'1000'}))field('source-'+source+'-'+key,value);
globalThis.document={getElementById:id=>nodes[id]||null,querySelectorAll:()=>[]};
try{
 assert.deepEqual(readConfigForm(config,'provider'),config);
 nodes['provider-key'].value='rotated-fixture';const saved=readConfigForm(config,'provider');
 assert.deepEqual(saved,{...config,provider:{...config.provider,api_key:'rotated-fixture'}});
 nodes['provider-key'].value='';assert.equal(readConfigForm(config,'provider').provider.api_key,'');
 nodes['context-window'].value='100000';assert.equal(readConfigForm(config,'provider').context.window_tokens,100000);
}finally{delete globalThis.document}
const state={config,settingTab:'tools'};const loader=createSettingsLoader({state,api:async path=>path==='/config'?switched:[]});
await loader.load('tools');
loader.saved(switched,{modelOnly:true});
assert.equal(state.settingsResources.tools.loaded,true);
assert.equal(state.settingsResources.mcpStatuses.loaded,true);
assert.equal(state.settingsResources.config.loaded,false);
await loader.load('provider');assert.equal(state.config,switched);
// The shell must never attribute a default-connection probe to a named profile.
const source=fs.readFileSync(new URL('./static/app.js',import.meta.url),'utf8');
const indicator={dataset:{}},context=vm.createContext({state,activeProvider,$:()=>indicator});
vm.runInContext(source.slice(source.indexOf('function modelConnection('),source.indexOf('function renderRunCount(')),context);
vm.runInContext("modelConnection({status:'success',matchesSavedConfig:true})",context);
assert.equal(indicator.dataset.status,'unprobed');assert.match(indicator.textContent,/fast-model.*fast.*未检测/);
state.config=config;vm.runInContext("modelConnection({status:'success',matchesSavedConfig:true})",context);
assert.equal(indicator.dataset.status,'success');assert.match(indicator.textContent,/legacy.*检测通过/);
console.log('Provider profile checks passed: pointer-only switching, draft guards, inherited role display, key-only form payload and retained tool catalogs.');
