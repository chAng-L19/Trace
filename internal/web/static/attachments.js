import {esc} from './ui.js';

// File objects stay in tab memory across SSE redraws; never browser storage.
const drafts=new Map();
const scope=(s,form)=>form==='new-run-form'?'new':s.selected;
export function attachmentPicker(disabled=false){return `<label>输入附件（每个最多 20 MiB）<input type="file" id="input-files" multiple data-ephemeral="true" ${disabled?'disabled':''}></label><div data-input-draft></div>`}
export function attachmentRecords(items=[]){return items.map(a=>`<article class="input-attachment"><a href="/api/inputs/${esc(a.id)}" download>${esc(a.name)} ↓</a><small>${esc(a.mime_type)} · ${Number(a.bytes).toLocaleString()} 字节 · 用户输入 · ${a.delivery==='native_image'?'原生图像':a.delivery==='workspace_file'?'文件路径交付':'上传已保存，待交付'}</small><details><summary>输入来源与 SHA256</summary><pre>${esc(a.hash)}\n来源运行 ${esc(a.source_run_id||'待绑定')}\n来源消息 ${esc(a.source_message_id||'待绑定')}</pre></details>${['image/png','image/jpeg','image/gif','image/webp'].includes(a.mime_type)?`<img class="input-image" src="/api/inputs/${esc(a.id)}?preview=1" alt="${esc(a.name)}" loading="lazy">`:''}</article>`).join('')}
export function chooseAttachments(s,input){
  const key=scope(s,input.form.id),items=drafts.get(key)||[];
  for(const file of input.files)if(file.size>20*1024*1024)throw new Error(`${file.name} 超过 20 MiB`);
  for(const file of input.files)items.push({file});
  drafts.set(key,items);input.value='';paintAttachments(s);
}
export function paintAttachments(s){
  const box=document.querySelector('[data-input-draft]');if(!box)return;
  const form=box.closest('form'),items=drafts.get(scope(s,form.id))||[];
  box.innerHTML=items.map((item,i)=>`<div class="form-row"><span>${esc(item.file.name)} · ${item.file.size.toLocaleString()} 字节${item.input?' · 已上传':''}</span><button type="button" data-action="remove-input" data-index="${i}">移除</button></div>`).join('');
}
export function removeAttachment(s,el){const key=scope(s,el.closest('form').id),items=drafts.get(key)||[];items.splice(Number(el.dataset.index),1);paintAttachments(s)}
export async function uploadAttachments(s,form){
  const items=[...(drafts.get(scope(s,form.id))||[])];
  for(const item of items){
    if(item.input)continue;
    const body=new FormData();body.append('file',item.file);
    const response=await fetch('/api/inputs',{method:'POST',body});
    const text=await response.text();let result;try{result=JSON.parse(text)}catch{throw new Error(`附件上传失败（HTTP ${response.status}）`)}
    if(!response.ok)throw new Error(result.error||'附件上传失败');item.input=result;
  }
  const ids=items.map(item=>item.input.id);Object.defineProperty(ids,'draftItems',{value:items});return ids;
}
export function clearAttachments(s,form,submitted){const key=scope(s,form.id);if(!submitted){drafts.delete(key);return}const remaining=(drafts.get(key)||[]).filter(item=>!submitted.draftItems.includes(item));if(remaining.length)drafts.set(key,remaining);else drafts.delete(key)}

export function taskInputPicker(inputs=[]){
  const unique=[...new Map(inputs.map(input=>[input.id,input])).values()];
  if(!unique.length)return '';
  return `<fieldset><legend>交付给此 Worker 的输入附件</legend><p class="note">仅交付勾选的输入；保留原始用户消息来源，不成为子任务执行证据。</p>${unique.map(a=>`<label class="checkbox-label"><input type="checkbox" id="task-input-${esc(a.id)}" data-task-input="${esc(a.id)}">${esc(a.name)} · ${esc(a.hash.slice(0,12))}</label>`).join('')}</fieldset>`;
}
