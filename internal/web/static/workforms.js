import {reportDownloads} from './delivery.js';
import {duration,messageBody,reportText} from './presentation.js';
import {messageCallRecords,callKey,toolOutput,callStatus,callDiagnostics} from './conversation.js';
import {taskInputPicker} from './attachments.js';
import {verificationPanel} from './verification.js';
import {esc,json,field,btn,status,label,evidenceLinks} from './ui.js';
import {sourceLinks,integrationActions,integrationImport} from './integrations.js';
import {mcpStatusBody,mcpStateLabel} from './mcp-status.js';

export function resultPanel(snap){
  const r=snap.result||{},cleanup=r.cleanup_status;
  let summary=r.summary||'尚无结果摘要',summaryLabel='';
  try{const parsed=JSON.parse(summary.replace(/^```json\s*\n/,'').replace(/\n```\s*$/,''));if(typeof parsed.trace_result?.summary==='string'){summary=parsed.trace_result.summary;summaryLabel='模型摘要 · 验证结论以本页状态为准'}}catch{}
  if(reportText(r.report).trim())summary=JSON.stringify({trace_report:r.report});
  const reason=r.terminal_reason||snap.run.pause_reason||snap.run.error||'运行尚未产生结果';
  const reasons={'run has not produced a verified final result':'运行尚未产生已验证的最终结果','operator cancelled run':'用户已取消运行','model reply completed; target success has not been verified':'模型回合结束；目标成功尚未验证','tool results remain unresolved':'仍有工具调用结果待处理','child worker completion or cleanup remains unresolved':'子任务结束或清理状态仍待核对','planned tasks remain unresolved':'计划中仍有任务未完成','structured result reports failure':'结构化结果报告失败','structured success marker and referenced execution evidence verified':'结构化成功标记及其执行证据已验证','success claim has missing, invalid or unsupported evidence':'成功声明缺少有效证据支持','structured result reports incomplete work':'结构化结果报告工作尚未完成','persisted success predicate has not matched execution evidence':'已保存的成功条件尚未匹配执行证据','persisted stdout-contains success predicate matched verified execution evidence':'成功条件已匹配经过验证的执行输出','time budget exhausted':'累计运行时间预算已用尽','model request budget exhausted':'模型请求预算已用尽','tool action budget exhausted':'工具动作预算已用尽'};
  return `<section class="result-panel"><div class="buttons">${reportDownloads(snap)}${btn('export','完整 JSON ↓','data-format="json"')}${btn('export','证据 JSON ↓','data-format="evidence"')}</div><div class="section-label">运行结论 <span>${status(r.status)}</span></div>${summaryLabel?`<p class="note">${esc(summaryLabel)}</p>`:''}<div class="result-summary">${messageBody({id:'run-result-'+snap.run.id,content:summary})}</div><dl class="result-meta"><dt>结束 / 等待原因</dt><dd title="${esc(reason)}">${esc(reasons[reason]||reason)}</dd><dt>清理状态</dt><dd>${status(cleanup)}${cleanup==='unknown'?' · 尚无清理验证结论，运行结束不代表清理完成。':''}</dd></dl>${(r.answers||[]).map(a=>`<article class="answer"><header><b>${esc(a.title||({answer:'结论',flag:'结果标记'}[a.kind]||'结论条目'))}</b>${status(a.verified?'verified':'unverified')}</header>${a.summary?`<p>${esc(a.summary)}</p>`:''}<pre>${esc(a.value)}</pre><div class="buttons">${evidenceLinks(a.evidence_ids)}</div></article>`).join('')}<div class="buttons">${evidenceLinks(r.evidence_ids)}</div>${r.reproduction_steps?.length?`<details class="request-details" open><summary>复现步骤 / ${r.reproduction_steps.length}</summary><ol>${r.reproduction_steps.map(step=>`<li>${esc(step)}</li>`).join('')}</ol></details>`:''}${verificationPanel(snap)}</section>`;
}

