import {esc,btn} from './ui.js';

// Same kind order and language suffixes as ReportDocument. Historical files
// remain explicit archive choices, never a current-run shortcut.
const reportKinds={final:'最终报告',stage:'阶段报告',finding:'发现报告'};
function currentReport(snap,finalOnly=false){
  for(const kind of finalOnly?['final']:Object.keys(reportKinds)){
    const r=snap?.reports?.findLast(r=>r.kind===kind&&r.source_status==='current'&&r.id&&r.files?.length);
    if(r)return r;
  }
}
const hasLanguage=(r,language)=>r?.files.some(f=>f.name?.endsWith('.'+language+'.md'));
function reportState(snap,finalOnly=false){
  const rows=(snap?.reports||[]).filter(r=>!finalOnly||r.kind==='final');
  if(snap?.projection==='planner'||rows.some(r=>r.source_status==='not_checked')||(snap?.projection==='activity'&&snap.totals?.reports&&!rows.length))return '归档状态待核对';
  if(rows.some(r=>r.source_status==='changed'&&r.files?.length))return '仅有历史归档';
  return finalOnly?'最终报告未归档':'报告未归档';
}
export function reportDownloads(snap,finalOnly=false){
  const r=currentReport(snap,finalOnly);
  if(!r){
    const text=reportState(snap,finalOnly);
    return text.endsWith('未归档')?'<span class="note">'+text+'</span>':'<a href="#history/'+encodeURIComponent(snap.run.id)+'?tab=artifacts">'+text+' · 查看报告与文件</a>';
  }
  const attrs='data-format="report" data-run="'+esc(snap.run.id)+'" data-report-id="'+esc(r.id)+'"';
  return '<span class="note">'+reportKinds[r.kind]+' v'+esc(r.version)+(r.translation_status==='translation_missing'?' · 待补译':'')+'</span>'+
    [['zh-CN','中文'],['en','English']].map(([lang,name])=>hasLanguage(r,lang)?btn('export',name+' ↓',attrs+' data-language="'+lang+'"'):'<span class="note">'+name+' 文件未归档</span>').join('');
}

