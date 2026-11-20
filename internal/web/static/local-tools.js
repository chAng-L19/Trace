import {esc,btn} from './ui.js';
import {sourceLinks} from './integrations.js';
import {toolDetail} from './workforms.js';

// Only the two supported browser projects belong beside MCP services.
export function localToolGroups(config,tools=[]){
  const groups=new Map();
  (config.local_tools||[]).forEach((manifest,index)=>{
    const project=(manifest.github_url||'').match(/^https:\/\/github\.com\/(lightpanda-io\/browser|cloakhq\/cloakbrowser)(?:\.git)?\/?(?:[?#].*)?$/i)?.[1]?.toLowerCase();
    if(!project)return;
    if(!groups.has(project))groups.set(project,{key:project,name:project==='lightpanda-io/browser'?'lightpanda':'cloakbrowser',source:manifest,members:[]});
    const tool=tools.find(t=>t.adapter==='local'&&t.local_name===manifest.name);
    groups.get(project).members.push({manifest,index,tool});
  });
  return [...groups.values()];
}
const description=group=>group.members.length===1?(group.source.description||'浏览网页、提取内容或执行页面交互。'):`包含 ${group.members.length} 个工具接口；进入目录查看用途与参数。`;
const enabled=group=>group.members.filter(({manifest})=>!manifest.disabled).length;
const registered=group=>group.members.filter(item=>item.tool).length;

export function toggleLocalGroup(config,key){
  const group=localToolGroups(config).find(group=>group.key===key);
  if(!group)throw new Error('此浏览器扩展已更新，请返回目录');
  const next=structuredClone(config),disabled=enabled(group)>0;
  for(const {index} of group.members)next.local_tools[index].disabled=disabled;
  return next;
}
export function integrationCatalogSearch(){
  return `<div class="toolbar integration-catalog-search"><label>搜索接入目录<input id="integration-filter" type="search" placeholder="服务、工具名称、用途或来源…"></label><p id="integration-match-count" class="note" role="status"></p></div>`;
}
export function catalogTools(config,tools=[]){
  const names=new Set(localToolGroups(config,tools).flatMap(group=>group.members.map(({manifest})=>manifest.name)));
  return tools.filter(tool=>tool.adapter==='mcp'||tool.adapter==='local'&&names.has(tool.local_name));
}
export function localToolsCatalog(config,tools=[]){
  const groups=localToolGroups(config,tools);
  return `<section class="mcp-management local-management"><div class="section-label">浏览器扩展 <span>${groups.length} 个扩展 · ${groups.reduce((total,group)=>total+group.members.length,0)} 个工具接口</span></div><p class="note">支持 Lightpanda 和 CloakBrowser。开关控制扩展的全部工具接口，停用保留配置；变更需先暂停运行和 Worker。</p><div class="mcp-services">${groups.map((group,index)=>`<article class="mcp-service" data-integration-kind="local" data-integration-search="${esc([group.name,group.source.github_url,...group.members.flatMap(({manifest,tool})=>[manifest.name,manifest.description,tool?.definition.function.name])].join(' ').toLowerCase())}"><button type="button" class="skill-title" data-action="local-detail" data-index="${index}" aria-label="查看 ${esc(group.name)} 浏览器扩展详情"><header><b>${esc(group.name)}</b><span class="badge">浏览器扩展</span></header><p>${esc(description(group))}</p></button><footer><label class="availability-switch"><input type="checkbox" role="switch" data-switch data-action="local-toggle" data-group="${esc(group.key)}" ${enabled(group)?'checked':''} aria-label="${esc(group.name)} 浏览器扩展启用"><span>${enabled(group)?'停用扩展':'启用扩展'}</span></label><span class="status ${enabled(group)?'enabled':'unknown'}">${enabled(group)?`${enabled(group)} / ${group.members.length} 已启用`:'已停用'} · ${registered(group)} / ${group.members.length} 已注册</span>${btn('local-tools',`${group.members.length} 个工具接口`,`data-index="${index}"`)}</footer></article>`).join('')}</div><p class="empty catalog-empty" data-integration-empty="local" ${groups.length?'hidden':''}>${groups.length?'没有匹配的浏览器扩展；调整搜索条件后重试。':'尚未配置支持的浏览器扩展。'}</p></section>`;
}
export function localDetail(config,tools,index){
  const group=localToolGroups(config,tools)[index];
  return `<div class="buttons">${btn('integration-catalog','← 返回接入目录')}</div><p class="service-description">${esc(description(group))}</p><dl class="service-info"><dt>接入类型</dt><dd>浏览器扩展</dd><dt>工具接口</dt><dd>${group.members.length} 个接口 · ${registered(group)} 个已注册</dd><dt>启用状态</dt><dd>${enabled(group)} / ${group.members.length} 个接口已启用</dd></dl>${sourceLinks({github_url:group.source.github_url})}<p class="note">已注册表示接口已加入 Agent 目录，实际结果见任务中的工具调用记录。</p><div class="buttons">${btn('local-tools',`查看工具接口（${group.members.length}）`,`data-index="${index}"`)}</div>`;
}
export function localTools(config,tools,index){
  const group=localToolGroups(config,tools)[index];
  return `<div class="toolbar"><div>${btn('local-detail','← 浏览器扩展详情',`data-index="${index}"`)}</div><label>搜索 ${esc(group.name)} 的工具接口<input id="service-tool-filter" type="search" placeholder="名称、用途或接口 ID…"></label><span id="service-tool-count" class="note" role="status">${group.members.length} 个接口</span></div><div class="service-tools">${group.members.map(({manifest,index:manifestIndex,tool})=>`<button type="button" class="tool-card" data-service-search="${esc([manifest.name,manifest.description,tool?.definition.function.name].join(' ').toLowerCase())}" data-action="local-tool-detail" data-index="${index}" data-manifest-index="${manifestIndex}"><h3>${esc(manifest.name)}</h3><p>${esc(manifest.description||'未提供用途说明。')}</p><small>${tool?'工具接口已注册':'工具接口尚未注册'}</small></button>`).join('')}</div><p id="service-tool-empty" class="empty" hidden>没有匹配的工具接口；调整搜索条件后重试。</p>`;
}
export function localToolDetail(config,tools,index,manifestIndex){
  const member=localToolGroups(config,tools)[index]?.members.find(item=>item.index===manifestIndex);
  if(!member)throw new Error('此浏览器扩展已更新，请返回目录');
  const {manifest,tool}=member;
  const definition={adapter:'local',local_name:manifest.name,server_name:'浏览器扩展',definition:tool?.definition||{function:{description:manifest.description,parameters:manifest.input_schema||{type:'object'}}}};
  return `<div class="buttons">${btn('local-tools','← 返回工具接口目录',`data-index="${index}"`)}</div><p class="note">浏览器扩展 · ${manifest.disabled?'已停用 · 配置保留':tool?'工具接口已注册':'配置已保存，工具接口尚未注册'} · 执行结果以任务调用记录为准。</p>${toolDetail(definition)}${sourceLinks({github_url:manifest.github_url})}`;
}
export function filterIntegrationCatalog(){
  const query=(document.getElementById('integration-filter')?.value||'').trim().toLowerCase();
  const cards=[...document.querySelectorAll('[data-integration-search]')];
  for(const card of cards)card.hidden=!!query&&!card.dataset.integrationSearch.includes(query);
  const count=document.getElementById('integration-match-count');
  const totals=kind=>{const all=cards.filter(card=>card.dataset.integrationKind===kind);return `${all.filter(card=>!card.hidden).length} / ${all.length}`};
  if(count)count.textContent=`MCP 服务 ${totals('mcp')} · 浏览器扩展 ${totals('local')}`;
  for(const empty of document.querySelectorAll('[data-integration-empty]'))empty.hidden=cards.some(card=>card.dataset.integrationKind===empty.dataset.integrationEmpty&&!card.hidden);
}
