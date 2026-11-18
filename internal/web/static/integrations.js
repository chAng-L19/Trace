import {esc,field,btn} from './ui.js';

// Keep source links beside their local checkout; never manufacture a project path for HTTP services.
export function sourceLinks(item){
  const path=item.path||item.local_path||'',github=item.github_url||'',project=item.path&&item.local_path&&item.path!==item.local_path?item.local_path:'';
  const githubLink=/^https:\/\/github\.com\//i.test(github)?`<a href="${esc(github)}" target="_blank" rel="noopener noreferrer">${esc(github)} ↗</a>`:esc(github);
  const localRow=(label,value)=>`<div><span>${label}</span><code title="Trace 服务所在机器的路径">${esc(value)}</code>${btn('copy-path','复制路径',`data-path="${esc(value)}" aria-label="复制${esc(label)}：${esc(value)}"`)}</div>`;
  return `<div class="integration-sources">${github?`<div><span>GitHub</span>${githubLink}</div>`:''}${path?localRow(item.path?'Skill 文件':'项目目录',path):''}${project?localRow('项目目录',project):''}${item.dir?localRow('执行工作目录',item.dir):''}${!path&&!github?'<p class="note">项目来源未记录；启动程序路径不代表项目来源。</p>':''}</div>`;
}
export function integrationActions(item,kind,name){
  if(!item.github_url)return '';
  const attrs=`data-id="${esc(item.integration_id||item.id||'')}" data-kind="${kind}" data-name="${esc(name||item.name||'')}"`;
  return `<div class="buttons integration-actions">${btn('integration-check','检查更新',attrs)}${btn('integration-update','立即更新',attrs+' title="更新此 GitHub 项目及同源接入项，保留本地修改；需先暂停运行和 Worker"')}</div>`;
}
export function integrationFeedback(s){
  const item=s.integrationFeedback;if(!item)return '';
  return `<p class="${item.error?'inline-error':'integration-feedback note'}" role="${item.error?'alert':'status'}" aria-live="${item.error?'assertive':'polite'}" ${item.busy?'aria-busy="true"':''}>${esc(item.message)}</p>`;
}
export function integrationImport(kind,embedded=false){
  return `${embedded?'':`<details id="github-import" class="integration-import section-gap"><summary>从 GitHub 导入 ${kind==='mcp'?'MCP 服务':'Skill'}</summary>`}
    <form id="integration-form" data-kind="${kind}" class="field-stack">
    <label>GitHub 仓库或子目录链接<input id="integration-url" type="url" required spellcheck="false" autocapitalize="none" placeholder="https://github.com/owner/repo/tree/main/${kind==='mcp'?'mcp':'skills/example'}"></label>
    <p class="note">项目下载到 Trace 服务所在机器；导入和更新需先暂停正在执行的主任务与 Worker。</p>
    ${kind==='mcp'?field('integration-name','服务名称（有仓库配置时可留空）'):''}
    ${kind==='mcp'?`<label class="checkbox-label"><input id="integration-enabled" type="checkbox" checked>导入后启用服务（依赖尚未安装时取消，先下载项目）</label>
    <details id="integration-mcp-options"><summary>指定 MCP 启动配置（仓库没有 MCP 配置时填写）</summary>
    <p class="note">填写仓库说明中的真实启动命令。工作目录默认为导入的项目目录；依赖需按项目说明在服务端安装。此处填写后替代仓库启动配置，不与其合并。</p>
    ${field('integration-command','程序路径或命令，例如 node / uvx')}
    <label>启动参数，每行一个<textarea id="integration-args" spellcheck="false" placeholder="server.js\n--stdio"></textarea></label>
    <label>环境变量，每行 NAME=value<textarea id="integration-env" spellcheck="false"></textarea></label></details>
    <p class="note">未指定命令时读取仓库中的 .mcp.json、mcp.json 或 trace-mcp.json；普通 GitHub 项目不一定提供 MCP 服务。</p>`
    :'<p class="note">导入目录需包含 SKILL.md；按其中声明的名称发现技能，并保留 GitHub 来源和服务端项目路径。</p>'}
    <div class="buttons"><button class="primary">导入并接入</button></div></form>${embedded?'':'</details>'}`;
}
export function readIntegration(form){
  const v=id=>form.querySelector('#'+id)?.value||'',body={kind:form.dataset.kind,url:v('integration-url').trim(),name:v('integration-name').trim()};
  if(body.kind==='mcp'){
    body.disabled=form.querySelector('#integration-enabled')?.checked===false;
    if(!v('integration-command').trim()&&(v('integration-args').trim()||v('integration-env').trim()))throw new Error('已填写启动参数或环境变量，请同时填写启动命令；读取仓库默认配置时请清空这些字段。');
  }
  if(body.kind==='mcp'&&v('integration-command').trim()){
    const env=Object.fromEntries(v('integration-env').split('\n').filter(line=>line.trim()).map(line=>{const i=line.indexOf('=');if(i<1)throw new Error('环境变量请按 名称=值 填写，每行一项。');return [line.slice(0,i).trim(),line.slice(i+1)]}));
    if(!body.name)throw new Error('指定 MCP 启动命令时，请填写服务名称。');
    body.mcp={name:body.name,disabled:body.disabled,command:v('integration-command').trim(),args:v('integration-args').split('\n').filter(line=>line.trim()),env};
  }
  return body;
}
export function integrationResult(result){
  const text={imported:'项目已导入，配置与目录已重新加载',updated:'项目已更新，配置与目录已重新加载',up_to_date:'当前项目已是最新版本',update_available:'发现上游更新，可点击“立即更新”',pinned:'已固定版本，不跟踪分支更新',local_changes:'本地存在未提交更改，已保留当前版本；处理更改后再更新',diverged:'本地与上游历史已分叉，已保留当前版本'}[result.status]||'操作已返回，请查看项目状态';
  const names=result.integration?.mcp_names||[],pending=result.status==='imported'&&names.length&&names.every(name=>result.config?.mcp?.find(m=>m.name===name)?.disabled);
  const revision=result.latest_revision||result.integration?.revision;
  return (pending?'项目已导入，服务尚未启动；安装依赖后在服务详情中启用':text)+(revision?` · ${revision.slice(0,12)}`:'')+(result.cleanup_error?` · 旧连接清理异常：${result.cleanup_error}`:'');
}
