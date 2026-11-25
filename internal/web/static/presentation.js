import {esc,json,label,evidenceLinks} from './ui.js';

// Markdown is rendered as escaped text; raw HTML and executable URL schemes stay text.
const SAFE_HREF=/^(https?:\/\/|mailto:|\/[^/\\])/;
function inline(text){
  return String(text).split(/(`[^`]+`|!?\[[^\]]*\]\([^\s)]+\))/g).map(part=>{
    if(part.startsWith('`')&&part.endsWith('`'))return `<code>${esc(part.slice(1,-1))}</code>`;
    const image=part.match(/^!\[([^\]]*)\]\(([^\s)]+)\)$/);
    if(image){
      if(/^\/api\/artifacts\/[a-fA-F0-9]{64}\/download\?preview=1$/.test(image[2]))return `<img class="artifact-media" src="${esc(image[2])}" alt="${esc(image[1])}">`;
      return SAFE_HREF.test(image[2])?`<a href="${esc(image[2])}" target="_blank" rel="noopener noreferrer">${esc(image[1]||image[2])}</a>`:esc(part);
    }
    const link=part.match(/^\[([^\]]+)\]\(([^\s)]+)\)$/);
    if(link&&SAFE_HREF.test(link[2]))return `<a href="${esc(link[2])}" target="_blank" rel="noopener noreferrer">${esc(link[1])}</a>`;
    return esc(part).replace(/\*\*([^*]+)\*\*/g,'<strong>$1</strong>').replace(/\*([^*\n]+)\*/g,'<em>$1</em>').replace(/~~([^~]+)~~/g,'<del>$1</del>');
  }).join('');
}
export function markdown(text){
  const lines=String(text??'').replace(/\r\n/g,'\n').split('\n'),out=[];
  for(let i=0;i<lines.length;){
    const line=lines[i];
    if(!line.trim()){i++;continue}
    const fence=line.match(/^\s*(```+|~~~+)(.*)$/);
    if(fence){const code=[];i++;while(i<lines.length&&!lines[i].trimStart().startsWith(fence[1]))code.push(lines[i++]);if(i<lines.length)i++;out.push(`<div class="code-block"><div>${esc(fence[2].trim()||'代码')}<button type="button" class="small" data-action="copy-code">复制代码</button></div><pre><code>${esc(code.join('\n'))}</code></pre></div>`);continue}
    const heading=line.match(/^(#{1,6})\s+(.+)$/);
    if(heading){const n=Math.min(heading[1].length+1,6);out.push(`<h${n}>${inline(heading[2])}</h${n}>`);i++;continue}
    if(/^\s*([-*_])\1\1+\s*$/.test(line)){out.push('<hr>');i++;continue}
    if(line.includes('|')&&/^\s*\|?\s*:?-{3,}/.test(lines[i+1]||'')){
      const cells=value=>value.trim().replace(/^\||\|$/g,'').split('|').map(v=>v.trim());
      const head=cells(line);i+=2;const rows=[];while(i<lines.length&&lines[i].includes('|'))rows.push(cells(lines[i++]));
      out.push(`<div class="markdown-table"><table><thead><tr>${head.map(c=>`<th>${inline(c)}</th>`).join('')}</tr></thead><tbody>${rows.map(row=>`<tr>${row.map(c=>`<td>${inline(c)}</td>`).join('')}</tr>`).join('')}</tbody></table></div>`);continue;
    }
    if(/^\s*>/.test(line)){const quoted=[];while(i<lines.length&&/^\s*>/.test(lines[i]))quoted.push(lines[i++].replace(/^\s*>\s?/,''));out.push(`<blockquote>${markdown(quoted.join('\n'))}</blockquote>`);continue}
    const list=line.match(/^\s*(?:([-+*])|(\d+)[.)])\s+(.+)$/);
    if(list){const tag=list[2]?'ol':'ul',items=[];while(i<lines.length){const item=lines[i].match(/^\s*(?:([-+*])|(\d+)[.)])\s+(.+)$/);if(!item||Boolean(item[2])!==Boolean(list[2]))break;items.push(`<li>${inline(item[3])}</li>`);i++}out.push(`<${tag}>${items.join('')}</${tag}>`);continue}
    const paragraph=[line];i++;
    while(i<lines.length&&lines[i].trim()&&!/^\s*(?:#{1,6}\s|```|~~~|>|[-+*]\s|\d+[.)]\s)/.test(lines[i])){if(lines[i].includes('|')&&/^\s*\|?\s*:?-{3,}/.test(lines[i+1]||''))break;paragraph.push(lines[i++])}
    out.push(`<p>${paragraph.map(inline).join('<br>')}</p>`);
  }
  return `<div class="markdown">${out.join('')}</div>`;
}

