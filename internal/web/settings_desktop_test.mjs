import assert from 'node:assert/strict';
import {settings,readConfigForm} from './static/settings.js';
import {integrationImport,integrationFeedback,readIntegration,sourceLinks} from './static/integrations.js';
import {createProviderProbe} from './static/provider-probe.js';
import {localToolDetail} from './static/local-tools.js';

const config={provider:{url:'https://fixture.invalid/v1',model:'fixture',api_key:'[stored secret]'},context:{window_tokens:250000,output_reserve:16384,compaction_mode:'semantic'},mcp:[],local_tools:[{name:'local-command',command:'python',args:['-c','print("LONG_COMMAND_FIXTURE")'],description:'Readable local tool'}],skill_paths:['/srv/skills'],prompt_path:'/srv/prompt.md'};
const state={page:'settings',settingTab:'provider',config,tools:[],skills:[],runs:[]};
const providerHTML=settings(state);
assert.match(providerHTML,/<details id="provider-advanced-config" class="config-section">/);
assert.ok(providerHTML.indexOf('id="provider-probe-button"')<providerHTML.indexOf('id="provider-advanced-config"'));
assert.ok(providerHTML.indexOf('id="prompt-path"')>providerHTML.indexOf('id="provider-advanced-config"'));
assert.match(providerHTML,/Trace 服务所在机器/);
assert.match(providerHTML,/留空密钥会清除已有值/);
assert.doesNotMatch(providerHTML,/只保存在本机/);

const mcpHTML=settings({...state,settingTab:'tools'});assert.doesNotMatch(mcpHTML,/data-action="local-detail"|id="local-json"/);
const toolsHTML=settings({...state,settingTab:'local'});
assert.match(toolsHTML,/data-action="local-detail"/);
assert.doesNotMatch(toolsHTML,/id="local-tool-catalog"/);
const localHTML=localToolDetail(config,[],0,0);
assert.match(localHTML,/<details id="local-command-0" class="request-details">/);
assert.match(localHTML,/每行是一个完整参数/);
assert.doesNotMatch(toolsHTML,/<p class="mono">python/);
assert.ok(localHTML.indexOf('LONG_COMMAND_FIXTURE')>localHTML.indexOf('id="local-command-0"'));
assert.match(toolsHTML,/<details id="advanced-tool-config">/);
const skillsHTML=settings({...state,settingTab:'skills'});
assert.match(skillsHTML,/重新保存根目录以发现技能/);
assert.match(skillsHTML,/每行一个服务端绝对路径/);
assert.match(settings({...state,settingTab:'skills',skills:[{name:'without-description',local_path:'/srv/unique-root'}]}),new RegExp('data-skill-search="[^"]*/srv/unique-root'));
assert.match(settings({...state,settingTab:'skills',skills:[{name:'without-description'}]}),/暂无技能简介/);

const values={'provider-url':config.provider.url,'provider-model':'new-model','provider-key':'[stored secret]','prompt-path':config.prompt_path,'context-window':'250000','model-window':'0','context-reserve':'16384','compaction-mode':'projection','mcp-json':'[]','local-json':'[]','skill-paths':'/srv/a\n/srv/b'};
globalThis.document={getElementById:id=>Object.hasOwn(values,id)?{value:values[id]}:null};
try{
  const updated=readConfigForm(config,'provider');
  assert.equal(updated.provider.api_key,'[stored secret]');
  assert.equal(updated.provider.model,'new-model');
  assert.equal(config.provider.model,'fixture');
  assert.deepEqual(updated.local_tools,config.local_tools);
  assert.equal(updated.context.model_max_tokens,0);
  values['provider-key']='';assert.equal(readConfigForm(config,'provider').provider.api_key,'');
  for(const invalid of ['{}','null','"text"']){values['mcp-json']=invalid;assert.throws(()=>readConfigForm(config,'tools'),/MCP 连接配置应为 JSON 数组/)}
  values['mcp-json']='[';assert.throws(()=>readConfigForm(config,'tools'),/MCP 连接配置不是有效 JSON/);
  values['mcp-json']='[]';values['local-json']='{}';assert.throws(()=>readConfigForm(config,'local'),/本地工具 Manifest应为 JSON 数组/);
  assert.deepEqual(readConfigForm(config,'skills').skill_paths,['/srv/a','/srv/b']);
}finally{delete globalThis.document}

const input={'integration-url':'https://github.com/example/repo','integration-name':'fixture','integration-command':'','integration-args':'--stdio','integration-env':''};
const form={dataset:{kind:'mcp'},querySelector:selector=>({value:input[selector.slice(1)],checked:false})};
assert.throws(()=>readIntegration(form),/同时填写启动命令/);
input['integration-args']='';input['integration-env']='TOKEN=a=b';assert.throws(()=>readIntegration(form),/同时填写启动命令/);
input['integration-command']='node';const imported=readIntegration(form);
assert.deepEqual(imported.mcp.env,{TOKEN:'a=b'});assert.equal(imported.disabled,true);
assert.match(integrationImport('mcp'),/替代仓库启动配置，不与其合并/);
assert.match(integrationImport('skill'),/项目下载到 Trace 服务所在机器/);
assert.match(integrationFeedback({integrationFeedback:{message:'failure',error:true}}),/role="alert"/);
assert.match(sourceLinks({path:'/srv/skills/example/SKILL.md'}),/aria-label="复制Skill 文件/);

const elements=Object.fromEntries(['provider-url','provider-model','provider-key','provider-probe-status','provider-probe-button','provider-probe-cancel'].map(id=>[id,{value:'',dataset:{}}]));
for(const [field,value]of Object.entries(config.provider))elements['provider-'+({api_key:'key'}[field]||field)].value=value;
const notifications=[];let complete;
const probe=createProviderProbe({getState:()=>state,getElement:id=>elements[id],onChange:value=>notifications.push(value),api:()=>new Promise(resolve=>{complete=resolve})});
probe.sync();assert.equal(notifications.at(-1).status,'unprobed');assert.equal(notifications.at(-1).matchesSavedConfig,true);
const pending=probe.start();assert.equal(notifications.at(-1).status,'probing');
complete({status:'success',detail:'短请求有响应',elapsed_ms:9,checked_at:'2026-10-14T00:00:00Z'});await pending;
assert.equal(notifications.at(-1).status,'success');assert.equal(notifications.at(-1).checked_at,'2026-10-14T00:00:00Z');
elements['provider-model'].value='draft-only';probe.sync();
assert.equal(notifications.at(-1).matchesSavedConfig,false);assert.equal(notifications.at(-1).status,'unprobed');
assert.match(elements['provider-probe-status'].textContent,/未保存草稿尚未检测/);
const cancelled=probe.start();probe.cancel();complete({status:'success',detail:'stale'});await cancelled;
assert.equal(notifications.at(-1).status,'failed');assert.doesNotMatch(elements['provider-probe-status'].textContent,/0 ms/);
state.page='history';probe.sync();assert.equal(notifications.at(-1).status,'unprobed');assert.equal(notifications.at(-1).matchesSavedConfig,true);
assert.doesNotMatch(JSON.stringify(notifications),/stored secret|fixture.invalid|api_key/);
state.config=null;probe.sync();assert.equal(notifications.at(-1).status,'unconfigured');
console.log('Desktop settings checks passed: progressive disclosure, server paths, recoverable imports, secret preservation, and credential-free probe status.');
