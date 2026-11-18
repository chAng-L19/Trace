import {esc} from './ui.js';

const sources={fofa:'FOFA',quake:'Quake',shodan:'Shodan'};
const limits={max_concurrency:[1,1,5,'并发请求'],min_interval_ms:[1000,0,60000,'请求间隔（毫秒，0 使用默认）'],max_pages:[5,1,100,'单查询最多页数'],max_requests:[20,1,10000,'每根会话请求上限'],max_results:[1000,1,1000000,'每根会话请求结果上限']};
const option=(value,title,selected)=>`<option value="${esc(value)}" ${value===selected?'selected':''}>${esc(title)}</option>`;
const input=(id,title,value,type='text',attrs='')=>`<label>${esc(title)}<input id="${id}" type="${type}" value="${esc(value)}" data-config-base="${esc(value)}" autocomplete="off" ${attrs}></label>`;
const providers=c=>({default:c.provider||{},...c.providers});
export function reconModelOptions(c,provider,model){
 const p=providers(c)[provider]||{},models=[...new Set([p.model,...Object.keys(p.model_capabilities||{})].filter(Boolean))];
 const efforts=p.model_capabilities?.[model]?.reasoning_efforts??(model===p.model?p.reasoning_efforts:[])??[];
 return {models,efforts};
}
export function reconSettings(c,includeModel=true){
 const role=c.role_models?.recon,selectedProvider=role?role.provider||'default':c.role_models?.worker?c.role_models.worker.provider||'default':c.active_provider||'default',p=providers(c)[selectedProvider]||{},model=role?.model||p.model||'',effort=role?.reasoning_effort||'',mode=role?'custom':'inherit';
 const choices=reconModelOptions(c,selectedProvider,model),providerNames=[...new Set([...Object.keys(providers(c)),selectedProvider])];
 const select=(id,title,value,options)=>`<label>${title}<select id="${id}" data-config-base="${esc(value)}">${options}</select></label>`;
 return `${includeModel?`<section id="recon-settings" class="config-section"><div class="section-label">信息收集模型 <span>仅影响新建的收集任务</span></div><p class="note">整理已有资料后按任务缺口补采；代码、文件和资料任务使用各自相关来源。收集结果通过固定来源交给后续任务。</p>${select('recon-mode','模型选择',mode,option('inherit','继承 Worker 默认模型',mode)+option('custom','独立选择信息收集模型',mode))}<fieldset id="recon-model-fields" ${role?'':'disabled'}><div class="form-row">${select('recon-provider','Provider',selectedProvider,providerNames.map(name=>option(name,name==='default'?'默认 Provider':name,selectedProvider)).join(''))}${input('recon-model','模型',model,'text','list="recon-model-options"')}<datalist id="recon-model-options">${choices.models.map(name=>option(name,name,'')).join('')}</datalist></div>${select('recon-effort','思考强度',effort,option('','使用模型默认强度',effort)+[...new Set([...choices.efforts,...(effort?[effort]:[])])].map(value=>option(value,value+(choices.efforts.includes(value)?'':'（当前声明待核对）'),effort)).join(''))}</fieldset><p class="note">Provider 与模型能力沿用已保存配置。任务开始后固定实际模型与思考强度，恢复继续使用原绑定。</p></section>`:''}<section class="config-section"><div class="section-label">信息收集数据源 <span>原生搜索 API</span></div><p class="note">密钥保存在服务器配置中，保留 [stored secret] 沿用原值，留空清除。各 Worker 共享根会话的数据源额度；这是 API 调用量设置。</p>${Object.entries(sources).map(([id,title])=>{const s=c.search_sources?.[id]||{},enabled=s.enabled?'true':'false';return `<details id="source-${id}"><summary>${title} · ${s.enabled?'已启用':'未启用'}</summary>${select('source-'+id+'-enabled','启用状态',enabled,option('false','未启用',enabled)+option('true','启用',enabled))}${input('source-'+id+'-api_key','API Key',s.api_key||'','password')}${id==='fofa'?input('source-fofa-email','账号邮箱（按账号接口要求，可留空）',s.email||''):''}<div class="buttons section-gap"><button type="button" id="source-${id}-probe" data-action="search-source-probe" data-source="${id}">检查已保存配置</button></div><p id="source-${id}-probe-status" class="note" role="status" aria-live="polite">已保存配置尚未检查</p><p class="note">只检查已保存账号，不提交搜索、不保存草稿，也不创建任务。</p><details><summary>查询额度与连接设置</summary>${input('source-'+id+'-base_url','API 基址（留空使用官方地址）',s.base_url||'','url')}<div class="form-row">${Object.entries(limits).map(([key,[fallback,min,max,label]])=>input('source-'+id+'-'+key,label,s[key]??fallback,'number',`min="${min}" max="${max}" step="1"`)).join('')}</div></details></details>`}).join('')}</section>`;
}
export function readReconSettings(config,get=id=>document.getElementById(id)){
 const c=structuredClone(config),mode=get('recon-mode');
 const read=id=>{const el=get(id);if(!el)throw new Error('信息收集配置字段缺失，请重新读取配置');if(el.dataset?.configConflict==='true')throw new Error('信息收集配置已更新，请保留草稿并基于最新配置重新编辑');return el.value};
 const currentMode=mode?read('recon-mode'):null;
 if(currentMode==='custom'){
  const provider=read('recon-provider'),model=read('recon-model').trim(),reasoning_effort=read('recon-effort');
  if(!providers(c)[provider])throw new Error('请选择已配置的 Provider');
  if(!model)throw new Error('请填写信息收集模型');
  if(reasoning_effort&&!reconModelOptions(c,provider,model).efforts.includes(reasoning_effort))throw new Error('所选模型未声明支持该思考强度');
  c.role_models={...c.role_models,recon:{...c.role_models?.recon,provider,model,reasoning_effort}};
 }else if(currentMode==='inherit'){
  if(c.role_models)delete c.role_models.recon;
 }else if(mode)throw new Error('信息收集模型选择无效');
 if(!get('source-fofa-enabled'))return c;
 for(const id of Object.keys(sources)){
  const fields=['enabled','api_key','base_url',...(id==='fofa'?['email']:[]),...Object.keys(limits)];
  if(!fields.some(field=>{const el=get('source-'+id+'-'+field);return el&&(el.dataset?.configBase===undefined||el.value!==el.dataset.configBase)}))continue;
  const old=c.search_sources?.[id],key='source-'+id+'-',enabled=read(key+'enabled');
  if(!['true','false'].includes(enabled))throw new Error('数据源启用状态无效');
  const next={...old,enabled:enabled==='true',api_key:read(key+'api_key'),base_url:read(key+'base_url').trim()};
  if(id==='fofa')next.email=read(key+'email');
  for(const [field,[fallback,min,max,label]] of Object.entries(limits)){
   const raw=read(key+field),value=Number(raw);
   if(raw.trim()===''||!Number.isInteger(value)||value<min||value>max)throw new Error(label+'须为 '+min+'–'+max+' 的整数');
   next[field]=value;
  }
  if(!old&&!next.enabled&&!next.api_key&&!next.email&&!next.base_url&&Object.entries(limits).every(([key,[value]])=>next[key]===value))continue;
  c.search_sources={...c.search_sources,[id]:next};
 }
 return c;
}
export function syncReconFields(config,changed,get=id=>document.getElementById(id)){
 const mode=get('recon-mode');if(!mode)return;
 const group=get('recon-model-fields');group.disabled=mode.value!=='custom';
 const provider=get('recon-provider'),model=get('recon-model'),effort=get('recon-effort'),list=get('recon-model-options');
 if(changed==='recon-provider'){model.value=providers(config)[provider.value]?.model||'';effort.value=''}
 const choices=reconModelOptions(config,provider.value,model.value),value=effort.value;
 list.innerHTML=choices.models.map(name=>option(name,name,'')).join('');
 effort.innerHTML=option('','使用模型默认强度',value)+[...new Set([...choices.efforts,...(value?[value]:[])])].map(item=>option(item,item+(choices.efforts.includes(item)?'':'（当前声明待核对）'),value)).join('');
}
