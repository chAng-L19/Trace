// Explicit account-info checks use saved server credentials, never form drafts.
export function createSearchSourceProbe({api,getState,getElement}){
 const pending=new Map(),results=new Map();
 let generation=0;
 const configIdentity=source=>JSON.stringify(getState().config?.search_sources?.[source]||{});
 const visible=()=>getState().page==='settings'&&getState().settingTab==='provider';
 const labels={authenticated:'账户认证通过',auth_error:'账户认证失败',rate_limited:'请求受限',outcome_unknown:'请求结果待确认',invalid_response:'账户回执格式异常',unavailable:'尚未启用或未配置密钥',upstream_error:'上游请求失败',busy:'已有请求运行',quota_exhausted:'账户额度耗尽'};
 function sync(){for(const source of ['fofa','quake','shodan']){
  const el=getElement('source-'+source+'-probe-status'),button=getElement('source-'+source+'-probe');if(!el||!button)continue;
  const item=results.get(source),match=item?.identity===configIdentity(source),flight=pending.get(source);
  button.disabled=!!flight;
  el.textContent=flight?'正在检查已保存配置…':match?`${labels[item.result.status]||'状态待确认'}${Number.isFinite(item.result.available_credits)?' · 剩余额度 '+item.result.available_credits:''}${item.result.retry_after_seconds?' · '+item.result.retry_after_seconds+' 秒后可重试':''}`:'已保存配置尚未检查';
  el.dataset.status=flight?'probing':match?item.result.status:'unprobed';
 }}
 async function start(source){
  if(!['fofa','quake','shodan'].includes(source)||pending.has(source)||!visible())return;
  const identity=configIdentity(source),version=generation,flight={identity};pending.set(source,flight);sync();
  try{const result=await api('/search-sources/'+source+'/probe','POST');if(version===generation&&identity===configIdentity(source))results.set(source,{identity,result})}
  catch{if(version===generation&&identity===configIdentity(source))results.set(source,{identity,result:{status:'outcome_unknown'}})}
  finally{if(pending.get(source)===flight)pending.delete(source);sync()}
 }
 return {start,sync,invalidate(){generation++;results.clear();sync()}};
}