export function budgetPanel(run){
  const b=run.budget||{},u=run.usage||{},limit=n=>n===undefined?'未记录':n===0?'不限':n;
  return `<section class="subsection"><div class="section-label">RUN BUDGET <span>已用 / 上限</span></div><dl class="key-values"><dt>模型请求</dt><dd>${u.model_requests||0} / ${limit(b.max_model_requests)}</dd><dt>工具动作</dt><dd>${u.tool_actions||0} / ${limit(b.max_actions)}</dd><dt>已结算运行秒数</dt><dd>${((run.elapsed_millis||0)/1000).toFixed(1)} / ${limit(b.max_seconds)}</dd></dl><p class="note">完成条件：${esc(run.completion_criteria||'依据目标与已保存验收要求')}</p><p class="note">精确输出谓词：${esc(run.success_contains||'未设置输出包含谓词；依据结构化结果与证据核验。')}</p>${btn('budget-help','设置此运行预算')}${btn('requirements-edit','查看 / 管理成功条件')}</section>`;
}

export function budgetHelp(run){
  const active=['running','waiting_tool'].includes(run.status),budget=run.budget||{};
  return `<p class="note">按当前运行的累计已用量设置上限。0 表示不限；父、子运行分别计数。</p><form id="budget-form" class="field-stack">${[['max_model_requests','模型请求上限'],['max_actions','工具动作上限'],['max_seconds','运行秒数上限']].map(([key,title])=>`<label>${title}<input id="budget-${key}" name="${key}" type="number" min="0" step="1" value="${budget[key]||0}" required ${active?'disabled':''}></label>`).join('')}<p class="note">${active?'先暂停当前运行，待执行结束后保存。':'只修改此运行；恢复时保留显式设置。上限按累计已用量计算；已耗尽时需调高上限或设为 0。保存不会自动恢复执行。'}</p><button class="primary" ${active?'disabled':''}>保存预算</button></form>`;
}

export function taskForm(tasks,inputs=[]){return `<details id="task-editor" class="section-gap" ${tasks.length?'':'open'}><summary>添加 / 编辑计划任务</summary><form id="task-form" class="field-stack"><label>任务目标<textarea id="task-goal" required placeholder="此任务需要完成什么？"></textarea></label><label>完成条件<textarea id="task-completion" placeholder="自然语言交付条件；不作为输出包含谓词"></textarea></label><details id="task-advanced"><summary>高级：任务 ID、执行方式、优先级、精确谓词与依赖</summary><div class="form-row">${field('task-id','任务 ID（可选，留空自动生成）')}<label>执行方式<select id="task-mode"><option value="explore">执行探索</option><option value="reason">分析推理</option></select></label><label>优先级<input id="task-priority" type="number" value="0"></label></div>${field('task-predicate','成功条件：工具输出必须包含（可选）')}${taskInputPicker(inputs)}<fieldset><legend>先完成这些任务（可多选）</legend>${tasks.map(t=>`<label class="checkbox-label"><input id="dependency-${esc(t.id)}" type="checkbox" data-dependency="${esc(t.id)}">${esc(t.id)} · ${esc(t.goal)}</label>`).join('')||'<p class="note">首个任务无需依赖。</p>'}</fieldset></details><p class="note">保存后点击“调度 Worker”开始执行。已经启动的任务保留原目标与依赖。</p><div class="buttons"><button class="primary">保存任务</button>${btn('task-new','新建任务')}</div></form></details>`}

export function readTask(form,tasks){
  const v=id=>form.querySelector('#'+id).value;
  const id=v('task-id').trim()||('task-'+(globalThis.crypto?.randomUUID?.()||Date.now().toString(36))),previous=tasks.find(t=>t.id===id);
  form.querySelector('#task-id').value=id;
  const task={...previous,id,goal:v('task-goal'),completion_criteria:form.querySelector('#task-completion')?.value||'',mode:v('task-mode'),priority:Number(v('task-priority')),success_contains:v('task-predicate'),input_attachment_ids:[...form.querySelectorAll('[data-task-input]:checked')].map(el=>el.dataset.taskInput),dependencies:[...form.querySelectorAll('[data-dependency]:checked')].map(el=>el.dataset.dependency)};
  return previous?tasks.map(t=>t.id===id?task:t):[...tasks,task];
}

