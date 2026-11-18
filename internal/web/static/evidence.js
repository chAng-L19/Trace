import {esc,label,short,btn,empty} from './ui.js';
import {evidencePresentation} from './evidence-report.js';

// Interaction reference: ARTEX traffic-evidence-viewer.tsx (e6ec569, AGPL-3.0).
// Independently implemented for Trace's ToolResult; no HTTP exchanges are inferred.
const sourceLabels={verified:'来源完整',unverified:'来源未验证',invalid:'来源失效'};
const text=value=>typeof value==='string'?value:JSON.stringify(value,null,2);
function object(value){try{const parsed=JSON.parse(value);return parsed&&typeof parsed==='object'?parsed:null}catch{return null}}
const fieldLabels={content:'正文',text:'文本',message:'消息',summary:'摘要',description:'说明',title:'标题',name:'名称',status:'状态',status_code:'响应状态码',code:'返回码',reason:'原因',error:'错误',ok:'是否成功',success:'是否成功',path:'文件路径',bytes:'文件大小（字节）',size:'大小',mime_type:'内容类型',eof:'已到文件末尾',offset:'起始字节',next_offset:'后续起点',truncated:'是否截断',capacity:'并发容量',tasks:'探索方向',frontier:'可开始方向',plan:'任务计划',workers:'协作执行',messages:'过程消息',goal:'目标',id:'记录 ID',task_id:'方向 ID',worker_id:'Worker ID',run_id:'运行 ID',child_run_id:'子运行 ID',parent_run_id:'父运行 ID',requirement_id:'验收条件 ID',evidence_ids:'引用证据',dependencies:'依赖方向',priority:'优先级',mode:'执行方式',success_contains:'完成判据',cleanup_status:'清理状态',result:'返回结果',data:'数据',items:'条目',count:'数量',total:'总数',url:'网址',method:'请求方法',headers:'请求头',request:'请求',response:'响应',created_at:'创建时间',updated_at:'更新时间',started_at:'开始时间',finished_at:'结束时间',lease_until:'租约截止',reasoned_through:'已处理位置',impact:'影响',root_cause:'根因',remediation:'修复建议',reproduction_steps:'复现步骤',severity:'严重程度',triage:'处置',review:'独立审阅',retests:'复测记录',predicate:'判据',references:'来源引用',artifacts:'原始产物'};
const fieldName=key=>fieldLabels[key]||label(key);
const technical=key=>key==='data_base64'||key==='hash'||key==='sha256'||key.endsWith('_hash');
const compact=value=>String(value??'').replace(/\s+/g,' ').trim();
const excerpt=(value,max=200)=>{const s=compact(value);return s.length>max?s.slice(0,max)+'…':s};
const stateName=value=>({pass:'判据通过',fail:'判据未通过',unknown:'待核对',submitted:'待审阅',reviewed:'审阅通过',reproduced:'复测通过',needs_evidence:'待补证据',rejected:'审阅未通过'}[value]||label(value));
function summaryOf(value,depth=0){
  if(value===null)return '空值';
  if(typeof value==='string'){const nested=depth<3&&object(value);return nested?summaryOf(nested,depth+1):excerpt(value)}
  if(typeof value!=='object')return String(value);
  if(Array.isArray(value))return value.length?`${value.length} 项记录：${value.slice(0,3).map(v=>summaryOf(v,depth+1)).join('；')}${value.length>3?'；其余见详情':''}`:'没有返回记录';
  if(depth>3)return `对象 · ${Object.keys(value).length} 个字段`;
  for(const key of ['content','text','message','summary'])if(typeof value[key]==='string'&&value[key].trim())return (value.path?value.path+' · ':'')+summaryOf(value[key],depth+1);
  const plan=Array.isArray(value.tasks)?value:value.plan&&Array.isArray(value.plan.tasks)?value.plan:null;
  if(plan){const counts=new Map();for(const task of plan.tasks){const status=stateName(task.status);counts.set(status,(counts.get(status)||0)+1)}return `${plan.tasks.length} 个探索方向${counts.size?' · '+[...counts].map(([s,n])=>s+' '+n).join('、'):''} · 可开始 ${(plan.frontier||[]).length} 项${plan.frontier?.length?'（'+plan.frontier.slice(0,3).join('、')+'）':''}${Array.isArray(value.workers)?' · '+value.workers.length+' 个 Worker':''}`}
  if(value.requirement_id)return `${stateName(value.status)} · 条件 ${value.requirement_id}${value.reason?' · '+excerpt(value.reason,100):''}`;
  if(value.path)return `${value.path}${value.bytes!==undefined?' · '+value.bytes+' 字节':''}${value.eof===false?' · 当前为部分内容':''}`;
  for(const key of ['description','title','goal','error','reason'])if(typeof value[key]==='string'&&value[key].trim())return `${value.status?stateName(value.status)+' · ':''}${excerpt(value[key])}`;
  const entries=Object.entries(value).filter(([k])=>!technical(k));
  if(!entries.length)return '仅含编码或校验字段 · 展开原文查看';
  return '字段：'+entries.slice(0,5).map(([k,v])=>`${fieldName(k)}${Array.isArray(v)?'（'+v.length+' 项）':v&&typeof v==='object'?'（对象）':k==='status'?'：'+stateName(v):'：'+excerpt(v,50)}`).join('；')+(entries.length>5?'；更多见详情':'');
}
export function evidenceReading(e){
  const raw=String(e.content??''),parsed=object(raw);
  const result=e.type==='tool_execution'&&parsed&&!Array.isArray(parsed)&&('stdout' in parsed||'stderr' in parsed||'exit_code' in parsed||'mcp_result' in parsed)?parsed:null;
  const streams=[];
  if(result){
    if(result.stdout)streams.push({title:'标准输出',value:String(result.stdout)});
    if(result.stderr)streams.push({title:'标准错误',value:String(result.stderr)});
    if(result.error)streams.push({title:'执行错误',value:String(result.error)});
    if(!streams.length&&result.mcp_result)streams.push({title:'MCP 返回',value:text(result.mcp_result)});
  }else if(raw)streams.push({title:'记录内容',value:raw});
  const first=streams[0]?.value||'',body=object(first);
  return {result,streams,summary:excerpt(body?summaryOf(body):first)||(result?.artifacts?.length?'输出已归档：'+result.artifacts.map(a=>label(a.kind)+' · '+Number(a.bytes).toLocaleString()+' 字节').join('；'):'没有文本输出'),source:sourceLabels[e.verification]||label(e.verification)};
}
function readable(value,depth=0){
  if(value===null)return '<span class="note">空值</span>';
  if(typeof value!=='object')return `<span class="evidence-value">${esc(typeof value==='boolean'?(value?'是':'否'):value)}</span>`;
  if(depth>=3)return `<span class="evidence-value">${esc(excerpt(summaryOf(value)))}</span><span class="note"> · 嵌套内容见完整原文</span>`;
  if(Array.isArray(value))return value.length?`<ol class="evidence-values">${value.slice(0,30).map(v=>`<li>${readable(v,depth+1)}</li>`).join('')}</ol>${value.length>30?'<p class="note">以上为前 30 项；完整列表保留在原文。</p>':''}`:'<span class="note">空列表</span>';
  const entries=Object.entries(value).filter(([k])=>!technical(k)),hidden=Object.keys(value).length-entries.length;
  return (entries.length?`<dl class="evidence-fields">${entries.map(([k,v])=>`<dt>${esc(fieldName(k))}</dt><dd>${readable(k==='status'?stateName(v):v,depth+1)}</dd>`).join('')}</dl>`:'<span class="note">没有可读正文字段</span>')+(hidden?`<p class="note">${hidden} 项编码或校验字段保留在完整原文中。</p>`:'');
}
function streamView(stream,preview){
  const parsed=object(stream.value),clipped=!parsed&&preview&&stream.value.length>1000,value=clipped?stream.value.slice(0,1000):stream.value;
  const structured=parsed?`<p class="evidence-summary">${esc(excerpt(summaryOf(parsed),300))}</p>${preview?`<details><summary>展开结构化内容</summary>${readable(parsed)}</details>`:readable(parsed)}`:'';
  return `<section class="evidence-stream"><h4>${esc(stream.title)}</h4>${parsed?structured:`<pre>${esc(value)}</pre>`}${clipped?'<p class="note">以上为前 1,000 字符；打开证据详情读取完整记录。</p>':''}</section>`;
}
function executionInfo(reading){
  const r=reading.result;if(!r)return '';
  return `<div class="evidence-outcome">${r.exit_code!==undefined?`<span>退出码 <b>${esc(r.exit_code)}</b></span>`:''}${r.interrupted?'<span>执行被中断</span>':''}${r.outcome_uncertain||r.worker_outcome_uncertain?'<span>外部结果待核对</span>':''}${r.mcp_is_error?'<span>MCP 返回错误</span>':''}</div>`;
}
function sourceStatus(e,reading){return `<span class="status ${esc(e.verification)}" title="来源校验与业务结论分别记录">${esc(reading.source)}</span>`}
function related(p){
  if(!p.links.length)return '<p class="note">此执行记录尚未被报告引用，保留在原始执行账本中。</p>';
  return '<section class="section-gap"><h4>报告中的引用用途</h4>'+p.links.map(link=>'<article class="evidence-citation"><h4>'+esc(link.title)+'</h4><p>'+esc(link.summary)+'</p><p class="note">'+esc(label(link.status))+'</p>'+((link.original_title||link.original_summary)?'<details><summary>查看引用原文</summary><pre>'+esc([link.original_title,link.original_summary].filter(Boolean).join('\n'))+'</pre></details>':'')+'</article>').join('')+'</section>';
}
export function evidenceCard(e,snap,p=evidencePresentation(e,snap)){
  const reading=evidenceReading(e),action=p.action;
  const name=action?.local_name||action?.remote_name||action?.name||label(e.type);
  return '<article class="evidence-card evidence-readable"><header><div><h3>'+esc(p.title)+'</h3><p class="note">'+esc(name)+' · '+esc(short(e.id))+'</p></div>'+sourceStatus(e,reading)+'</header><p class="evidence-summary">'+esc(p.summary)+'</p><p class="note">'+(p.reportRelated?'报告明确引用 · '+p.links.length+' 处':'未关联报告的执行记录')+'</p><details class="request-details"><summary>展开执行输出</summary>'+executionInfo(reading)+(reading.streams.length?streamView(reading.streams[0],true):'<p class="note">没有文本输出。查看退出码与原始产物。</p>')+(reading.streams.length>1?'<p class="note">另有 '+(reading.streams.length-1)+' 项输出，详情中分别展示。</p>':'')+'</details><div class="buttons section-gap">'+btn('evidence-detail','阅读完整证据','data-id="'+esc(e.id)+'"')+(action?btn('action-detail','来源调用','data-id="'+esc(e.call_id)+'" data-request="'+esc(action.request_id||'')+'"'):'')+'</div></article>';
}
export function evidenceDetail(e,snap){
  if(!e)return empty('证据记录已更新','关闭详情并重新选择证据。');
  const p=evidencePresentation(e,snap),reading=evidenceReading(e),action=p.action,artifacts=reading.result?.artifacts||action?.result?.artifacts||[];
  const sourceLink=action?btn(snap.catalogRun?'catalog-action':'action-detail','查看工具输入和执行记录',snap.catalogRun?`data-run="${esc(snap.catalogRun)}" data-id="${esc(e.id)}"`:`data-id="${esc(e.call_id)}" data-request="${esc(action.request_id||'')}"`):'<p class="note">当前快照没有对应调用记录。</p>';
  return `<div class="evidence-readable"><h3>${esc(p.title)}</h3><p class="evidence-summary">${esc(p.summary)}</p>${related(p)}<div class="toolbar">${sourceStatus(e,reading)}<span class="note">来源校验不等于漏洞成立或业务验收通过。</span></div><details class="request-details section-gap"${snap.expandOutput?' open':''}><summary>展开执行输出</summary>${executionInfo(reading)}${reading.streams.map(v=>streamView(v,false)).join('')||empty('没有文本输出','检查执行状态与原始产物。')}</details>${artifacts.length?`<section class="section-gap"><h4>完整原始产物</h4><p class="note">当前文本可能为归档后的预览，下载保留完整字节。</p>${artifacts.map(a=>`<div class="buttons">${btn('artifact',esc(label(a.kind))+' · '+Number(a.bytes).toLocaleString()+' 字节',`data-hash="${esc(a.hash)}"`)}<a href="/api/artifacts/${esc(a.hash)}/download" download>下载 ↓</a></div>`).join('')}</section>`:''}<details class="request-details section-gap"><summary>来源与完整原文</summary><dl class="key-values"><dt>证据 ID</dt><dd>${esc(e.id)}</dd><dt>来源调用</dt><dd>${esc(e.call_id||'未记录')}</dd><dt>来源请求</dt><dd>${esc(e.request_id||'未记录')}</dd><dt>来源事件</dt><dd>#${esc(e.source_event_sequence)}</dd><dt>SHA256</dt><dd class="hash">${esc(e.content_hash)}</dd></dl>${sourceLink}<pre>${esc(e.content)}</pre></details></div>`;
}
