import assert from 'node:assert/strict';
import {localToolGroups,localToolsCatalog,localDetail,localTools,localToolDetail,filterIntegrationCatalog} from './static/local-tools.js';
import {mcpDetail,mcpTools,mcpForm} from './static/workforms.js';

const project={local_path:'/srv/web-analysis',github_url:'https://github.com/example/analysis'};
const config={local_tools:[
  {...project,name:'parse',description:'Parse JavaScript',command:'node',args:['PRIVATE_ARGUMENT']},
  {...project,name:'trace',description:'Trace callers',command:'node',mode:'stdio'},
  {name:'standalone',command:'node'},
  {...project,local_path:'/srv/other',name:'other',command:'node'},
],mcp:[{name:'analysis',command:'node'}]};
const tools=[{adapter:'mcp',local_name:'trace',server_name:'analysis',definition:{function:{name:'mcp_trace',description:'Network trace'}}},
  {adapter:'local',local_name:'parse',definition:{function:{name:'local_parse',description:'Parse JavaScript',parameters:{type:'object',properties:{source:{type:'string',description:'Program source'}},required:['source']}}}}];
const groups=localToolGroups(config,tools);
assert.equal(groups.length,3,'only matching explicit project sources may share a card');
assert.equal(groups[0].members.length,2);
assert.equal(groups[0].members[1].tool,undefined,'same-name MCP interface is not a registered local tool');
const cards=localToolsCatalog(config,tools);
assert.equal((cards.match(/data-integration-kind="local"/g)||[]).length,3);
assert.match(cards,/3 个来源 · 4 个命令接口/);
assert.doesNotMatch(cards,/PRIVATE_ARGUMENT|data-action="mcp-probe"|<pre>/);
assert.match(cards,/1 \/ 2 已注册/);
assert.match(localDetail(config,tools,0),/按调用启动；本页不执行工具/);
assert.match(localTools(config,tools,0),/data-manifest-index="1"/);
const detail=localToolDetail(config,tools,0,0);
assert.match(detail,/data-action="local-tools"/);
assert.doesNotMatch(detail,/data-action="mcp-tools"/);
assert.match(detail,/<td class="mono">source<\/td>/);
assert.match(detail,/<details class="request-details"><summary>高级：完整工具定义/);
assert.match(detail,/<details id="local-command-0" class="request-details">/);
assert.match(localToolDetail(config,tools,0,1),/接口尚未注册/);
assert.match(localToolsCatalog({local_tools:[]}),/尚未配置本地工具/);
assert.match(mcpForm(config,tools),/MCP 服务 <span>1 个服务/);
assert.match(mcpForm(config,tools),/data-integration-search="[^"]*network trace/);
const multiTools=[...tools,{...tools[0],remote_name:'inspect',definition:{function:{name:'mcp_inspect',description:'检查页面'}}}];
const multiCard=mcpForm(config,multiTools);
assert.equal((multiCard.match(/<article class="mcp-service"/g)||[]).length,1);
assert.doesNotMatch(multiCard,/mcp-tool-list|data-action="tool-detail"|data-action="mcp-tools"/);
assert.match(mcpTools(config,multiTools,0),/data-action="tool-detail" data-index="2"/);

const elements={'integration-filter':{value:'  PARSE  '},'integration-match-count':{textContent:''}};
const searchCards=[{dataset:{integrationKind:'mcp',integrationSearch:'network trace'}},{dataset:{integrationKind:'local',integrationSearch:'parse javascript'}}];
const empties=['mcp','local'].map(kind=>({dataset:{integrationEmpty:kind},hidden:true}));
globalThis.document={getElementById:id=>elements[id],querySelectorAll:selector=>selector==='[data-integration-search]'?searchCards:empties};
try{
  filterIntegrationCatalog();
  assert.deepEqual(searchCards.map(card=>card.hidden),[true,false]);
  assert.deepEqual(empties.map(empty=>empty.hidden),[false,true]);
  assert.equal(elements['integration-match-count'].textContent,'MCP 服务 0 / 1 · 本地来源 1 / 1');
  elements['integration-filter'].value='missing';filterIntegrationCatalog();
  assert.ok(empties.every(empty=>!empty.hidden));
  elements['integration-filter'].value='';filterIntegrationCatalog();
  assert.ok(searchCards.every(card=>!card.hidden));
}finally{delete globalThis.document}
console.log('Local tool catalog checks passed: explicit-source grouping, separate counts, registration semantics, parameter detail, and shared search empty states.');
