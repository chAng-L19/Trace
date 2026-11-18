import {esc} from './ui.js';

// Row identities survive page changes; values remain in the existing tab-only draft store.
const additions=new Map();
export function addModelRow(group){const index=additions.get(group)??(group==='providers'?0:1);additions.set(group,index+1);return index}
export function resetModelRows(){additions.clear()}
const option=(value,label,current)=>`<option value="${esc(value)}" ${value===current?'selected':''}>${esc(label)}</option>`;
const input=(id,label,value='',type='text')=>`<label>${esc(label)}<input id="${id}" type="${type}" value="${esc(value)}" data-config-base="${esc(value)}" autocomplete="off"></label>`;
const select=(id,label,value,options)=>`<label>${esc(label)}<select id="${id}" data-config-base="${esc(value)}">${options}</select></label>`;
const flag=(id,label,value)=>select(id,label,String(!!value),option('false','否',String(!!value))+option('true','是',String(!!value)));
const remove=id=>flag(id+'-remove','删除此行',false);
const plus=group=>`<button type="button" class="small" data-action="model-row-add" data-group="${esc(group)}">＋ 添加一行</button>`;
const rows=(items,group)=>[...items,...Array.from({length:additions.get(group)??(group==='providers'?0:1)},()=>null)];
export const providerRows=c=>rows(Object.entries(c.providers||{}),'providers');
const efforts=value=>(value||[]).join(', ');
const split=value=>[...new Set(value.split(/[,，\s]+/).filter(Boolean))];
const entries=c=>({default:c.provider||{},...c.providers});
const priceFields={model:'模型',provider_id:'Provider 身份（可选）',currency:'币种',input_per_million:'每百万输入价格',output_per_million:'每百万输出价格',cache_read_per_million:'每百万缓存读取价格（可选）',cache_write_per_million:'每百万缓存写入价格（可选）',source:'价格来源',effective_at:'生效时间（ISO 8601）'};
function priceSettings(c){return `<details id="model-price-forms" class="config-section model-settings"><summary>模型价格表 <span>可选</span></summary><p class="note">未配置价格保持未知。分项计费需要明确缓存读写价格与 usage；报价不等于 Provider 账单。币种与参考费用池在下方保留。</p>${rows(c.costs?.rates||[],'prices').map((price,i)=>{const id='model-price-'+i,p=price||{},basis=p.billing_basis||'';return `<fieldset data-model-price="${i}"><legend>${esc(p.model||'新增报价')}</legend><div class="model-settings-row">${Object.entries(priceFields).map(([key,label])=>input(id+'-'+key,label,p[key]??'')).join('')}${select(id+'-billing_basis','计费方式',basis,option('','旧配置：全部输入单价',basis)+option('legacy_inclusive_input','全部输入单价',basis)+option('disjoint_cache_v1','未缓存输入 / 缓存读写分开计价',basis))}${remove(id)}</div></fieldset>`}).join('')}${plus('prices')}</details>`}
export function modelChoices(c,provider,model){
 const p=entries(c)[provider]||{},declared=p.model_capabilities?.[model];
 return {models:[...new Set([p.model,...Object.keys(p.model_capabilities||{})].filter(Boolean))],efforts:declared?.reasoning_efforts??(p.model===model?p.reasoning_efforts:[])??[],source:declared?'逐模型配置声明':p.model===model?'Provider 默认模型声明':'未声明能力'};
}
function capabilities(provider,index){
 const group='caps-'+index;
 return `<details id="${group}" class="model-capabilities"><summary>逐模型能力 · 配置声明</summary><p class="note">声明来自本页配置，不表示已探测通过。未知模型不继承其他模型能力；保留旧模型声明可供既有运行恢复。</p>${rows(Object.entries(provider.model_capabilities||{}),group).map((row,i)=>{const [model,caps]=row||['',{}],id=group+'-'+i;return `<div class="model-settings-row" data-capability-row="${index}" data-row="${i}">${input(id+'-model','模型名称',model)}${flag(id+'-image','图像输入',caps.image_input)}${input(id+'-efforts','支持的思考强度（逗号分隔）',efforts(caps.reasoning_efforts))}${remove(id)}</div>`}).join('')}${plus(group)}</details>`;
}
export function providerCapabilitySettings(c){
 return `<details id="default-capability-forms" class="model-settings provider-capability-settings"><summary>模型能力 <span>配置声明</span></summary>${flag('provider-image-declaration','原生图像输入',c.provider?.image_input)}${input('model-default-efforts','支持的思考强度（逗号分隔）',efforts(c.provider?.reasoning_efforts))}${capabilities(c.provider||{},'default')}</details>`;
}
export function namedProviderEditors(c,selected){
 return providerRows(c).map((row,i)=>{const [name,p]=row||['',{}],id='model-provider-'+i;return `<section class="provider-editor-fields model-settings" data-model-provider="${i}" data-profile-panel="${i}" ${selected===String(i)?'':'hidden'}><div class="provider-editor-heading"><div><p class="eyebrow">${name?'已保存配置':'新建配置'}</p><h2>${esc(name||'添加模型连接')}</h2></div><span class="provider-editor-state">${name===(c.active_provider||'default')?'当前默认':'未启用'}</span></div><div class="provider-connection-fields">${input(id+'-name','配置名称',name)}${input(id+'-url','API 地址',p.url||'','url')}${input(id+'-model','模型名称',p.model||'')}${input(id+'-key','API Key',p.api_key||'','password')}</div><p class="note provider-secret-note">保留 [stored secret] 沿用已保存密钥；留空密钥会清除已有值。密钥只保存在 Trace 服务所在机器。</p><details id="provider-options-${i}" class="provider-capability-settings"><summary>模型能力与配置管理</summary>${flag(id+'-image','原生图像输入',p.image_input)}${input(id+'-efforts','支持的思考强度（逗号分隔）',efforts(p.reasoning_efforts))}${capabilities(p,i)}${remove(id)}<p class="note">删除在保存时生效；正在使用的配置须先切换默认。更名后需重新填写密钥。</p></details></section>`}).join('');
}
export function modelSettings(c){
 const names=Object.keys(entries(c));
 return `<details id="model-role-forms" class="config-section model-settings"><summary>角色与阶段模型 <span>默认继承</span></summary><p class="note">未单独指定的编排者与 Worker 使用当前默认；信息收集继承 Worker。仅新运行使用新默认，已有运行保留模型绑定。</p>${['orchestrator','worker','recon'].map(role=>{const value=c.role_models?.[role],id='model-role-'+role,p=value?value.provider||'default':role==='recon'&&c.role_models?.worker?c.role_models.worker.provider||'default':c.active_provider||'default',model=value?.model||(role==='recon'?c.role_models?.worker?.model:undefined)||entries(c)[p]?.model||'',effort=value?.reasoning_effort||'',choices=modelChoices(c,p,model);return `<fieldset class="model-role" data-model-role="${role}"><legend>${({orchestrator:'编排者',worker:'Worker',recon:'信息收集'})[role]}</legend><div class="model-settings-row">${select(id+'-mode','默认方式',value?'custom':'inherit',option('inherit',role==='recon'?'继承 Worker':'继承默认 Provider',value?'custom':'inherit')+option('custom','独立选择',value?'custom':'inherit'))}${select(id+'-provider','Provider',p,[...new Set([...names,p])].map(name=>option(name,name,p)).join(''))}${input(id+'-model','模型名称',model)}${select(id+'-effort','思考强度',effort,option('','模型默认',effort)+[...new Set([...choices.efforts,...(effort?[effort]:[])])].map(v=>option(v,v,effort)).join(''))}</div><p id="${id}-source" class="note">${esc(choices.source)} · 连通性检测与能力声明独立</p></fieldset>`}).join('')}<details id="stage-route-forms"><summary>任务可选阶段模型</summary><p class="note">仅列明的 Provider / 模型对可供任务选择；空列表关闭阶段选择，不新增自动调度。</p>${rows(c.role_models?.stage_routes||[],'routes').map((route,i)=>`<div class="model-settings-row" data-model-route="${i}">${input('model-route-'+i+'-provider','Provider',route?.provider||'')}${input('model-route-'+i+'-model','模型',route?.model||'')}${remove('model-route-'+i)}</div>`).join('')}${plus('routes')}</details></details>${priceSettings(c)}`;
}

