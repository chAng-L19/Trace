// A submitted draft owns the revision it was first edited against, including
// navigation away/back and an explicit reload while that draft is retained.
const baselines=new Map();
const key=(state,form)=>`${state.settingTab}:${form.id}`;
export function rememberConfigRevision(state,form){
 if(!form||!['config-form','mcp-form','integration-form'].includes(form.id))return;
 const id=key(state,form);if(!baselines.has(id))baselines.set(id,form.dataset.configRevision??state.config?.revision??'');
}
export function configRevision(state,form){return form?(baselines.get(key(state,form))??form.dataset.configRevision??state.config?.revision??''):state.config?.revision??''}
export function forgetConfigRevision(state,form){if(form)baselines.delete(key(state,form))}
export function configHeaders(revision){return revision?{'If-Match':'"'+revision+'"'}:{}}
export function configConflictMessage(error){return ['config_revision_required','config_revision_conflict'].includes(error.code)?'配置版本已变化，未保存草稿已保留。重新读取后请对比最新配置；复制需要保留的草稿并丢弃本页草稿，再重新编辑保存。':error.message}

export function mergeToolSelection(selected,tools,adapter,checked){
 const scope=new Set(Array.isArray(adapter)?adapter:tools.filter(t=>t.adapter===adapter).map(t=>t.definition.function.name));
 const previous=selected===undefined?tools.filter(t=>['mcp','local'].includes(t.adapter)).map(t=>t.definition.function.name):selected;
 return [...new Set([...previous.filter(id=>!scope.has(id)),...checked])];
}