export function mcpDescription(m){
  if(m.description)return m.description;
  return m.url?'为 Agent 接入远端工具能力。通过 HTTP 连接；具体用途可在工具目录查看。':'为 Agent 接入服务器上的工具能力。启动本地程序并通过 stdio 通信；具体用途可在工具目录查看。';
}
// API replaces tool arrays on reload; share one directory index across views.
const toolIndexes=new WeakMap();
export function toolIndex(tools){
  if(toolIndexes.has(tools))return toolIndexes.get(tools);
  const mcp=new Map(),external=[];
  tools.forEach((t,i)=>{
    if(t.adapter!=='mcp'&&t.adapter!=='local')return;
    const entry={t,i,search:[t.local_name,t.remote_name,t.definition.function.name,t.definition.function.description,t.server_name].join(' ').toLowerCase(),source:t.server_name||t.adapter};
    external.push(entry);
    if(t.adapter==='mcp'){if(!mcp.has(t.server_name))mcp.set(t.server_name,[]);mcp.get(t.server_name).push(entry)}
  });
  const index={mcp,external};toolIndexes.set(tools,index);return index;
}
export function toolDetail(tool,backAttrs=''){
  const f=tool.definition.function,schema=f.parameters,properties=schema?.properties||{},required=new Set(schema?.required||[]);
  const type=p=>typeof p==='boolean'?(p?'任意值':'不接受值'):Array.isArray(p.type)?p.type.join(' / '):p.type||(p.properties?'object':p.enum?'枚举':p.$ref?'引用结构':'复合结构');
  return `${backAttrs?`<div class="buttons">${btn('mcp-tools','← 返回服务工具目录',backAttrs)}</div>`:''}<p class="service-description">${esc(f.description||'此工具未提供用途说明。')}</p><dl class="service-info"><dt>工具名称</dt><dd>${esc(tool.local_name||tool.remote_name||f.name)}</dd><dt>接口 ID</dt><dd class="mono">${esc(f.name||'尚未注册')}</dd><dt>来源</dt><dd>${esc(tool.server_name||(tool.adapter==='local'?'本地命令':tool.adapter)||'未记录')}</dd>${tool.remote_name&&tool.remote_name!==f.name?`<dt>远端名称</dt><dd class="mono">${esc(tool.remote_name)}</dd>`:''}</dl><h3>输入参数</h3>${schema?.description?`<p class="note">${esc(schema.description)}</p>`:''}${Object.keys(properties).length?`<div class="table-wrap section-gap"><table class="data-table"><thead><tr><th>参数</th><th>类型</th><th>必填</th><th>说明</th></tr></thead><tbody>${Object.entries(properties).map(([name,p])=>`<tr><td class="mono">${esc(name)}</td><td class="mono">${esc(type(p))}</td><td>${required.has(name)?'必填':'可选'}</td><td>${esc(p.description||'未提供说明')}${p.enum?`<p class="note">可选值：${esc(p.enum.map(v=>JSON.stringify(v)).join(' / '))}</p>`:''}${p.default!==undefined?`<p class="note">默认值：${json(p.default)}</p>`:''}</td></tr>`).join('')}</tbody></table></div>`:`<p class="note section-gap">${schema?.type==='object'&&!schema.oneOf&&!schema.anyOf&&!schema.allOf?'未声明固定参数。':'未提供可展开的参数列表。'}完整参数约束见高级定义。</p>`}<details class="request-details"><summary>高级：完整工具定义 / JSON</summary><pre>${json(tool)}</pre></details>`;
}
export function mcpDetail(config,tools,index,statuses=[]){
  const m=config.mcp[index],count=toolIndex(tools).mcp.get(m.name)?.length||0;
  return `<div class="buttons">${btn('integration-catalog','← 返回接入目录')}${btn('mcp-toggle',m.disabled?'启用服务':'停用服务',`data-index="${index}" data-service="${esc(m.name)}"`)}</div><p class="service-description">${esc(mcpDescription(m))}</p><dl class="service-info"><dt>连接方式</dt><dd>${m.url?'Streamable HTTP':'本地 stdio'}</dd><dt>启用状态</dt><dd>${m.disabled?'已禁用 · 配置保留，目录未加载':'已启用'}</dd><dt>${m.url?'服务地址':'启动命令'}</dt><dd class="mono">${esc(m.url||[m.command,...m.args||[]].join(' '))}</dd>${m.dir?`<dt>工作目录</dt><dd class="mono">${esc(m.dir)}</dd>`:''}<dt>工具目录</dt><dd>${m.disabled?'启用服务后发现工具接口':`${count} 个工具接口`}</dd></dl><section id="mcp-connection-status" data-name="${esc(m.name)}" aria-live="polite">${mcpStatusBody(statuses.find(item=>item.name===m.name)||{enabled:!m.disabled,connection:{state:m.disabled?'disabled':'not_connected'}})}</section>${sourceLinks(m)}${integrationActions(m,'mcp',m.name)}<p class="note">发现接口不代表每个分析引擎及其本地依赖已可用，以实际调用结果为准。启停和编辑会立即重建 Agent 连接，需先暂停运行和 Worker。停用保留配置、关闭 Trace 持有的连接并移出工具目录；HTTP 远端服务不会被关闭。</p><div class="buttons">${btn('mcp-edit','编辑连接',`data-index="${index}" data-service="${esc(m.name)}"`)}${btn('mcp-tools',`查看工具（${m.disabled?'未加载':count}）`,`data-index="${index}" data-service="${esc(m.name)}" ${m.disabled?'disabled':''}`)}</div>`;
}
export function mcpTools(config,tools,index){
  const m=config.mcp[index],items=toolIndex(tools).mcp.get(m.name)||[];
  return `<div class="toolbar"><div>${btn('mcp-detail','← 服务详情',`data-index="${index}" data-service="${esc(m.name)}"`)}</div><label>搜索 ${esc(m.name)} 的工具<input id="service-tool-filter" type="search" placeholder="名称、用途或接口 ID…"></label><span id="service-tool-count" class="note">${items.length} 个接口</span></div><div class="service-tools" data-name="${esc(m.name)}">${m.disabled?'<p class="note">服务已禁用，目录未加载。</p>':items.map(({t,i,search})=>`<button class="tool-card" data-service-search="${esc(search)}" data-action="tool-detail" data-index="${i}" data-mcp-index="${index}" data-service="${esc(m.name)}" data-tool-name="${esc(t.definition.function.name)}"><h3>${esc(t.local_name||t.remote_name||t.definition.function.name)}</h3><p>${esc(t.definition.function.description)}</p><small class="mono">${esc(t.definition.function.name)}</small></button>`).join('')||'<p class="note">此服务当前未发现工具接口。</p>'}</div><p id="service-tool-empty" class="empty" hidden>没有匹配的工具接口；调整搜索条件后重试。</p>`;
}
export function mcpForm(config,tools=[],statuses=[]){const groups=toolIndex(tools).mcp;return `<section class="mcp-management"><div class="section-label">MCP 服务 <span>${(config.mcp||[]).length} 个服务</span></div><div class="mcp-services"><button class="mcp-add" type="button" data-action="mcp-new"><b>＋</b><span>添加 MCP 服务</span></button>${(config.mcp||[]).map((m,i)=>`<article class="mcp-service" data-integration-kind="mcp" data-integration-search="${esc([m.name,m.description,m.github_url,m.local_path,...(groups.get(m.name)||[]).map(entry=>entry.search)].join(' ').toLowerCase())}"><button type="button" class="skill-title" data-action="mcp-detail" data-index="${i}" data-service="${esc(m.name)}" aria-label="查看 ${esc(m.name)} 服务详情"><header><b>${esc(m.name)}</b><span class="badge">${m.url?'HTTP':'STDIO'}</span></header><p>${esc(mcpDescription(m))}</p></button><footer><label class="availability-switch"><input type="checkbox" role="switch" data-switch data-action="mcp-toggle" data-index="${i}" data-service="${esc(m.name)}" ${m.disabled?'':'checked'} aria-label="${esc(m.name)} 服务启用"><span>${m.disabled?'启用服务':'停用服务'}</span></label><span class="status ${m.disabled?'unknown':'enabled'}">${m.disabled?'已禁用':'已启用 · '+esc(mcpStateLabel(statuses.find(item=>item.name===m.name)?.connection?.state))}</span><span class="note">${m.disabled?'目录未加载':`${(groups.get(m.name)||[]).length} 个工具接口`}</span></footer></article>`).join('')}</div><p class="empty catalog-empty" data-integration-empty="mcp" ${(config.mcp||[]).length?'hidden':''}>${(config.mcp||[]).length?'没有匹配的 MCP 服务；调整搜索条件后重试。':'尚未配置 MCP 协议服务；浏览器扩展见下方。'}</p><p class="note">服务卡片开关控制启用 / 停用；检测连接不改变启用状态。变更前请暂停运行和 Worker。</p></section>`}