// Advanced JSON supplies the base. Only edited row fields override it, so opening
// the ordinary form cannot erase supported fields or overwrite advanced edits.
export function readModelSettings(config,get=id=>document.getElementById(id),query=selector=>document.querySelectorAll(selector)){
 const c=structuredClone(config),el=id=>get(id),value=id=>el(id)?.value||'',changed=id=>!!el(id)&&el(id).dataset?.configBase!==undefined&&value(id)!==el(id).dataset.configBase;
 const set=(obj,key,id,convert=v=>v)=>{if(changed(id))obj[key]=convert(value(id))};
 const readCaps=(p,index)=>{
  const next={...p.model_capabilities};let touched=false;
  for(const row of query(`[data-capability-row="${index}"]`)){
   const i=Number(row.dataset.row),id='caps-'+index+'-'+i,original=el(id+'-model')?.dataset?.configBase||'',name=value(id+'-model').trim();
   if(!['model','image','efforts','remove'].some(key=>changed(id+'-'+key)))continue;touched=true;
   if(value(id+'-remove')==='true'){delete next[original];continue}
   if(!name)throw new Error('能力声明需要模型名称');
   if(name!==original&&Object.hasOwn(next,name))throw new Error('逐模型能力名称重复：'+name);
   const caps={...next[original]};set(caps,'image_input',id+'-image',v=>v==='true');set(caps,'reasoning_efforts',id+'-efforts',split);
   delete next[original];next[name]=caps;
  }
  if(touched)p.model_capabilities=next;
 };
 c.provider={...c.provider};set(c.provider,'image_input','provider-image-declaration',v=>v==='true');set(c.provider,'reasoning_efforts','model-default-efforts',split);readCaps(c.provider,'default');
 for(const row of query('[data-model-provider]')){
  const i=row.dataset.modelProvider,id='model-provider-'+i,original=el(id+'-name')?.dataset?.configBase||'',name=value(id+'-name').trim(),p={...c.providers?.[original]};
  const touched=['name','url','model','key','image','efforts','remove'].some(key=>changed(id+'-'+key));
  readCaps(p,i);if(!touched&&JSON.stringify(p)===JSON.stringify(c.providers?.[original]||{}))continue;
  c.providers={...c.providers};if(value(id+'-remove')==='true'){delete c.providers[original];continue}
  if(!name||name==='default'||name!==original&&Object.hasOwn(c.providers,name))throw new Error('命名 Provider 需要唯一名称，且不得使用 default');
  set(p,'url',id+'-url',v=>v.trim());set(p,'model',id+'-model',v=>v.trim());set(p,'api_key',id+'-key');set(p,'image_input',id+'-image',v=>v==='true');set(p,'reasoning_efforts',id+'-efforts',split);
  if(name!==original&&p.api_key==='[stored secret]')throw new Error('Provider 更名后请重新填写 API Key，或留空清除；草稿已保留。');
  if(!p.url||!p.model)throw new Error('命名 Provider 需要 API 地址与默认模型');delete c.providers[original];c.providers[name]=p;
 }
 for(const role of ['orchestrator','worker','recon']){
  const id='model-role-'+role;if(!['mode','provider','model','effort'].some(key=>changed(id+'-'+key)))continue;
  c.role_models={...c.role_models};if(value(id+'-mode')==='inherit'){delete c.role_models[role];continue}
  const original=c.role_models?.[role],initialize=changed(id+'-mode')||!original;
  const provider=initialize||changed(id+'-provider')?value(id+'-provider'):original.provider||'default',p=entries(c)[provider];
  const model=(initialize||changed(id+'-model')?value(id+'-model').trim():original.model)||p?.model,effort=(initialize||changed(id+'-effort')?value(id+'-effort'):original.reasoning_effort)||'';
  if(!p||!model)throw new Error('角色需要已配置 Provider 与模型');if(effort&&!modelChoices(c,provider,model).efforts.includes(effort))throw new Error('所选模型未声明支持该思考强度');
  c.role_models[role]={...c.role_models[role],provider,model,reasoning_effort:effort};
 }
 const routeRows=[...query('[data-model-route]')];
 if(routeRows.some(row=>['provider','model','remove'].some(key=>changed('model-route-'+row.dataset.modelRoute+'-'+key)))){
  const baseline=routeRows.map(row=>{const id='model-route-'+row.dataset.modelRoute;return ['provider','model'].map(key=>el(id+'-'+key)?.dataset.configBase||'')}).filter(row=>row.some(Boolean));
  if(JSON.stringify(baseline)!==JSON.stringify((c.role_models?.stage_routes||[]).map(route=>[route.provider||'',route.model||''])))throw new Error('阶段路由 JSON 已改变行结构；草稿保留，请分别保存 JSON 或行表单的修改。');
  const next=[];for(const row of routeRows){const id='model-route-'+row.dataset.modelRoute,provider=value(id+'-provider').trim(),model=value(id+'-model').trim();if(value(id+'-remove')==='true'||!provider&&!model)continue;if(!entries(c)[provider]||!model)throw new Error('阶段路由需要已配置 Provider 与模型');if(next.some(r=>r.provider===provider&&r.model===model))throw new Error('阶段路由重复');next.push({provider,model})}c.role_models={...c.role_models,stage_routes:next};
 }
 const prices=[...query('[data-model-price]')];
 if(prices.some(row=>[...Object.keys(priceFields),'billing_basis','remove'].some(key=>changed('model-price-'+row.dataset.modelPrice+'-'+key)))){
  const identity=['model','provider_id','currency','effective_at'],baseline=prices.map(row=>identity.map(key=>el('model-price-'+row.dataset.modelPrice+'-'+key)?.dataset.configBase||'')).filter(row=>row.some(Boolean));
  if(JSON.stringify(baseline)!==JSON.stringify((c.costs?.rates||[]).map(price=>identity.map(key=>price[key]||''))))throw new Error('价格 JSON 已改变行身份或顺序；草稿保留，请分别保存 JSON 或行表单的修改。');
  const next=[];for(const row of prices){const i=Number(row.dataset.modelPrice),id='model-price-'+i;if(value(id+'-remove')==='true')continue;const p={...c.costs?.rates?.[i]};for(const key of [...Object.keys(priceFields),'billing_basis'])set(p,key,id+'-'+key,v=>v.trim());if(Object.values(p).every(v=>v===''))continue;if(!Object.keys(p).length)continue;for(const key of ['provider_id','cache_read_per_million','cache_write_per_million','billing_basis'])if(p[key]==='')delete p[key];next.push(p)}c.costs={...c.costs,rates:next};
 }
 return c;
}
export function syncModelFields(config,changed,get=id=>document.getElementById(id)){
 if(!get('model-default-efforts'))return;
 const preview=structuredClone(config);preview.provider={...preview.provider,model:get('provider-model')?.value??preview.provider?.model,reasoning_efforts:split(get('model-default-efforts')?.value||'')};
 for(const row of document.querySelectorAll('[data-model-provider]')){const id='model-provider-'+row.dataset.modelProvider,name=get(id+'-name')?.value.trim();if(name&&get(id+'-remove')?.value!=='true')preview.providers={...preview.providers,[name]:{...preview.providers?.[name],model:get(id+'-model')?.value,reasoning_efforts:split(get(id+'-efforts')?.value||'')}}}
 for(const row of document.querySelectorAll('[data-capability-row]')){const index=row.dataset.capabilityRow,id='caps-'+index+'-'+row.dataset.row,model=get(id+'-model')?.value.trim(),name=index==='default'?'default':get('model-provider-'+index+'-name')?.value.trim(),p=entries(preview)[name];if(model&&p&&get(id+'-remove')?.value!=='true')p.model_capabilities={...p.model_capabilities,[model]:{image_input:get(id+'-image')?.value==='true',reasoning_efforts:split(get(id+'-efforts')?.value||'')}}}
 for(const role of ['orchestrator','worker','recon']){
  const id='model-role-'+role,mode=get(id+'-mode');if(!mode)continue;
  const provider=get(id+'-provider'),model=get(id+'-model'),effort=get(id+'-effort');
  provider.innerHTML=[...new Set([...Object.keys(entries(preview)),provider.value])].map(name=>option(name,name,provider.value)).join('');
  for(const field of [provider,model,effort])field.disabled=mode.value!=='custom';
  if(changed===id+'-provider'){model.value=entries(preview)[provider.value]?.model||'';effort.value=''}
  if(changed===id+'-model')effort.value='';
  const choices=modelChoices(preview,provider.value,model.value),current=effort.value;
  effort.innerHTML=option('','模型默认',current)+[...new Set([...choices.efforts,...(current?[current]:[])])].map(v=>option(v,v+(choices.efforts.includes(v)?'':'（声明待核对）'),current)).join('');
  get(id+'-source').textContent=choices.source+' · 目录发现 / 连通性检测均不证明该能力';
 }
}
