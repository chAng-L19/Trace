// Results stay in memory and belong to exact fields plus the saved credential generation.
// Credentials are never rendered, logged, or put in browser storage here.
export function createProviderProbe({api,getState,getElement,onChange=()=>{}}){
  let identity='',controller=null,sequence=0,result=null;
  const observed=new Map(),profile=(c,name)=>name==='default'?c?.provider:c?.providers?.[name];
  const signature=(provider,name='default',revision=getState().config?.revision||'',storedName=name)=>JSON.stringify({name,storedName,url:provider?.url||'',model:provider?.model||'',api_key:provider?.api_key||'',revision});
  const savedResult=(name='default')=>observed.get(name)?.identity===signature(profile(getState().config,name),name)?observed.get(name).result:null;
  const active=()=>{const s=getState();return s.page==='settings'&&s.settingTab==='provider'&&!!s.config&&!!getElement('provider-url')};
  const draft=()=>{const selected=getElement('provider-editor-selection')?.value||'default',prefix=selected==='default'?'provider':'model-provider-'+selected,nameField=getElement(prefix+'-name');return {name:selected==='default'?'default':nameField?.value.trim()||'新增配置',storedName:selected==='default'?'default':nameField?.dataset?.configBase||'',fields:{url:getElement(prefix+'-url')?.value||'',model:getElement(prefix+'-model')?.value||'',api_key:getElement(prefix+'-key')?.value||''}}};
  const activeObservation=()=>{const name=getState().config?.active_provider||'default',provider=profile(getState().config,name);return {provider:name,...savedResult(name),status:savedResult(name)?.status||(provider?(provider.url?'unprobed':'mock'):'unconfigured'),matchesSavedConfig:true}};
  function reset(){sequence++;controller?.abort();controller=null;result=null}
  function sync(){
    if(!active()){
      if(controller)reset();identity='';
      onChange(activeObservation());
      return;
    }
    const c=getState().config,{name,storedName,fields}=draft(),next=signature(fields,name,c.revision||'',storedName),saved=profile(c,name);
    if(next!==identity){reset();identity=next;result=observed.get(name)?.identity===next?observed.get(name).result:null}
    const mock=fields.url==='',matchesSavedConfig=storedName===name&&!!saved&&['url','model','api_key'].every(key=>fields[key]===(saved[key]||''));
    const probeStatus=controller?'probing':result?.status||(mock?'mock':'unprobed');
    onChange({provider:name,status:probeStatus,matchesSavedConfig,model:fields.model,savedResult:savedResult(name),activeResult:activeObservation(),detail:result?.detail,elapsed_ms:result?.elapsed_ms,checked_at:result?.checked_at});
    const status=getElement('provider-probe-status'),button=getElement('provider-probe-button'),cancel=getElement('provider-probe-cancel');
    if(!status||!button||!cancel)return;
    const elapsed=Number.isFinite(result?.elapsed_ms)?` · ${result.elapsed_ms} ms`:'';
    status.textContent=controller?`${name} · 正在检测连通性…`:result?`${name} · ${matchesSavedConfig?'当前配置':'未保存草稿'}：${result.status==='success'?'检测通过 · ':result.status==='responded'?'收到响应 · ':''}${result.detail}${elapsed}`:mock?(name==='default'?'Mock 模式 · 不发送网络请求':'填写 API 地址后检测'):!fields.model.trim()?'填写模型名称后检测':`${name} · ${matchesSavedConfig?'当前配置':'未保存草稿'}尚未检测`;
    status.className=result?.status==='failed'?'inline-error':'note';
    status.dataset.status=probeStatus;
    const panel=getElement('provider-probe-panel');if(panel)panel.dataset.status=probeStatus;
    const time=getElement('provider-probe-time');if(time){const checked=new Date(result?.checked_at||'');time.textContent=Number.isNaN(checked.getTime())?'':`检测时间 ${checked.toLocaleTimeString('zh-CN',{hour12:false})} · 结果仅对应本次连接配置`;time.hidden=!time.textContent}
    button.disabled=!!controller||mock||!fields.model.trim()||!!button.closest?.('form[aria-busy="true"]');button.textContent=controller?'检测中…':'检测连通性';
    cancel.hidden=!controller;
  }
  async function start(){
    sync();if(!active()||controller)return;
    const {name,storedName,fields}=draft();if(!fields.url||!fields.model.trim())return;
    if(!storedName&&fields.api_key==='[stored secret]'){result={status:'failed',detail:'新增配置请填写 API Key，或留空使用无密钥接口'};sync();return}
    const token=++sequence,current=identity;
    controller=new AbortController();const signal=controller.signal;result=null;sync();
    try{
      const value=await api('/provider/probe','POST',storedName==='default'?fields:{...fields,provider:storedName||name},{signal});
      const latest=active()?draft():null;
      if(token!==sequence||!latest||identity!==current||signature(latest.fields,latest.name,getState().config?.revision||'',latest.storedName)!==current)return;
      result=value;observed.set(name,{identity:current,storedName,result:value});
    }catch(error){
      if(token!==sequence)return;
      result={status:'failed',detail:error.name==='AbortError'?'检测已取消':'连通性检测请求失败；草稿和已保存配置保持不变'};observed.set(name,{identity:current,storedName,result});
    }finally{if(token===sequence){controller=null;sync()}}
  }
  function cancel(){reset();result={status:'failed',detail:'检测已取消；远端已消耗的请求不自动重试'};const {name,storedName}=draft();observed.set(name,{identity,storedName,result});sync()}
  // A successful save redacts keys; preserve only a result for the submitted credentials.
  function saved(submitted,stored){
    for(const [name,observation] of observed){
      const sent=profile(submitted,name),next=profile(stored,name);
      if(next&&observation.identity===signature(sent,name,undefined,observation.storedName))observed.set(name,{...observation,storedName:name,identity:signature(next,name,stored.revision||'')});
      else observed.delete(name);
    }
    reset();identity='';
  }
  return {sync,start,cancel,saved};
}

export const providerProbeControls=`<section id="provider-probe-panel" class="provider-probe-panel" data-status="unprobed"><div class="provider-probe-heading"><strong>连接检测</strong><div class="buttons"><button type="button" id="provider-probe-button" data-action="provider-probe">检测连通性</button><button type="button" id="provider-probe-cancel" data-action="provider-probe-cancel" hidden>取消检测</button></div></div><p id="provider-probe-status" class="note" role="status" aria-live="polite">当前配置尚未检测</p><p id="provider-probe-time" hidden></p><p class="provider-probe-note">使用正在编辑的配置发送一次短请求，可能产生少量用量；不会保存草稿或创建任务。不证明工具调用或视觉能力。</p></section>`;