export function readMCP(form,config){
  const v=id=>form.querySelector('#'+id).value,lines=id=>v(id).split('\n').filter(line=>line.trim());
  const pairs=id=>Object.fromEntries(lines(id).map(line=>{const i=line.indexOf('=');if(i<1)throw new Error('环境变量和请求头请按 名称=值 填写，每行一项。');return [line.slice(0,i).trim(),line.slice(i+1)]}));
  const name=v('mcp-name').trim(),original=form.querySelector('#mcp-original')?.value||'',existing=config.mcp?.find(item=>item.name===name);
  if(existing&&!original)throw new Error('同名 MCP 服务已存在，请从服务详情进入编辑连接。');
  if(original&&JSON.stringify(existing)!==original)throw new Error('此服务配置已变化，请重新打开编辑连接后保存。');
  const m={...existing,...(v('mcp-transport')==='http'?{name,url:v('mcp-url').trim(),headers:pairs('mcp-headers')}:{name,command:v('mcp-command').trim(),args:lines('mcp-args'),dir:v('mcp-dir').trim(),env:pairs('mcp-env')})};
  for(const key of v('mcp-transport')==='http'?['command','args','env','dir']:['url','headers'])delete m[key];
  m.github_url=(v('mcp-github_url')||'').trim();m.local_path=(v('mcp-local_path')||'').trim();
  m.description=v('mcp-description').trim();m.disabled=!form.querySelector('#mcp-enabled').checked;
  const next=structuredClone(config),items=next.mcp||[];next.mcp=items.some(item=>item.name===name)?items.map(item=>item.name===name?m:item):[...items,m];return next;
}

