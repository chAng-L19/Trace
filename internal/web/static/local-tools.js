import {esc,btn} from './ui.js';
import {sourceLinks,integrationActions} from './integrations.js';
import {toolDetail} from './workforms.js';

// ARTEX tools/page.tsx: source-aware card catalog and searchable detail entry.
// Muteki ExtensionSettings.tsx: registration and observed health stay distinct.
// Group only explicit project sources; command names are not project identity.
export function localToolGroups(config,tools=[]){
  const groups=new Map();
  (config.local_tools||[]).forEach((manifest,index)=>{
    const path=manifest.local_path||'',key=path?JSON.stringify([path,manifest.github_url||'']):`tool:${index}`;
    if(!groups.has(key))groups.set(key,{key,name:path?path.replace(/[\\/]+$/,'').split(/[\\/]/).pop()||manifest.name:manifest.name,source:manifest,members:[]});
    const tool=tools.find(t=>t.adapter==='local'&&t.local_name===manifest.name);
    groups.get(key).members.push({manifest,index,tool});
  });
  return [...groups.values()];
}
const modes=group=>[...new Set(group.members.map(({manifest})=>manifest.mode==='stdio'?'JSONL':'ARGV'))].join(' / ');
const description=group=>group.members.length===1?(group.source.description||'在 Trace 服务所在机器执行的本地命令。'):`包含 ${group.members.length} 个本地命令接口；进入目录查看各接口用途与参数。`;
const registered=group=>group.members.filter(item=>item.tool).length;

export function integrationCatalogSearch(){
  return `<div class="toolbar integration-catalog-search"><label>搜索接入目录<input id="integration-filter" type="search" placeholder="服务、工具名称、用途或来源…"></label><p id="integration-match-count" class="note" role="status"></p></div>`;
}
export function localToolsCatalog(config,tools=[]){
  const groups=localToolGroups(config,tools);
  return `<section class="mcp-management local-management"><div class="section-label">本地工具 <span>${groups.length} 个来源 · ${(config.local_tools||[]).length} 个命令接口</span></div><p class="note">与 MCP 共用目录浏览方式；本地命令在 Trace 服务所在机器执行，按调用启动，无 MCP 协议连接。</p><div class="mcp-services">${groups.map((group,index)=>`<article class="mcp-service" data-integration-kind="local" data-integration-search="${esc([group.name,group.source.github_url,group.source.local_path,...group.members.flatMap(({manifest,tool})=>[manifest.name,manifest.description,tool?.definition.function.name])].join(' ').toLowerCase())}"><button type="button" class="skill-title" data-action="local-detail" data-index="${index}" aria-label="查看 ${esc(group.name)} 本地工具详情"><header><b>${esc(group.name)}</b><span class="badge">本地 · ${modes(group)}</span></header><p>${esc(description(group))}</p></button><footer><span class="status unknown">${registered(group)} / ${group.members.length} 已注册</span>${btn('local-tools',`${group.members.length} 个命令接口`,`data-index="${index}"`)}</footer></article>`).join('')}</div><p class="empty catalog-empty" data-integration-empty="local" ${groups.length?'hidden':''}>${groups.length?'没有匹配的本地工具；调整搜索条件后重试。':'尚未配置本地工具；可在高级配置中添加命令 Manifest。'}</p></section>`;
}
export function localDetail(config,tools,index){
  const group=localToolGroups(config,tools)[index];
  return `<div class="buttons">${btn('integration-catalog','← 返回接入目录')}</div><p class="service-description">${esc(description(group))}</p><dl class="service-info"><dt>接入类型</dt><dd>本地命令 · ${modes(group)}</dd><dt>执行位置</dt><dd>Trace 服务所在机器</dd><dt>工具目录</dt><dd>${group.members.length} 个命令接口 · ${registered(group)} 个已注册</dd><dt>执行状态</dt><dd>按调用启动；本页不执行工具</dd></dl>${sourceLinks(group.source)}${integrationActions(group.source,'local',group.source.name)}<p class="note">已注册表示接口已加入 Agent 目录，不代表程序依赖、目标输入或实际执行已通过验证。执行结果见任务中的工具调用记录。</p><div class="buttons">${btn('local-tools',`查看工具（${group.members.length}）`,`data-index="${index}"`)}</div>`;
}
export function localTools(config,tools,index){
  const group=localToolGroups(config,tools)[index];
  return `<div class="toolbar"><div>${btn('local-detail','← 本地工具详情',`data-index="${index}"`)}</div><label>搜索 ${esc(group.name)} 的工具<input id="service-tool-filter" type="search" placeholder="名称、用途或接口 ID…"></label><span id="service-tool-count" class="note" role="status">${group.members.length} 个接口</span></div><div class="service-tools">${group.members.map(({manifest,index:manifestIndex,tool})=>`<button type="button" class="tool-card" data-service-search="${esc([manifest.name,manifest.description,tool?.definition.function.name].join(' ').toLowerCase())}" data-action="local-tool-detail" data-index="${index}" data-manifest-index="${manifestIndex}"><h3>${esc(manifest.name)}</h3><p>${esc(manifest.description||'未提供用途说明。')}</p><small>${tool?'已注册':'尚未注册'} · ${manifest.mode==='stdio'?'JSONL 输入':'命令行参数'}</small></button>`).join('')}</div><p id="service-tool-empty" class="empty" hidden>没有匹配的工具接口；调整搜索条件后重试。</p>`;
}
export function localToolDetail(config,tools,index,manifestIndex){
  const manifest=config.local_tools[manifestIndex],tool=tools.find(t=>t.adapter==='local'&&t.local_name===manifest.name);
  const definition=tool||{adapter:'local',local_name:manifest.name,definition:{function:{description:manifest.description,parameters:manifest.input_schema||{type:'object'}}}};
  return `<div class="buttons">${btn('local-tools','← 返回本地工具目录',`data-index="${index}"`)}</div><p class="note">本地命令 · ${tool?'接口已注册':'配置已保存，接口尚未注册'} · 执行结果以任务调用记录为准。</p>${toolDetail(definition)}<details id="local-command-${manifestIndex}" class="request-details"><summary>查看启动配置 · ${manifest.mode==='stdio'?'JSONL 输入':'命令行参数'} · ${(manifest.args||[]).length} 个参数</summary><dl class="service-info"><dt>程序</dt><dd><code>${esc(manifest.command)}</code></dd>${manifest.dir?`<dt>工作目录</dt><dd><code>${esc(manifest.dir)}</code></dd>`:''}</dl>${manifest.args?.length?`<p class="note">每行是一个完整参数；不会按空格再次拆分。</p><pre>${esc(manifest.args.join('\n'))}</pre>`:'<p class="note">此工具没有固定启动参数。</p>'}</details>`;
}
export function filterIntegrationCatalog(){
  const query=(document.getElementById('integration-filter')?.value||'').trim().toLowerCase();
  const cards=[...document.querySelectorAll('[data-integration-search]')];
  for(const card of cards)card.hidden=!!query&&!card.dataset.integrationSearch.includes(query);
  const count=document.getElementById('integration-match-count');
  const totals=kind=>{const all=cards.filter(card=>card.dataset.integrationKind===kind);return `${all.filter(card=>!card.hidden).length} / ${all.length}`};
  if(count)count.textContent=`MCP 服务 ${totals('mcp')} · 本地来源 ${totals('local')}`;
  for(const empty of document.querySelectorAll('[data-integration-empty]'))empty.hidden=cards.some(card=>card.dataset.integrationKind===empty.dataset.integrationEmpty&&!card.hidden);
}
