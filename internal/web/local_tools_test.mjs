import assert from 'node:assert/strict';
import {toggleLocalGroup,catalogTools,localToolGroups,localToolsCatalog,localDetail,localTools,localToolDetail,filterIntegrationCatalog} from './static/local-tools.js';
import {mergeToolSelection} from './static/config-revision.js';
import {settings} from './static/settings.js';
import {mcpTools,mcpForm} from './static/workforms.js';

const config={local_tools:[
  {name:'droidasc',github_url:'https://github.com/MG1937/ASC',command:'python3'},
  {name:'ast-grep',github_url:'https://github.com/example/web-analysis',local_path:'/srv/web-analysis',command:'node'},
  {name:'http-request',github_url:'https://github.com/lexiforest/curl_cffi',command:'python3'},
  {name:'page-fetch',github_url:'https://github.com/lexiforest/curl_cffi',command:'python3'},
  {name:'standalone',command:'node',local_path:'/srv/venv/bin'},
  {name:'lightpanda',github_url:'https://github.com/lightpanda-io/browser',local_path:'/srv/PRIVATE_BROWSER_PATH',dir:'/srv/PRIVATE_WORKDIR',description:'Browse pages',command:'PRIVATE_PROGRAM',args:['PRIVATE_ARGUMENT'],input_schema:{type:'object',properties:{source:{type:'string',description:'Page source'}},required:['source']}},
  {name:'cloakbrowser',github_url:'https://github.com/CloakHQ/CloakBrowser',command:'PRIVATE_PROGRAM',args:['PRIVATE_ARGUMENT'],mode:'stdio'},
  {name:'browser-read',github_url:'https://github.com/lightpanda-io/browser.git',local_path:'/srv/second-checkout',command:'python3'},
  {name:'lightpanda-spoof',github_url:'https://github.com/example/other',command:'python3'},
],mcp:[{name:'analysis',command:'node'}]};
const hiddenNames=['droidasc','ast-grep','http-request','page-fetch','standalone','lightpanda-spoof'];
const tools=[{adapter:'mcp',local_name:'browser-read',server_name:'analysis',definition:{function:{name:'mcp_trace',description:'Network trace'}}},
  ...hiddenNames.map(name=>({adapter:'local',local_name:name,definition:{function:{name:'local_'+name}}})),
  ...['lightpanda','cloakbrowser'].map(name=>({adapter:'local',local_name:name,definition:{function:{name:'local_'+name,description:'Browse pages',parameters:config.local_tools[5].input_schema}}})),
];
const groups=localToolGroups(config,tools);
assert.deepEqual(groups.map(group=>group.name),['lightpanda','cloakbrowser']);
assert.deepEqual(groups[0].members.map(item=>item.index),[5,7],'project identity groups browser interfaces across checkout paths');
assert.equal(groups[0].members[1].tool,undefined,'same-name MCP interface is not a registered browser interface');
const cards=localToolsCatalog(config,tools);
assert.equal((cards.match(/data-integration-kind="local"/g)||[]).length,2);
assert.match(cards,/2 个扩展 · 3 个工具接口/);
assert.match(cards,/1 \/ 2 已注册/);
assert.doesNotMatch(cards,/droidasc|ast-grep|http-request|page-fetch|standalone|lightpanda-spoof/);
const detail=localToolDetail(config,tools,0,5);
assert.match(detail,/data-action="local-tools"/);
assert.match(detail,/<td class="mono">source<\/td>/);
assert.match(detail,/href="https:\/\/github.com\/lightpanda-io\/browser"/);
assert.match(localToolDetail(config,tools,0,7),/接口尚未注册/);
assert.throws(()=>localToolDetail(config,tools,0,0),/此浏览器扩展已更新/);
assert.match(localTools(config,tools,0),/data-manifest-index="5"/);
assert.match(localTools(config,tools,0),/data-manifest-index="7"/);
assert.match(localTools(config,tools,1),/data-manifest-index="6"/);
for(const html of [cards,localDetail(config,tools,0),localDetail(config,tools,1),localTools(config,tools,0),localTools(config,tools,1),detail,localToolDetail(config,tools,1,6)]){
  assert.match(html,/浏览器扩展|工具接口/);
  assert.doesNotMatch(html,/本地命令|ARGV|JSONL|命令接口|启动配置|启动命令|执行位置|执行工作目录|local-command-|PRIVATE_|\/srv\/|python3/);
}
assert.match(localToolsCatalog({local_tools:[]}),/尚未配置支持的浏览器扩展/);
const visible=catalogTools(config,tools),scope=visible.map(tool=>tool.definition.function.name);
assert.deepEqual(scope,['mcp_trace','local_lightpanda','local_cloakbrowser']);
const hiddenIDs=hiddenNames.map(name=>'local_'+name);
assert.deepEqual(mergeToolSelection(undefined,tools,scope,[]),hiddenIDs,'clearing visible choices retains hidden command IDs');
assert.deepEqual(mergeToolSelection(['local_http-request','unknown-id','local_lightpanda'],tools,scope,['local_cloakbrowser']),['local_http-request','unknown-id','local_cloakbrowser']);
for(const settingTab of ['tools','local']){
  const page=settings({config,tools,settingTab,runs:[],mcpStatuses:[],snap:{run:{id:'run',goal:'fixture',tool_selection:{ids:['local_http-request','local_lightpanda']}}}});
  assert.equal((page.match(/data-integration-kind="local"/g)||[]).length,2);
  assert.match(page,/data-tool-id="mcp_trace"/);
  assert.match(page,/data-tool-id="local_lightpanda"/);
  assert.match(page,/data-tool-id="local_cloakbrowser"/);
  for(const id of hiddenIDs)assert.ok(!page.includes('data-tool-id="'+id+'"'));
  assert.doesNotMatch(page,/href="#local-tools"/);
}
const emptyMCP={...config,mcp:[]};
assert.equal((localToolsCatalog(emptyMCP,tools).match(/data-integration-kind="local"/g)||[]).length,2);
assert.match(mcpForm(config,tools),/MCP 服务 <span>1 个服务/);
assert.match(mcpForm(config,tools),/data-integration-search="[^"]*network trace/);
const multiTools=[...tools,{...tools[0],remote_name:'inspect',definition:{function:{name:'mcp_inspect',description:'检查页面'}}}];
const multiCard=mcpForm(config,multiTools);
assert.equal((multiCard.match(/<article class="mcp-service"/g)||[]).length,1);
assert.doesNotMatch(multiCard,/mcp-tool-list|data-action="tool-detail"|data-action="mcp-tools"/);
assert.match(mcpTools(config,multiTools,0),new RegExp('data-action="tool-detail" data-index="'+(multiTools.length-1)+'"'));
const browserKey=groups[0].key,disabled=toggleLocalGroup(config,browserKey);
assert.ok([5,7].every(index=>disabled.local_tools[index].disabled));
assert.ok([5,7].every(index=>config.local_tools[index].disabled===undefined));
assert.deepEqual(disabled.local_tools.filter((_,index)=>![5,7].includes(index)),config.local_tools.filter((_,index)=>![5,7].includes(index)));
assert.ok([5,7].every(index=>toggleLocalGroup(disabled,browserKey).local_tools[index].disabled===false));
const disabledCards=localToolsCatalog(disabled,tools.filter(tool=>tool.local_name!=='lightpanda'));
assert.match(disabledCards,/data-action="local-toggle"[^>]+aria-label="lightpanda 浏览器扩展启用"/);
assert.doesNotMatch(disabledCards,/data-action="local-toggle"[^>]+checked[^>]+aria-label="lightpanda 浏览器扩展启用"/);
assert.match(disabledCards,/已停用 · 0 \/ 2 已注册/);
assert.throws(()=>toggleLocalGroup(config,'missing-project'),/此浏览器扩展已更新/);
assert.equal(localToolGroups({local_tools:[{name:'lightpanda',github_url:'https://github.com.example.org/lightpanda-io/browser'},{name:'cloakbrowser',github_url:'https://github.com/example/other'}]}).length,0);
assert.equal(localToolGroups({local_tools:[{name:'renamed-browser',github_url:'https://github.com/CloakHQ/CloakBrowser/'}]})[0].name,'cloakbrowser');
const elements={'integration-filter':{value:'  LIGHTPANDA  '},'integration-match-count':{textContent:''}};
const searchCards=[{dataset:{integrationKind:'mcp',integrationSearch:'network trace'}},{dataset:{integrationKind:'local',integrationSearch:'lightpanda browse pages'}}];
const empties=['mcp','local'].map(kind=>({dataset:{integrationEmpty:kind},hidden:true}));
globalThis.document={getElementById:id=>elements[id],querySelectorAll:selector=>selector==='[data-integration-search]'?searchCards:empties};
try{
  filterIntegrationCatalog();
  assert.deepEqual(searchCards.map(card=>card.hidden),[true,false]);
  assert.deepEqual(empties.map(empty=>empty.hidden),[false,true]);
  assert.equal(elements['integration-match-count'].textContent,'MCP 服务 0 / 1 · 浏览器扩展 1 / 1');
  elements['integration-filter'].value='missing';filterIntegrationCatalog();
  assert.ok(empties.every(empty=>!empty.hidden));
  elements['integration-filter'].value='';filterIntegrationCatalog();
  assert.ok(searchCards.every(card=>!card.hidden));
}finally{delete globalThis.document}
console.log('Browser catalog checks passed: two supported projects, hidden GitHub commands, selector preservation, original indices, switches, private detail and MCP services.');