export function recordDetail(kind,item,snap){
  let body='';
  if(kind==='asset-detail'||kind==='task-detail'){
    body=`<h3>${esc(item.value||item.goal)}</h3><p class="note">${kind==='task-detail'&&snap.projection==='planner'&&item.status==='verified'?'<span class="status unverified">已记录 · 待核验</span>':status(item.coverage||item.status)} · ${esc(label(item.kind||item.mode))}</p><p>完成条件：${esc(item.completion_criteria||'未设置')}</p><p>精确验证谓词：${esc(item.predicate||item.success_contains||'未设置')}</p><dl class="service-info"><dt>记录 ID</dt><dd class="mono">${esc(item.id)}</dd>${kind==='asset-detail'?`<dt>范围</dt><dd>${esc(label(item.scope||'included'))}</dd><dt>上级资产</dt><dd class="mono">${esc(item.parent_id||'未设置')}</dd>`:`<dt>依赖任务</dt><dd>${(item.dependencies||[]).map(esc).join(', ')||'无依赖'}</dd><dt>优先级</dt><dd>${esc(item.priority??0)}</dd>`}</dl>${item.error?`<p class="inline-error">${esc(item.error)}</p>`:''}`;
    const source=item.evidence_run_id||item.source_run_id;
    body+=`<div class="buttons section-gap">${source&&source!==snap.run.id?btn('select','进入证据来源运行 ↗',`data-id="${esc(source)}"`):evidenceLinks(item.evidence_ids)}${item.child_run_id?btn('select','进入子运行 ↗',`data-id="${esc(item.child_run_id)}"`):''}${kind==='asset-detail'?btn('asset-edit','编辑资产',`data-id="${esc(item.id)}"`):''}</div>`;
  }
  if(kind==='action-detail')body=`<h3>${esc(item.local_name||item.remote_name||item.name)}</h3><p>${status(callStatus(item))}</p><p class="note">调用 ${esc(item.call_id)} · 请求 ${esc(item.request_id||'未记录')} · ${duration(item.started_at,item.finished_at)}</p><h3>参数</h3><pre>${esc(item.arguments||'{}')}</pre><h3 class="section-gap">实际输出</h3>${toolOutput(item.result)}<div class="buttons section-gap">${evidenceLinks((snap.evidence||[]).filter(e=>e.call_id===item.call_id&&(e.request_id?e.request_id===item.request_id:(snap.actions||[]).filter(a=>a.call_id===item.call_id).length===1)).map(e=>e.id))}${(item.result?.artifacts||[]).map(a=>btn('artifact','原始产物 · '+esc(a.kind),`data-hash="${esc(a.hash)}"`)).join('')}</div>`;
  if(kind==='action-detail'&&item.status==='uncertain')body+=`<p class="note">${snap.run.execution_active===false?'当前未在工作台运行；点击“恢复运行”后继续核对未知结果。':'未知结果由 Agent 自动核对和重规划；'}原始错误保留，原调用不会重放。</p>`;
  if(kind==='action-detail'&&item.status==='uncertain'&&['paused','waiting_tool'].includes(snap.run.status)&&messageCallRecords(snap).some(record=>!record.reply&&callKey(record.action)===callKey(item)))body+=`<form id="tool-observation-form" class="field-stack section-gap" data-run="${esc(snap.run.id)}" data-request="${esc(item.request_id||'')}" data-call="${esc(item.call_id)}"><label>补充核对结果（可选）<textarea name="content" required placeholder="记录已核实的输出、错误或外部状态；尚未确认的部分请明确说明。"></textarea></label><p class="note">${snap.run.execution_active===false?'请点击“恢复运行”继续处理原始输出。':'Agent 会自动读取原始输出并继续核对和重规划。'}此处仅用于补充外部观测，不会重放原调用，也不是继续运行的前置条件。</p><button type="submit">保存补充观测</button></form>`;
  if(kind==='action-detail'&&(snap.requests||[]).some(r=>r.id===item.request_id))body+=btn('request-detail','模型请求 / 工具 schema',`data-request="${esc(item.request_id)}"`);
  return body+`<details class="request-details"><summary>完整账本字段</summary><pre>${json(item)}</pre></details>`;
}