export function parseContent(text){try{return JSON.parse(String(text).trim().replace(/^```(?:json)?\s*\n/,'').replace(/\n```\s*$/,''))}catch{return undefined}}
const names={message:'消息',ok:'成功',count:'数量',total:'总计',text:'正文',links:'链接',forms:'表单',tools:'工具',selected:'已选择',query:'查询',matches:'匹配项',items:'条目',method:'请求方法',headers:'响应头',trace_result:'运行结果',final_result:'最终结果',summary:'摘要',status:'状态',answers:'答案',value:'内容',kind:'类型',verified:'验证声明',evidence:'证据',evidence_ids:'引用证据',content:'内容',reproduction_steps:'复现步骤',cleanup_status:'清理状态',terminal_reason:'结束原因',findings:'发现',result:'结果',results:'结果',title:'标题',description:'说明',error:'错误',stdout:'标准输出',stderr:'标准错误',exit_code:'退出码',command:'命令',path:'路径',file_path:'文件路径',url:'地址',goal:'目标',reason:'原因',next_action:'后续操作',tool_calls:'工具调用',arguments:'参数',name:'名称',id:'ID'};
export function structured(value){
  if(value===null||value===undefined)return '<span class="muted">—</span>';
  if(typeof value==='string')return markdown(value);
  if(typeof value!=='object')return esc(typeof value==='boolean'?(value?'是':'否'):value);
  if(Array.isArray(value))return value.length?`<ul class="structured-list">${value.map(v=>`<li>${structured(v)}</li>`).join('')}</ul>`:'<span class="muted">暂无条目</span>';
  return `<dl class="structured-fields">${Object.entries(value).map(([key,v])=>`<dt>${esc(names[key]||key)}</dt><dd>${typeof v==='string'&&key.endsWith('status')?esc(label(v)):structured(v)}</dd>`).join('')}</dl>`;
}
export function readable(text){const value=parseContent(text);return value!==undefined?structured(value):markdown(text)}
export function streamingBody(text,id){const mixed=resultFences(text,id,true);if(mixed!==undefined)return mixed;return parseContent(text)===undefined&&/^\s*[{[]/.test(text)?'<p class="note">正在整理结构化结果…</p>'+rawDetails('stream-raw-'+id,text,'查看流式原文'):messageBody({id,content:text})}
export function contentSummary(text){
  const parsed=parseContent(text);
  if(parsed===undefined&&/^\s*(?:```(?:json)?\s*)?[{[]/.test(text))return '正在整理结构化记录…';
  const value=parsed?.trace_result||parsed?.final_result||parsed;
  if(value&&typeof value==='object'){const summary=value.summary||value.description||value.title||value.message||value.text;return typeof summary==='string'?summary:(Array.isArray(value)?`${value.length} 项记录`:`结构化记录 · ${Object.keys(value).map(k=>names[k]||k).join('、')}`)}
  return String(text||'').replace(/[#*`]/g,'').replace(/\s+/g,' ').trim();
}
export const duration=(start,end)=>{const ms=Date.parse(end)-Date.parse(start);return Number.isFinite(ms)&&ms>=0?`${(ms/1000).toFixed(2)} 秒`:'耗时未记录'};
export function rawDetails(id,value,title='原始记录'){return `<details id="${esc(id)}" class="request-details raw-record"><summary>${title}</summary><pre>${typeof value==='string'?esc(value):json(value)}</pre></details>`}
function answerBody(answer){
  if(!answer||typeof answer!=='object'||Array.isArray(answer)||!Array.isArray(answer.evidence_ids)||!answer.evidence_ids.every(id=>typeof id==='string'))return structured(answer);
  return structured(answer.value??'')+evidenceLinks(answer.evidence_ids);
}
export const reportText=report=>typeof report?.zh_cn==='string'&&report.zh_cn.trim()?report.zh_cn:typeof report?.en==='string'?report.en:'';
const hasReport=value=>value!==null&&typeof value==='object'&&!Array.isArray(value)&&(Object.hasOwn(value,'trace_report')||Boolean(reportText((value.trace_result||value.final_result)?.report).trim()));
function reportPrefix(text){
  // Recognize a top-level key in incomplete JSON without matching nested examples
  // or report-like text inside a string. Field order need not put the report first.
  let depth=0;
  for(const token of text.matchAll(/"(?:\\.|[^"\\])*(?:"|$)|[{}\[\]]/g)){
    if(token[0].startsWith('"')){if(depth===1&&token[0]==='"trace_report"'&&/^\s*:/.test(text.slice(token.index+token[0].length)))return true}
    else depth+=token[0]==='{'||token[0]==='['?1:-1;
  }
  return false;
}
function reportBody(value,id){
  // ARTEX report-tab / markdown.tsx (e6ec569): present the report as Markdown.
  // Keep Trace's escaped renderer and expose the original envelope only on demand.
  const result=value.trace_result||value.final_result;
  const body=reportText(value.trace_report).trim()?reportText(value.trace_report):reportText(result?.report);
  const documents=value.trace_report?.documents;
  const bodies=[body].concat(Array.isArray(documents)?documents.map(reportText):[]);
  const rendered=bodies.filter(text=>text.trim()).map(markdown).join('<hr>');
  return `<div class="message-summary">${rendered||'<p class="note">报告正文为空；可查看原始消息。</p>'}</div>${result&&typeof result==='object'?`<details id="result-fields-${esc(id)}" class="request-details"><summary>结果详情</summary>${structured(Object.fromEntries(Object.entries(result).filter(([key])=>key!=='report')))}</details>`:''}`;
}
// Only top-level report/result envelopes are interpreted; other code stays Markdown.
function resultFences(content,id,streaming=false,allowReport=true){
  const lines=String(content).replace(/\r\n/g,'\n').split('\n'),parts=[];let plain=[],found=false,reportFound=false,index=0;
  for(let i=0;i<lines.length;){
    const opening=lines[i].match(/^\s*(`{3,}|~{3,})([^`]*)$/);
    if(!opening){plain.push(lines[i++]);continue}
    const start=i++,body=[];
    while(i<lines.length&&!(lines[i].trim().startsWith(opening[1])&&/^(`+|~+)\s*$/.test(lines[i].trim())))body.push(lines[i++]);
    const closed=i<lines.length;if(closed)i++;
    const payload=body.join('\n'),parsed=parseContent(payload),envelope=parsed?.trace_result||parsed?.final_result;
    const report=allowReport&&(hasReport(parsed)||(parsed===undefined&&reportPrefix(payload)));
    const named=/^\s*\{\s*"(?:trace_result|final_result)"\s*:/.test(payload)||report;
    const keys=allowReport?['trace_result','final_result','trace_report']:['trace_result','final_result'];
    const prefix=payload.trim().match(/^\{\s*(?:"([a-z_]*)?)?$/),possible=prefix&&(!prefix[1]||keys.some(key=>key.startsWith(prefix[1])));
    if(opening[2].trim().toLowerCase()!=='json'||(!envelope&&!named&&!(streaming&&!closed&&possible))){plain.push(...lines.slice(start,i));continue}
    found=true;const lead=plain.join('\n');if(lead.trim())parts.push(markdown(lead));plain=[];const key=id+'-fence-'+index++;
    if(report){
      reportFound=true;
      parts.push(closed&&hasReport(parsed)?reportBody(parsed,key):'<p class="note">'+(streaming?'正在整理结构化报告…':'结构化报告未完整解析；可查看原文。')+'</p>');
    }else if(envelope&&typeof envelope==='object'){
      const rendered=messageBody({id:key,content:payload});
      parts.push(parts.length?`<details id="result-envelope-${esc(key)}" class="request-details"><summary>结构化结果详情</summary>${rendered}</details>`:rendered);
    }else parts.push('<p class="note">'+(streaming?'正在整理结构化结果…':'结构化结果未完整解析；可查看原文。')+'</p>'+rawDetails('stream-raw-'+key,lines.slice(start,i).join('\n'),'查看结果原文'));
  }
  if(!found)return undefined;
  if(plain.join('\n').trim())parts.push(markdown(plain.join('\n')));
  return parts.join('')+(reportFound?rawDetails('message-raw-'+id,content,'原始模型消息'):'');
}
export function messageBody(message){
  const content=message.content||'',value=parseContent(content),result=value?.trace_result||value?.final_result;
  if(message.role==='user')return markdown(content);
  const allowReport=!message.role||message.role==='assistant'||message.role==='report';
  const fenced=/^\s*(?:```|~~~)/.test(content);
  if(allowReport&&hasReport(value)&&!fenced)return reportBody(value,message.id)+rawDetails('message-raw-'+message.id,content,'原始模型消息');
  if(value===undefined||(allowReport&&hasReport(value)&&fenced)){const mixed=resultFences(content,message.id,false,allowReport);if(mixed!==undefined)return mixed}
  if(value!==undefined&&!result&&fenced)return markdown(content);
  if(result&&typeof result==='object')return `<div class="message-summary">${result.summary?markdown(result.summary):'<p>模型提交了结构化结果。</p>'}</div><p class="note">模型摘要；验证情况以运行结论和证据状态为准。</p>${Array.isArray(result.answers)?result.answers.map(a=>`<div class="message-answer">${answerBody(a)}</div>`).join(''):''}${Array.isArray(result.reproduction_steps)&&result.reproduction_steps.length?`<h4>复现步骤</h4>${structured(result.reproduction_steps)}`:''}<details id="result-fields-${esc(message.id)}" class="request-details"><summary>结果详情</summary>${structured(Object.fromEntries(Object.entries(result).filter(([key])=>!['summary','answers','reproduction_steps'].includes(key))))}</details>${rawDetails('message-raw-'+message.id,content,'原始模型消息')}`;
  return value!==undefined?`${structured(value)}${rawDetails('message-raw-'+message.id,content,'原始消息')}`:markdown(content);
}
