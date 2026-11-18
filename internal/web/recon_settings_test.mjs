import assert from 'node:assert/strict';
import {reconSettings,readReconSettings,reconModelOptions,syncReconFields} from './static/recon-settings.js';

const config={provider:{model:'main',api_key:'[stored secret]',reasoning_efforts:['low']},providers:{research:{model:'research-model',api_key:'[stored secret]',model_capabilities:{'research-model':{reasoning_efforts:['high']},vision:{reasoning_efforts:['medium']}}}},role_models:{worker:{provider:'default',model:'main'},stage_routes:[{provider:'research',model:'vision'}],future_role:{retained:true}},search_sources:{fofa:{enabled:true,api_key:'[stored secret]',email:'[stored secret]',custom_field:'retained',max_requests:30}},mcp:[{name:'external'}],local_tools:[{name:'local'}],future_config:{retained:true}};
const original=structuredClone(config),defaults={max_concurrency:1,min_interval_ms:1000,max_pages:5,max_requests:20,max_results:1000};
function form(c=config){
 const fields={'recon-mode':'inherit','recon-provider':'research','recon-model':'research-model','recon-effort':'high'};
 for(const id of ['fofa','quake','shodan']){
  const s=c.search_sources?.[id]||{},key='source-'+id+'-';
  Object.assign(fields,{[key+'enabled']:s.enabled?'true':'false',[key+'api_key']:s.api_key||'',[key+'email']:s.email||'',[key+'base_url']:s.base_url||''});
  for(const [name,value] of Object.entries(defaults))fields[key+name]=String(s[name]??value);
 }
 const nodes=Object.fromEntries(Object.entries(fields).map(([id,value])=>[id,{value,dataset:{},innerHTML:''}]));
 nodes['recon-model-fields']={disabled:false};nodes['recon-model-options']={innerHTML:''};
 return {nodes,get:id=>nodes[id]};
}
const html=reconSettings(config);
assert.match(html,/id="recon-settings"/);assert.match(html,/id="recon-mode"/);
for(const id of ['fofa','quake','shodan'])assert.match(html,new RegExp('id="source-'+id+'-api_key" type="password"'));
assert.match(html,/每根会话请求上限/);assert.match(html,/每根会话请求结果上限/);
assert.match(html,/id="recon-model-fields" disabled/);
assert.doesNotMatch(html,/<textarea/,'ordinary collection configuration needs no JSON');
const controls=form();
let result=readReconSettings(config,controls.get);
assert.deepEqual(config,original,'reading the form must not mutate the cached configuration');
assert.deepEqual(result.role_models.stage_routes,config.role_models.stage_routes);
assert.deepEqual(result.role_models.future_role,config.role_models.future_role);
assert.deepEqual(result.providers,config.providers);assert.deepEqual(result.future_config,config.future_config);
assert.deepEqual(result.mcp,config.mcp);assert.deepEqual(result.local_tools,config.local_tools);
assert.equal(result.search_sources.fofa.api_key,'[stored secret]');assert.equal(result.search_sources.fofa.custom_field,'retained');
assert.equal(result.search_sources.quake,undefined,'saving unrelated settings does not fabricate disabled sources');
controls.nodes['recon-mode'].value='custom';result=readReconSettings(config,controls.get);
assert.deepEqual(result.role_models.recon,{provider:'research',model:'research-model',reasoning_effort:'high'});
controls.nodes['recon-mode'].value='inherit';assert.equal(readReconSettings(result,controls.get).role_models.recon,undefined);
controls.nodes['source-fofa-api_key'].value='';assert.equal(readReconSettings(config,controls.get).search_sources.fofa.api_key,'','explicitly clearing a key is preserved');
controls.nodes['source-fofa-enabled'].value='false';assert.equal(readReconSettings(config,controls.get).search_sources.fofa.enabled,false);
controls.nodes['source-fofa-max_requests'].value='0';assert.throws(()=>readReconSettings(config,controls.get),/请求上限/);
controls.nodes['source-fofa-max_requests'].value='25';controls.nodes['source-fofa-max_results'].value='NaN';assert.throws(()=>readReconSettings(config,controls.get),/结果上限/);
controls.nodes['source-fofa-max_results'].value='500';controls.nodes['source-fofa-api_key'].dataset.configConflict='true';assert.throws(()=>readReconSettings(config,controls.get),/配置已更新/);
delete controls.nodes['source-fofa-api_key'].dataset.configConflict;
controls.nodes['recon-mode'].value='custom';controls.nodes['recon-effort'].value='low';assert.throws(()=>readReconSettings(config,controls.get),/未声明支持/);
controls.nodes['recon-effort'].value='';controls.nodes['recon-provider'].value='missing';assert.throws(()=>readReconSettings(config,controls.get),/已配置的 Provider/);
assert.deepEqual(reconModelOptions(config,'research','vision').efforts,['medium']);
controls.nodes['recon-provider'].value='default';syncReconFields(config,'recon-provider',controls.get);
assert.equal(controls.nodes['recon-model'].value,'main');assert.equal(controls.nodes['recon-effort'].value,'');
assert.equal(controls.nodes['recon-model-fields'].disabled,false);
assert.match(controls.nodes['recon-effort'].innerHTML,/low/);assert.doesNotMatch(controls.nodes['recon-effort'].innerHTML,/high/);
controls.nodes['recon-mode'].value='inherit';syncReconFields(config,'recon-mode',controls.get);assert.equal(controls.nodes['recon-model-fields'].disabled,true);
assert.doesNotMatch(reconSettings({...config,role_models:{recon:{provider:'<script>',model:'<script>'}}}),/<script>/);
assert.deepEqual(readReconSettings(config,()=>null),config,'legacy forms retain collection settings');
console.log('U6 collection forms: independent model, shared-source limits, secrets, unknown fields, conflicts and capability validation PASS');