export function resourceLimitReason(value){
 if(value.includes('pids.events'))return '进程 / 线程数量达到单条命令上限。Python 线程池也占用该额度；缩小并发后，核对本次已执行的操作再继续。';
 if(value.includes('memory.events'))return '单条命令达到内存上限，进程可能已被终止；需核对已执行的操作。';
 if(value.includes('output_bytes'))return '命令输出达到归档上限；请缩小输出范围并核对本次操作。';
 return '命令触发执行资源上限：'+value;
}
export function pendingTools(snap,tools=[]){
  const registered=new Set(tools.map(t=>t.definition.function.name)),calls=[];
  for(const record of messageCallRecords(snap)){
    const call=record.action;
    if(call.status==='uncertain'||!record.reply&&!call.result&&!registered.has(call.name)&&snap.run.status==='waiting_tool'&&!['started','running'].includes(call.status))calls.push(call);
  }
  for(const action of snap.actions||[])if(action.status==='uncertain'&&!calls.some(call=>callKey(call)===callKey(action)))calls.push(action);
  if(!calls.length)return '';
  return `<details class="pending-tools" open><summary>需要核对的调用 · ${calls.length}</summary>${snap.run.execution_active===false?'<p class="note">当前未在工作台运行；点击“恢复运行”继续处理待核对结果。</p>':''}${calls.map(call=>`<p class="note">${esc(call.name)} · ${call.status==='uncertain'?`执行状态或部分操作结果尚待核对；不会自动重放，${snap.run.execution_active===false?'恢复运行后继续处理。':'Agent 会继续处理。'}`:'工具未接入当前目录，等待实际结果。'}</p>${callDiagnostics(call)}${!call.result?.diagnostic&&call.result?.worker_resource_limit?`<p class="inline-error">${esc(resourceLimitReason(call.result.worker_resource_limit))}</p>`:''}${call.result?.error?`<p class="note">${esc(call.result.error)}</p>`:''}${btn('action-detail','调用详情',`data-id="${esc(call.call_id)}" data-request="${esc(call.request_id||'')}"`)}${call.status==='uncertain'?'<p class="note">打开调用详情查看原始输出；补充观测是可选项，原始错误保留且不会重放原调用。</p>':`<pre>${esc(`trace tool ${snap.run.id} ${call.call_id} "实际结果文本"`)}</pre>`}`).join('')}</details>`;
}
export function mcpEditor(){return `<div id="mcp-editor"><label class="section-gap">接入来源<select id="mcp-source"><option value="connect">连接已有服务</option><option value="github">从 GitHub 下载源码并接入</option></select></label><div data-mcp-source="connect"><p class="note">连接服务端已可运行的程序或 HTTP 服务，不下载源码。</p><form id="mcp-form" class="field-stack"><input id="mcp-original" type="hidden" value=""><div class="form-row">${field('mcp-name','连接名称','',true)}<label>连接方式<select id="mcp-transport"><option value="stdio">本地程序（stdio）</option><option value="http">HTTP 服务</option></select></label></div><label>服务简介<textarea id="mcp-description" placeholder="简述此服务提供的能力（可选）"></textarea></label><label class="checkbox-label"><input id="mcp-enabled" type="checkbox" checked>启用服务（取消勾选会保留连接配置）</label><div data-mcp-stdio class="field-stack">${field('mcp-command','程序路径或命令（例如 node / uvx）')}<label>启动参数，每行一个<textarea id="mcp-args" placeholder="C:\\tools\\server.js"></textarea></label>${field('mcp-dir','工作目录（可选）')}<label>环境变量，每行 NAME=value<textarea id="mcp-env" spellcheck="false"></textarea></label></div><div data-mcp-http class="field-stack" hidden>${field('mcp-url','服务 URL')}<label>HTTP 请求头，每行 Name=value<textarea id="mcp-headers" spellcheck="false" placeholder="Authorization=Bearer …"></textarea></label></div><div class="form-row">${field('mcp-github_url','GitHub 来源（可选）')}${field('mcp-local_path','本地项目路径（可选）')}</div><p class="note">保存启用服务时会实际连接并加载工具。GitHub 来源仅作记录，填写链接不会下载项目。保留 [stored secret] 表示沿用已保存密钥。</p><div class="buttons"><button class="primary">保存服务</button>${btn('dialog-cancel','取消')}</div></form></div><div data-mcp-source="github" hidden>${integrationImport('mcp',true)}</div></div>`}