// Muteki 144df766, lib/clipboard.ts / components/ai-native/code-block.tsx
// (repository AGPL-3.0; code-block credits upstream MIT). Native implementation.
export async function copyText(text){
  if(navigator.clipboard?.writeText){try{await navigator.clipboard.writeText(text);return}catch{}}
  const active=document.activeElement,field=document.createElement('textarea');field.value=text;field.readOnly=true;field.style.cssText='position:fixed;left:-9999px;top:0';
  (document.querySelector('dialog[open]')||document.body).append(field);field.focus({preventScroll:true});field.select();let copied=false;
  try{copied=document.execCommand('copy')}finally{field.remove();active?.focus({preventScroll:true})}
  if(!copied)throw new Error('复制未完成，请选中原文后复制');
}
export function artifactReference(snap,hash){
  const sources=[];
  for(const action of snap?.actions||[])for(const ref of action.result?.artifacts||[])if(ref.hash===hash)sources.push({...ref,request_id:action.request_id,call_id:action.call_id});
  return sources.length?{...sources[0],sources}:undefined;
}
export function mediaKind(mime=''){const type=mime.split(';')[0].trim().toLowerCase();return type.startsWith('image/')?'image':type.startsWith('audio/')?'audio':type.startsWith('video/')?'video':type==='application/pdf'?'pdf':''}
export function mediaMarkup(ref,url){const kind=mediaKind(ref.mime_type),source=esc(url),name=esc(ref.name||ref.filename||'CAS 文件');return kind==='image'?`<img class="artifact-media" src="${source}" alt="${name}">`:kind==='audio'?`<audio class="artifact-media" src="${source}" controls preload="metadata"></audio>`:kind==='video'?`<video class="artifact-media" src="${source}" controls preload="metadata"></video>`:kind==='pdf'?`<iframe class="artifact-pdf" src="${source}" title="${name}" sandbox></iframe>`:''}
export function createDelivery({state,showDetail,notice,isCurrent,token,captureNotice=()=>notice}){
  let url,controller;
  function close(){controller?.abort();controller=null;if(url)URL.revokeObjectURL(url);url=null}
  async function preview(el){
    const ref=artifactReference(state.snap,el.dataset.hash);if(!ref||!mediaKind(ref.mime_type))return false;
    close();const download='/api/artifacts/'+encodeURIComponent(ref.hash)+'/download';
    showDetail(ref.name||'媒体产物',`<p role="status">正在读取关联 CAS 文件…</p><a href="${download}" download>下载原始字节 ↓</a>`,true);
    const current=token(),abort=new AbortController();controller=abort;
    try{
      const response=await fetch(download,{signal:abort.signal});if(!response.ok)throw new Error('产物读取失败（HTTP '+response.status+'）');
      const data=await response.blob();if(abort.signal.aborted||controller!==abort||!isCurrent(current))return true;
      url=URL.createObjectURL(new Blob([data],{type:ref.mime_type}));
      showDetail(ref.sources.length>1?'同字节媒体产物':ref.name||'媒体产物',`<p class="note">${esc(ref.mime_type)} · ${ref.bytes} 字节</p><p class="hash">SHA256 ${esc(ref.hash)}</p>${mediaMarkup(ref,url)}<section><h4>关联来源 · ${ref.sources.length} 项</h4>${ref.sources.map(source=>`<p class="note">${esc(source.name||source.filename||source.kind||'CAS 文件')} · 请求 ${esc(source.request_id)} / 调用 ${esc(source.call_id)}</p>`).join('')}</section><p class="note">相同字节可被多次登记；以上列出当前快照中的全部关联，预览格式采用首项登记的 MIME。浏览器若不支持此格式，可下载原始文件。预览不改变来源验证或报告结论。</p><a href="${download}" download="${esc(ref.name||ref.filename||'')}">下载原始字节 ↓</a>`,true);
      return true;
    }catch(error){if(error.name==='AbortError'||abort.signal.aborted||controller!==abort||!isCurrent(current))return true;showDetail('产物读取失败',`<p class="inline-error">${esc(error.message)}</p><a href="${download}" download>下载原始字节 ↓</a>`,true);return true}
  }
  async function dispatch(action,el){const notice=captureNotice();
    if(action==='copy-code'){await copyText(el.closest('.code-block').querySelector('code').textContent);notice('代码已复制');return true}
    if(action==='copy-message'){let m=(state.snap.messages||[]).find(m=>m.id===el.dataset.message);if(!m)throw new Error('消息已更新，请重新选择');if(m.preview){const response=await fetch(`/api/runs/${encodeURIComponent(state.selected)}/messages/${encodeURIComponent(m.id)}`);if(!response.ok)throw new Error('完整消息读取失败');m=await response.json()}await copyText(m.content||'');notice('消息正文已复制');return true}
    if(action==='copy-link'){await copyText(new URL(el.dataset.href||location.hash,location.href).href);notice('定位链接已复制');return true}
    if(action==='artifact'&&!el.dataset.offset&&await preview(el))return true;
    if(action==='export'){
      const run=state.selected,{format}=el.dataset,params=new URLSearchParams({format});
      let language,stillCurrent;
      if(format==='report'){
        const {reportId,run:buttonRun}=el.dataset,nav=token().nav;
        language=el.dataset.language;
        const matches=()=>!!run&&buttonRun===run&&state.selected===run&&state.snap?.run.id===run&&token().nav===nav;
        if(!matches()||!reportId||(language!=='zh-CN'&&language!=='en')){
          notice('请选择该运行已归档的报告版本和语言',true);return true;
        }
        params.set('report_id',reportId);params.set('language',language);stillCurrent=matches;
      }
      const response=await fetch('/api/runs/'+encodeURIComponent(run)+'/export?'+params);
      if(!response.ok){let detail;try{detail=await response.json()}catch{}throw new Error(detail?.error||'导出失败（HTTP '+response.status+'）')}
      const blob=await response.blob();if(stillCurrent&&!stillCurrent()){notice('运行已切换，已取消下载');return true}
      const href=URL.createObjectURL(blob),link=document.createElement('a');link.href=href;link.download=response.headers.get('Content-Disposition')?.match(/filename="([^"]+)"/)?.[1]||`trace-${run}-${format}.${format==='report'?language+'.md':'json'}`;document.body.append(link);link.click();link.remove();setTimeout(()=>URL.revokeObjectURL(href),1000);notice('文件已读取，浏览器已开始下载');return true;
    }
    return false;
  }
  return {dispatch,close};
}

