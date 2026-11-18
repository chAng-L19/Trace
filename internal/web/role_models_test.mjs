import assert from 'node:assert/strict';
import {settings,readConfigForm} from './static/settings.js';
const config={provider:{url:'https://fixture.invalid',model:'base',api_key:'[stored secret]',reasoning_efforts:['low']},context:{},role_models:{worker:{provider:'fast',model:'worker'}},providers:{fast:{url:'https://worker.invalid',model:'worker',api_key:'[stored secret]'}}};
assert.match(settings({config,settingTab:'provider',tools:[],skills:[],runs:[]}),/id="role-models"/);
const values={'provider-url':config.provider.url,'provider-model':'base','provider-key':'[stored secret]','prompt-path':'','context-window':'250000','model-window':'0','context-reserve':'16384','compaction-mode':'projection','role-models':JSON.stringify(config.role_models),'named-providers':JSON.stringify(config.providers),'provider-reasoning':'["low"]'};
globalThis.document={getElementById:id=>Object.hasOwn(values,id)?{value:values[id]}:null};
values['provider-model-capabilities']='{"old":{"image_input":true,"reasoning_efforts":["low"]}}';
try{
 const result=readConfigForm(config,'provider');
 assert.deepEqual(result.role_models,config.role_models);
 assert.deepEqual(result.providers,config.providers);
 assert.deepEqual(result.provider.reasoning_efforts,['low']);
 assert.deepEqual(result.provider.model_capabilities,{old:{image_input:true,reasoning_efforts:['low']}});
 for(const bad of ['null','[]','"text"']){values['role-models']=bad;assert.throws(()=>readConfigForm(config,'provider'),/JSON 对象/)}
 values['role-models']='{}';values['named-providers']='null';assert.throws(()=>readConfigForm(config,'provider'),/JSON 对象/);
}finally{delete globalThis.document}
console.log('role model configuration UI passed');