export function usagePanel(s){
  const base=usageSummaryPanel(s),cache=s.usageSummary?.usage?.prompt_cache;if(!cache)return base;
  const ratio=Number.isFinite(cache.read_ratio)&&cache.read_ratio>=0&&cache.read_ratio<=1?(cache.read_ratio*100).toFixed(1)+'%':'未知',count=value=>Number.isSafeInteger(value)&&value>=0?String(value):'未知';
  return base+`<section class="subsection"><div class="section-label">模型缓存 · 有明细样本</div><dl class="key-values"><dt>缓存读取命中</dt><dd>${ratio} · ${count(cache.known_read_tokens)} / ${count(cache.observed_input_tokens)} 输入 token</dd><dt>明细覆盖</dt><dd>${count(cache.observed_responses)} 已知 / ${count(cache.missing_responses)} 缺失 / ${count(cache.invalid_responses)} 异常响应</dd><dt>已知缓存写入 token</dt><dd>${count(cache.known_write_tokens)}</dd></dl><p class="note">按同时报告有效缓存读取与总输入的响应加权计算；缺明细不计为零，非请求命中率或IPC缓存。费用按请求固定的计费方式展示：legacy_inclusive_input 按全部输入计价；disjoint_cache_v1 分开未缓存输入、缓存读写与输出，缺明细保持未知。并非供应商账单。</p></section>`;
}
function usageSummaryPanel(s){
  const summary=s.usageSummary;if(!summary)return `<section class="subsection"><div class="section-label">父子用量与费用</div>${s.usageError?`<p class="inline-error">${esc(s.usageError)}</p>`:'<p class="note">按需汇总每个运行的实际 usage；缺失用量不计为零。</p>'}${btn('usage-refresh','读取用量与费用')}${btn('run-diagnostics','请求身份 / 分段用时 / 费用')}</section>`;
  const u=summary.usage||{},cost=summary.cost||{},pool=summary.pool||{},amount=value=>typeof value==='string'||typeof value==='number'?esc(value):'未知';
  return `<section class="subsection"><div class="section-label">父子用量与费用 <span>${summary.run_ids?.length||0} 个运行</span></div>${s.usageError?`<p class="inline-error">${esc(s.usageError)} · 以下保留上次成功读取结果</p>`:''}<dl class="key-values"><dt>已知输入 / 输出 token</dt><dd>${u.known_input_tokens??'未知'} / ${u.known_output_tokens??'未知'}</dd><dt>已知总 token</dt><dd>${u.known_total_tokens??'未知'}</dd><dt>缺失输入 / 输出 / 总量</dt><dd>${summary.missing_input_responses??'未知'} / ${summary.missing_output_responses??'未知'} / ${summary.missing_total_responses??'未知'}</dd><dt>估算费用${cost.complete?'':'（不完整）'}</dt><dd>${cost.priced_responses?amount(cost.known_amount)+' '+esc(cost.currency||''):'未知 / 尚无已定价响应'}</dd><dt>尚未定价响应</dt><dd>${cost.unpriced_responses??'未知'}</dd>${pool.limit?`<dt>费用池上限</dt><dd>${amount(pool.limit)} ${esc(pool.currency)}</dd><dt>已知余额${pool.complete?'':'（未含未知费用）'}</dt><dd>${amount(pool.remaining)}</dd><dt>额度状态</dt><dd>${pool.exhausted?'已达到':'未达到'}参考额度</dd>`:''}</dl><p class="note">按运行 ID 去重汇总 reported usage，再按请求固定价格估算费用，并非供应商账单。上下文估算不计费，参考额度不触发执行硬限。未知费用与缺失 usage 不当作零。${summary.run_ids?.length?'包含 '+esc(summary.run_ids.join('、')):''}</p><details><summary>价格来源与精度</summary>${(summary.rates||[]).map(rate=>`<p class="note">${esc(rate.model)} · 每百万输入 ${amount(rate.input_per_million)} / 输出 ${amount(rate.output_per_million)} ${esc(rate.currency)}${rate.cache_read_per_million!==undefined?' / 缓存读取 '+amount(rate.cache_read_per_million):''}${rate.cache_write_per_million!==undefined?' / 缓存写入 '+amount(rate.cache_write_per_million):''} · ${esc(rate.billing_basis||'legacy_inclusive_input')}<br>来源：${esc(rate.source)} · 生效：${esc(rate.effective_at||'未指定')}</p>`).join('')||'<p class="note">未配置价格；费用保持未知。</p>'}<p class="note">按服务端十进制字符串展示价格和金额，保留精度；旧请求没有价格时保持未定价。</p></details>${btn('usage-refresh','刷新用量与费用')}${btn('run-diagnostics','请求身份 / 分段用时 / 费用')}</section>`;
}
