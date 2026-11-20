import {esc} from './ui.js';
export {esc} from './ui.js';
import {projectEvent,mergePreview,isPreviewEvent} from './live.js';
import {workerMessageInput} from './workers.js';
import {renderExplorationDetail,updateExplorationSearch,locateExploration,mountExploration,explorationControl,selectExplorationChain} from './exploration.js';
import {evidenceDetail} from './evidence.js';
import {createChainEvents} from './evidence-chain-events.js';
import {evidenceKey,catalogVisible,catalogDetail,runDeleteForm,evidenceDeleteForm} from './evidence-manager.js';
import {readable,rawDetails} from './presentation.js';
import {toolOutput,conversation,conversationPreview,messageCallRecords,inspectionCalls,toolDisclosureBody} from './conversation.js';
import {toggleLocalGroup,catalogTools,localToolGroups,localDetail,localTools,localToolDetail,filterIntegrationCatalog} from './local-tools.js';
import {readRequirements} from './verification.js';
import {renderPage, pageInfo,settingsRoutes,resolveRoute} from './views.js';
import {readConfigForm} from './settings.js';
import {messageDeliveryDetail,runDiagnosticsDetail} from './run-diagnostics.js';
import {measureUI,performanceSample,performanceDiagnostics,uiNow} from './performance.js';
export {performanceDiagnostics} from './performance.js';
import {addModelRow,resetModelRows,syncModelFields} from './model-settings.js';
import {rememberConfigRevision,configRevision,forgetConfigRevision,configHeaders,configConflictMessage,mergeToolSelection} from './config-revision.js';
import {syncReconFields} from './recon-settings.js';
import {createWorkerCapacity} from './worker-capacity.js';
import {createSearchSourceProbe} from './search-source-probe.js';
import {createSettingsLoader} from './settings-loader.js';
import {createMCPStatus} from './mcp-status.js';
import {createProviderProbe} from './provider-probe.js';
import {activeProvider,switchProviderProfile,providerEditorName,providerEditorSelection} from './provider-profiles.js';
import {paintAttachments,chooseAttachments,removeAttachment,uploadAttachments,clearAttachments} from './attachments.js';
import {sourceLinks,integrationActions,readIntegration,integrationResult} from './integrations.js';
import {readTask,readMCP,mcpEditor,recordDetail,budgetHelp,mcpDetail,mcpTools,toolDetail} from './workforms.js';
import {patchLive,patchConversationPreview,redraw,rememberInput,forgetForm,captureWorkerForm,acknowledgeWorkerForm,setField,filterTools,filterServiceTools,mcpTransport,viewScope,formFeedback,locateGraph} from './interactions.js';
import {createRunManagement,readCreationOptions,runLink} from './run-management.js';
import {createDelivery,copyText} from './delivery.js';
import {graphSnapshot,replayEvents} from './history-projection.js';
import {createConversationHistory} from './conversation-history.js';
import {boundedCache,captureForm,acknowledgeForm} from './interactions.js';

export const state={page:'overview',runs:[],selected:sessionStorage.getItem('trace.run')||'',snap:null,tools:[],skills:[],config:null,settingsError:'',settingTab:'provider',historyFilter:'',historyStatus:'',historyDetail:false,eventFilter:'',eventQuery:'',evidenceQuery:'',evidenceStatus:'',evidenceTab:'evidence',taskTab:'activity',graphFilter:'',lastSeq:0};
let source, refreshTimer, noticeTimer, revision=0, navigation=0, detailRevision=0, catalogRevision=0, graphMatchIndex=0, artifactOrigin;
const $=s=>document.querySelector(s);
const settingsLoader=createSettingsLoader({api,state});
const mcpStatus=createMCPStatus({api,getState:()=>state,getElement:id=>document.getElementById(id)});
const providerProbe=createProviderProbe({api,getState:()=>state,getElement:id=>document.getElementById(id),onChange:modelConnection});
const workerCapacity=createWorkerCapacity({api,getRun:()=>state.page==='history'&&state.historyDetail?state.selected:'',getElement:()=>document.querySelector('[data-worker-tree-capacity]')});
const searchSourceProbe=createSearchSourceProbe({api,getState:()=>state,getElement:id=>document.getElementById(id)});
const chainEvents=createChainEvents({state,api,showDetail,render,refresh,notice,captureNotice:scopedNotice,beginDetail:()=>({nav:navigation,detail:++detailRevision,run:state.selected}),isCurrent:token=>token.nav===navigation&&token.detail===detailRevision&&token.run===state.selected});
const runManagement=createRunManagement({state,api,showDetail,refresh,refreshRun,render,notice,captureNotice:scopedNotice});
const delivery=createDelivery({state,showDetail,notice,captureNotice:scopedNotice,token:()=>({nav:navigation,detail:detailRevision,run:state.selected}),isCurrent:t=>t.nav===navigation&&t.detail===detailRevision&&t.run===state.selected&&$('#detail-dialog').open});
const conversationHistory=createConversationHistory({state,api:(...args)=>workerCapacity.foreground(()=>api(...args)),render});
function applyTheme(value){const theme=['paper','white','black'].includes(value)?value:'paper';document.documentElement.dataset.theme=theme;$('#theme-select').value=theme;localStorage.setItem('trace.theme',theme)}
applyTheme(localStorage.getItem('trace.theme'));
export async function api(path,method='GET',body,options={}){
  const response=await fetch('/api'+path,{...options,method,headers:{...(method==='GET'?{}:{'Content-Type':'application/json'}),...options.headers},body:body===undefined?undefined:JSON.stringify(body)});
  const raw=await response.text();let data;try{data=measureUI('http-json-parse',()=>raw?JSON.parse(raw):null)}catch{throw new Error(`工作台请求失败（HTTP ${response.status}）；请检查连接或重新登录`)}if(!response.ok){const error=new Error(data?.error||`工作台请求失败（HTTP ${response.status}）`);error.status=response.status;error.code=data?.code;error.revision=data?.revision;error.message=configConflictMessage(error);throw error}return data;
}
function notice(text,error=false,inlineAllowed=true){if(error&&inlineAllowed&&$('#detail-dialog').open){$('#notice').hidden=true;let inline=$('#dialog-error');if(!inline){inline=document.createElement('p');inline.id='dialog-error';inline.className='inline-error';inline.setAttribute('role','alert');$('#dialog-content').prepend(inline)}inline.textContent=text;return}const el=$('#notice');el.textContent=text;el.className=error?'error':'';el.hidden=false;clearTimeout(noticeTimer);noticeTimer=setTimeout(()=>el.hidden=true,error?15000:6000);}
function scopedNotice(){const nav=navigation,scope=viewScope(state),run=state.selected;return (...args)=>{if(nav===navigation&&scope===viewScope(state)&&run===state.selected)notice(...args)}}
function connection(text,online){$('#connection').textContent=text;$('#connection-dot').classList.toggle('online',online);$('#connection-status')?.setAttribute('data-status',online?'online':text.includes('失败')?'error':'connecting')}
function modelConnection(result={}){
  const el=$('#model-connection');if(!el)return;const {name,provider}=activeProvider(state.config);
  if((result.provider||'default')!==name)result=result.activeResult?.provider===name?result.activeResult:{};
  let status='unprobed',text='模型：读取配置…',detail='模型状态与工作台连接分别显示';
  if(provider){
    const draft=!result.matchesSavedConfig&&['success','failed','responded','probing'].includes(result.status),observation=result.matchesSavedConfig||draft?result:result.savedResult||{},model=draft?result.model:provider.model;
    status=observation.status||(!provider.url?'mock':'unprobed');
    const label=({success:'连通性检测通过',failed:observation.detail?.startsWith('检测已取消')?'检测已取消':'连通性检测失败',responded:'有响应 · 检测未通过',probing:'检测中…',cancelled:'检测已取消',mock:'未配置 · Mock 模式',unprobed:'已配置 · 未检测'})[status]||'已配置 · 未检测';
    text='模型：'+(model?model+' · ':'')+(name==='default'?'':name+' · ')+(draft?'草稿 · ':'')+label+(draft?' · 未保存':'');detail=observation.detail||detail;
  }
  el.textContent=text;el.dataset.status=status;el.title=text+'\n'+detail;
}
function renderRunCount(){
  const running=$('#running-count');if(running){running.textContent=state.runsLoaded===false?'正在读取任务':state.listError?'任务状态待更新':state.runs.filter(r=>r.status==='running').length+' 正在运行'+(state.runs.some(r=>r.status==='waiting_tool')?' · '+state.runs.filter(r=>r.status==='waiting_tool').length+' 等待结果':'');running.dataset.active=String(state.runs.some(r=>r.status==='running'))}
}
function render(force=false){
  const info=state.page==='settings'?{provider:['模型与 API','07 / MODEL CONNECTION'],tools:['MCP管理','08 / MCP'],local:['MCP管理','08 / MCP'],skills:['Skill 管理','09 / SKILLS']}[state.settingTab]:pageInfo[state.page]||pageInfo.overview;$('#page-title').textContent=info[0];$('#section-code').textContent=info[1].split(' / ').slice(1).join(' / ')||info[1];
  renderRunCount();
  document.querySelectorAll('[data-page]').forEach(el=>el.setAttribute('aria-current',el.dataset.page===state.page&&(!el.dataset.tab||el.dataset.tab===state.settingTab||el.dataset.tab==='tools'&&state.settingTab==='local')?'page':'false'));
  const html=measureUI('page-serialize',()=>renderPage(state)),changed=measureUI('page-reconcile',()=>redraw(state,html,force));if(changed!==false){mountExploration();document.querySelectorAll('[data-switch]').forEach(el=>el.checked=el.hasAttribute('checked'));filterIntegrationCatalog();paintAttachments(state);if(state.config){syncReconFields(state.config);syncModelFields(state.config)}}applyPending();providerProbe.sync();
  for(const form of document.querySelectorAll('#config-form,#mcp-form,#integration-form'))form.dataset.configRevision??=state.config?.revision||'';
  workerCapacity.paint();searchSourceProbe.sync();
  $('#event-seq').textContent=state.snap?.run.event_seq??'—';
  renderProviderLabel();
}
function renderProviderLabel(){const {name,provider}=activeProvider(state.config);$('#provider-label').textContent=!state.config?'配置尚未加载':provider?.url?`${provider.model||'MODEL'} / ${name} / OPENAI COMPATIBLE`:'MOCK PROVIDER / 本地验证'}
const pending=new Set();
const serviceViews=boundedCache(32);
function operationKey(el){if(el.id==='message-form')return 'message:'+state.selected;if(['config-form','mcp-form','integration-form'].includes(el.id)||['mcp-edit','mcp-new','mcp-toggle','local-toggle','skill-toggle','discard-config','integration-check','integration-update','provider-profile-switch','provider-profile-edit','provider-profile-new'].includes(el.dataset.action))return 'config-save';return viewScope(state)+':'+state.selected+':'+((el.id?[el.id,el.dataset.run,el.dataset.keys].join(':'):'')||el.dataset.worker||[el.dataset.action,el.dataset.run,el.dataset.id,el.dataset.task,el.dataset.worker,el.dataset.index,el.dataset.hash,el.dataset.offset].join(':'))}
function applyPending(){
  document.querySelectorAll('#page form,#detail-dialog form,#page [data-action],#detail-dialog [data-action]').forEach(el=>{
    const busy=pending.has(operationKey(el));if(busy)el.setAttribute('aria-busy','true');else el.removeAttribute('aria-busy');
    const controls=el.tagName==='FORM'?el.querySelectorAll('input,textarea,select,button'):[el];
    for(const control of controls){if(busy){control.dataset.pendingDisabled??=String(control.disabled);control.disabled=true}else if(control.dataset.pendingDisabled!==undefined&&!control.closest('form[aria-busy="true"]')){control.disabled=control.dataset.pendingDisabled==='true';delete control.dataset.pendingDisabled}}
  });
}
async function saveConfig(next,quiet=false,options={}){const notice=scopedNotice();const result=await api('/config','PUT',next,{headers:configHeaders(options.revision??state.config?.revision)});searchSourceProbe.invalidate();providerProbe.saved(next,result.config);settingsLoader.saved(result.config,{...options,modelOnly:!!result.model_only});await loadSettings();if(!quiet)notice(result.cleanup_error?`配置已保存；旧连接清理异常：${result.cleanup_error}`:state.settingsError?'配置已保存；本页资源刷新失败，请重试':result.model_only?'模型配置已保存；新运行使用当前默认，已有运行保留模型绑定，后续恢复读取该连接最新密钥。':'配置已保存；相关工具目录与连接状态已更新',!!result.cleanup_error||!!state.settingsError);return result}

const dirtyRuns=new Set(),runLoads=new Map(),runViews=boundedCache(32),requestDetails=boundedCache(24,4*1024*1024),requestLoads=new Map(),runControllers=new Map();
function cancelRunReads(){workerCapacity.reset();delivery.close();conversationHistory.cancel();for(const controller of runControllers.values())controller.abort();runControllers.clear();runLoads.clear();for(const flight of requestLoads.values())flight.controller.abort();requestLoads.clear();dirtyRuns.clear();state.workspaceReadAt=undefined}
let listLoad,paintFrame,listDirty=false;
function paintSoon(){if(!paintFrame)paintFrame=setTimeout(()=>{paintFrame=null;if(state.page!=='history'||!state.historyDetail||state.taskTab!=='activity'||!state.snap)return;const element=document.querySelector('.conversation'),start=uiNow(),patch=measureUI('stream-serialize',()=>conversationPreview(state.snap,{lazy:true}));measureUI('stream-reconcile',()=>{if(!patchConversationPreview(element,patch))patchLive(element,conversation(state.snap,{lazy:true}))});performanceSample('stream-paint',start,{sequence:state.snap.run.event_seq});const seq=$('#event-seq');if(seq)seq.textContent=state.snap.run.event_seq},50)}
async function refreshList(invalidate=false){
  if(!listLoad||invalidate)listDirty=true;
  if(listLoad)return listLoad;
  // The mounted task reads its workspace, not list summaries. Worker lifecycle
  // list updates only change the shell counter; do not rebuild its transcript.
  const paint=()=>state.page==='history'&&state.historyDetail?renderRunCount():render();
  listLoad=(async()=>{while(listDirty){listDirty=false;try{const all=await api('/runs?include_archived=true&include_deleted=true');if(listDirty)continue;state.runs=all.filter(r=>!r.deleted);state.hiddenRuns=all.filter(r=>r.deleted);state.runsLoaded=true;state.listError='';if(!state.historyDetail)connection('工作台已连接',true);paint()}catch(error){if(listDirty)continue;state.listError=error.message;state.runsLoaded=true;paint();throw error}}})().finally(()=>listLoad=null);
  return listLoad;
}
async function refreshRun(id=state.selected){
  if(!id)return;
  dirtyRuns.add(id);
  if(runLoads.has(id))return runLoads.get(id);
  const work=(async()=>{
    while(dirtyRuns.delete(id)){
      const rev=revision,tab=state.taskTab||'activity',controller=new AbortController();runControllers.set(id,controller);let snap;
      try{snap=await workerCapacity.foreground(()=>api('/runs/'+id+'?view=workspace&tab='+encodeURIComponent(tab)+(tab==='activity'?'&activity=true'+(state.workerActivityExpanded?'':'&workers=false'):''),'GET',undefined,{signal:controller.signal}))}catch(error){if(state.selected!==id||rev!==revision)return;if(controller.signal.aborted){if(tab!==(state.taskTab||'activity')){dirtyRuns.add(id);continue}return}throw error}finally{if(runControllers.get(id)===controller)runControllers.delete(id)}
      if(state.selected!==id||rev!==revision)return;
      if(tab!==(state.taskTab||'activity')){dirtyRuns.add(id);continue}
      if(controller.signal.aborted)return;
      snap.childPreviews=state.snap?.childPreviews;state.snap=conversationHistory.refresh(mergePreview(snap,state.snap),state.snap);state.runLoading=false;state.runError='';state.workspaceReadAt=uiNow();
      // Subscribe from the first accepted committed watermark; dirty follow-up
      // reads must not hold the live connection behind another workspace GET.
      if(state.historyDetail&&!source)subscribe();render();
    }
  })().catch(error=>{if(runLoads.get(id)===work&&state.selected===id){state.runLoading=false;state.runError=error.message;render()}throw error}).finally(()=>{if(runLoads.get(id)===work)runLoads.delete(id)});
  runLoads.set(id,work);return work;
}
async function refresh(){const notice=scopedNotice();
  try{await Promise.all([refreshList(true),state.historyDetail&&state.selected?refreshRun():null,state.page==='evidence'||state.historyDetail&&state.taskTab==='evidence'?loadCatalog(true):null]);render();connection('工作台已连接',true)}catch(error){notice(error.message,true,false)}
}
let catalogLoad,catalogDirty=false;
async function loadCatalog(invalidate=false){
  if(state.catalogMode!=='records')return chainEvents.load(invalidate);
  if(state.page==='history')return;
  if(catalogLoad){if(invalidate)catalogDirty=true;return catalogLoad}
  const request=++catalogRevision;catalogDirty=true;
  catalogLoad=(async()=>{while(catalogDirty){catalogDirty=false;try{const catalog=await api('/evidence');if(catalogDirty)continue;if(request!==catalogRevision)return;state.catalog=catalog;state.catalogScope='';state.catalogError='';const keys=new Set(state.catalog.map(evidenceKey));state.catalogSelected=(state.catalogSelected||[]).filter(key=>keys.has(key))}catch(error){if(catalogDirty)continue;if(request===catalogRevision)state.catalogError=error.message}}})().finally(()=>catalogLoad=null);return catalogLoad;
}
function forgetDeletedRun(id){if(state.selected===id)cancelRunReads();runViews.delete(id);dirtyRuns.delete(id);for(const key of requestDetails.keys())if(key.startsWith(id+':'))requestDetails.delete(key);if(state.selected!==id)return;revision++;source?.close();source=null;clearTimeout(refreshTimer);refreshTimer=null;state.eventHistory=null;state.eventHistoryRequest=(state.eventHistoryRequest||0)+1;state.selected='';state.snap=null;state.historyDetail=false;sessionStorage.removeItem('trace.run')}
const btnCatalogBack=entry=>`<button type="button" class="small" data-action="catalog-detail" data-run="${esc(entry.run_id)}" data-id="${esc(entry.evidence.id)}">← 返回证据详情</button>`;
function subscribe(){
  source?.close();source=null;clearTimeout(refreshTimer);refreshTimer=null;state.lastSeq=state.snap?.run.event_seq||0;
  if(!state.selected||!state.historyDetail)return;
  const selected=state.selected;
  const subscription=source=new EventSource(`/api/runs/${selected}/events?after=${state.lastSeq}`);
  workerCapacity.refresh();
  source.onopen=()=>{if(source===subscription)connection('工作台 · 实时订阅',true)};
  source.onerror=()=>{if(source===subscription){state.workspaceReadAt=undefined;connection('工作台重连中',false)}};
  source.addEventListener('trace',event=>{
    if(source!==subscription||state.selected!==selected||!state.historyDetail)return;
    const received=uiNow();let item;try{item=measureUI('sse-parse',()=>JSON.parse(event.data))}catch{connection('实时帧解析失败，正在核对',false);refreshRun(selected).catch(()=>{});return}
    if(item.sequence<=state.lastSeq)return;state.lastSeq=item.sequence;
    // A workspace read can overtake SSE delivery. Its committed watermark
    // already covers replayed state events; do not refetch or repaint them.
    if(!measureUI('sse-project',()=>projectEvent(state.snap,item)))return;performanceSample('sse-receive',received,{sequence:item.sequence,bytes:event.data.length});paintSoon();
    if(!isPreviewEvent(item)){
      dirtyRuns.add(selected);
      if(!refreshTimer)refreshTimer=setTimeout(()=>{refreshTimer=null;if(dirtyRuns.has(selected))refreshRun(selected).catch(()=>{})},180);
      if(/^run_|^worker_(started|finished)$/.test(item.type))refreshList(true).catch(()=>{});
      if(/^worker_(started|finished|resumed|cancel_requested|yielded|interrupted|lease_expired|capacity_updated)$|^task_(claimed|completed|failed|released)$|^plan_/.test(item.type))workerCapacity.refresh();
    }
  });
}
const runViewFields=['taskTab','graphFilter','eventFilter','eventQuery','evidenceQuery','evidenceStatus','evidenceRelation','messageCursor'];
async function selectRun(id,page=state.page,{replace=false,deepLink,force=false}={}){
  if(!force&&!deepLink&&id===state.selected&&page===state.page&&state.historyDetail&&state.snap&&!state.runLoading)return;
  // Plain hash/back routes carry empty parsed link fields. They must restore
  // the saved cursor, unlike an actual tab/message/event locator. Normalize
  // after the no-op guard: hashchange already closed the old SSE connection.
  if(deepLink&&!Object.values(deepLink).some(Boolean))deepLink=undefined;
  if(state.selected&&state.snap)runViews.set(state.selected,Object.fromEntries(runViewFields.map(key=>[key,state[key]])));
  cancelRunReads();const rev=++revision,nav=++navigation;source?.close();source=null;clearTimeout(refreshTimer);refreshTimer=null;
  state.selected=id;state.snap=null;state.replaySnapshot=null;state.replaySequence=undefined;state.replayLoading=false;state.replayError='';state.usageSummary=null;state.usageError='';state.deepLink=deepLink;state.page=page;state.historyDetail=true;state.runLoading=true;state.runError='';
  Object.assign(state,{taskTab:'activity',graphFilter:'',eventFilter:'',eventQuery:'',evidenceQuery:'',evidenceStatus:'',evidenceRelation:'report',messageCursor:null,workerActivityExpanded:false},runViews.get(id)||{});
  if(deepLink?.tab)state.taskTab=deepLink.tab;if(deepLink?.message||deepLink?.call||deepLink?.request)state.taskTab='activity';if(deepLink?.event)state.taskTab='events';
  state.eventHistory=null;state.eventHistoryRequest=(state.eventHistoryRequest||0)+1;sessionStorage.setItem('trace.run',id);
  const hash=page==='history'?runLink(id,deepLink):'#'+(page==='settings'?settingsRoutes[state.settingTab]:page);if(location.hash!==hash)history[replace?'replaceState':'pushState'](null,'',hash);
  render();
  try{await refreshRun(id);if(rev!==revision||nav!==navigation)return;if(!source)subscribe();if(!deepLink&&state.messageCursor&&state.taskTab==='activity')await conversationHistory.load(state.messageCursor);if(rev!==revision||nav!==navigation)return;if(state.taskTab==='events'&&!deepLink?.event)await loadEventHistory();if(state.taskTab==='evidence'){await loadCatalog();if(rev!==revision||nav!==navigation)return;render()}if(rev!==revision||nav!==navigation)return;await revealDeepLink();runManagement.reconcile(id).then(ack=>{if(ack&&state.selected===id&&rev===revision&&nav===navigation){state.messageAck={run:id,text:'上次提交已核对：'+ack.status};render()}}).catch(()=>{})}
  catch(error){if(rev===revision){state.runLoading=false;state.runError=error.message;render()}}
}
async function loadEventHistory(before){
  const id=state.selected,request=state.eventHistoryRequest=(state.eventHistoryRequest||0)+1;
  state.eventsLoading=true;state.eventsError='';render();
  const params=new URLSearchParams({limit:'100'});if(before)params.set('before',before);if(state.eventFilter)params.set('type',state.eventFilter);if(state.eventQuery)params.set('query',state.eventQuery);if(!state.eventsRaw)params.set('summary','true');
  try{const page=await api(`/runs/${id}/events/history?${params}`);if(id!==state.selected||request!==state.eventHistoryRequest)return;state.eventHistory=page;state.eventsLoading=false;render()}
  catch(error){if(id===state.selected&&request===state.eventHistoryRequest){state.eventsLoading=false;state.eventsError=error.message;render()}}
}

function showDetail(title,content,preserveMedia=false){if(!preserveMedia)delivery.close();mcpStatus.close();detailRevision++;$('#dialog-title').textContent=title;$('#dialog-content').innerHTML=content;for(const form of $('#dialog-content').querySelectorAll?.('#mcp-form,#integration-form')||[])form.dataset.configRevision=state.config?.revision||'';if(!$('#detail-dialog').open)$('#detail-dialog').showModal();applyPending();const panel=$('#mcp-connection-status');if(panel)mcpStatus.open(panel.dataset.name)}
async function copyPath(path){return copyText(path)}
async function revealDeepLink(){
  const link=state.deepLink,id=state.selected,rev=revision,nav=navigation,detail=detailRevision;if(!link)return;
  let target;
  if(link.chain){await chainEvents.action({dataset:{action:'chain-detail',run:link.chain_run||id,id:link.chain,hash:link.chain_hash}});return}
  if(link.message){
    let message=(state.snap.messages||[]).find(m=>m.id===link.message);
    if(!message){await conversationHistory.load({around:link.message},'start');if(id!==state.selected||rev!==revision||nav!==navigation||detail!==detailRevision)return;message=state.snap.messages?.find(m=>m.id===link.message)}
    if(!message)throw new Error('定位消息不在当前账本中');
    if(message.role==='tool'&&message.tool_call_id){const record=messageCallRecords(state.snap).find(r=>r.reply===message||r.message===message);await dispatch('action-detail',{dataset:{id:message.tool_call_id,request:record?.action.request_id||message.request_id}});return}
    target=[...document.querySelectorAll('[data-message-id]')].find(el=>el.dataset.messageId===link.message);
  }
  if((link.call||link.request)&&!link.message){try{await conversationHistory.load({...link.request?{request:link.request}:{},...link.call?{call:link.call}:{}},'start')}catch(error){if(error.status!==404)throw error}if(id!==state.selected||rev!==revision||nav!==navigation)return;await dispatch(link.call?'action-detail':'request-detail',{dataset:{id:link.call,request:link.request}});return}
  if(link.event){
    const sequence=Number(link.event);if(!Number.isSafeInteger(sequence)||sequence<1){await loadEventHistory();return}
    state.eventsRaw=true;state.eventFilter='';state.eventQuery='';await loadEventHistory(sequence+1);if(id!==state.selected||rev!==revision||nav!==navigation)return;target=document.getElementById('event-'+sequence);if(target)target.open=true;
  }
  if(target){const pane=target.closest?.('[data-scroll]');if(pane){pane.scrollTop+=target.getBoundingClientRect().top-pane.getBoundingClientRect().top-(pane.clientHeight-target.getBoundingClientRect().height)/2}else target.scrollIntoView({block:'center',inline:'nearest'});target.setAttribute('tabindex','-1');target.focus({preventScroll:true})}
  else if(link.message||link.request||link.call||link.event)notice('此定位记录不在当前视图中；原身份保留在地址栏，可从原始事件核对');
}
async function mutate(path,body={},method='POST'){const result=await api(path,method,body);await refresh();return result}
async function dispatch(action,el){const notice=scopedNotice();
  if(action==='provider-profile-edit'){state.providerEditor=el.dataset.editor;render(true);return}
  if(action==='provider-profile-new'){rememberConfigRevision(state,$('#config-form'));state.providerEditor=String(Object.keys(state.config.providers||{}).length+addModelRow('providers'));render(true);$('#model-provider-'+state.providerEditor+'-name')?.focus();return}
  if(action==='provider-profile-switch'){const current={...state},form=el.closest('#config-form'),next=switchProviderProfile(state.config,el.dataset.provider,form);await saveConfig(next,false,{revision:configRevision(current,form)});forgetForm(current,form);forgetConfigRevision(current,form);formFeedback(current,form,`已将「${el.dataset.provider}」设为默认；新运行使用此配置。`);return}
  if(action==='messages-page'){await conversationHistory.load(el.dataset.before?{before:el.dataset.before}:{after:el.dataset.after},el.dataset.before?'end':'start');return}
  if(action==='messages-latest'){await conversationHistory.load({},'end');return}
  if(action==='message-detail'){const id=state.selected,rev=revision,nav=navigation,token=++detailRevision,message=await api(`/runs/${id}/messages/${encodeURIComponent(el.dataset.message)}`);if(id===state.selected&&rev===revision&&nav===navigation&&token===detailRevision)showDetail('完整消息',`${readable(message.content)}${rawDetails('message-full',message,'完整账本记录')}`);return}
  if(['tool-detail','mcp-detail','mcp-tools','mcp-toggle','mcp-edit','mcp-new','skill-toggle','local-toggle','local-detail','local-tools','local-tool-detail'].includes(action)&&['config','tools',...(['tool-detail','mcp-detail','mcp-tools','mcp-toggle','mcp-edit','mcp-new'].includes(action)?['mcpStatuses']:[])].some(field=>state.settingsResources?.[field]?.loading))throw new Error('工具目录正在更新，请稍后重试');
  if(['mcp-detail','mcp-tools','mcp-toggle','mcp-edit','tool-detail'].includes(action)&&el.dataset.service!==undefined){const index=(state.config.mcp||[]).findIndex(m=>m.name===el.dataset.service);if(index<0)throw new Error('此服务已更新，请返回目录');if(action==='tool-detail')el.dataset.mcpIndex=String(index);else el.dataset.index=String(index)}
  if(action==='usage-refresh'){const id=state.selected,rev=revision,nav=navigation;try{const summary=await api(`/runs/${id}/usage`);if(id===state.selected&&rev===revision&&nav===navigation){state.usageSummary=summary;state.usageError=''}}catch(error){if(id===state.selected&&rev===revision&&nav===navigation)state.usageError=error.message;throw error}return}
  if(action==='replay-open'){const id=state.selected,rev=revision,nav=navigation;state.replayLoading=true;state.replayError='';render();try{const snap=await api('/runs/'+id);if(id===state.selected&&rev===revision&&nav===navigation){state.replaySnapshot=snap;state.replaySequence=replayEvents(snap).at(-1)?.sequence||0}}catch(error){if(id===state.selected&&rev===revision&&nav===navigation)state.replayError=error.message;throw error}finally{if(id===state.selected&&rev===revision&&nav===navigation)state.replayLoading=false}return}
  if(action==='replay-live'){state.replaySnapshot=null;state.replaySequence=undefined;return}
  if(action==='replay-step'){const events=replayEvents(state.replaySnapshot),index=events.findIndex(e=>e.sequence===state.replaySequence)+Number(el.dataset.step);if(events[index])state.replaySequence=events[index].sequence;return}
  if(action==='remove-input'){removeAttachment(state,el);return}
  const id=state.selected;
  if(action==='archive-back'){cancelRunReads();navigation++;source?.close();source=null;clearTimeout(refreshTimer);refreshTimer=null;state.historyDetail=false;state.page='history';history.pushState(null,'','#history');render();return}
  if(action==='archive-run'||action==='archive-pause'){const target=state.runs.find(r=>r.id===el.dataset.id);if(!target)throw new Error('会话已更新，请刷新后重试');await api('/runs/'+target.id+'/'+(action==='archive-pause'?'pause':target.status==='created'?'run':'resume'),'POST',{});await refresh();notice(action==='archive-pause'?'暂停请求已提交':'会话已开始运行');return}
  if(action==='archive-hidden'){state.historyHidden=!state.historyHidden;state.historyArchived=false;state.historyDetail=false;render();return}
  if(action==='archive-delete'){const target=[...state.runs,...(state.hiddenRuns||[])].find(r=>r.id===el.dataset.id);if(!target)return;showDetail('删除会话',runDeleteForm(target,state.snap?.run.id===target.id?state.snap.evidence?.length:undefined));return}
  if(action==='model-row-add'){rememberConfigRevision(state,$('#config-form'));addModelRow(el.dataset.group);render(true);return}
  if(action==='dialog-cancel'){$('#detail-dialog').close();return}
  if(action==='message-deliveries'||action==='run-diagnostics'){const target=el.dataset.run||state.selected,request=++detailRevision,nav=navigation,path=action==='message-deliveries'?'message-deliveries':'diagnostics',params=new URLSearchParams({limit:'40'});if(el.dataset.before)params.set('before',el.dataset.before);const data=await api('/runs/'+encodeURIComponent(target)+'/'+path+'?'+params);if(request!==detailRevision||nav!==navigation||state.selected!==target)return;showDetail(action==='message-deliveries'?'消息投递与归档回执':'请求身份、分段用时与费用',action==='message-deliveries'?messageDeliveryDetail(data,target):runDiagnosticsDetail(data));return}
  if(action.startsWith('chain-')||action==='catalog-mode'){await chainEvents.action(el);return}
  if(action==='catalog-refresh'){await loadCatalog();render();return}
  if(action==='catalog-open-run'){$('#detail-dialog').close();await selectRun(el.dataset.id,'history');return}
  if(action==='catalog-detail'||action==='catalog-action'){
    const nav=navigation,request=++detailRevision,entry=await api('/evidence/'+encodeURIComponent(el.dataset.run)+'/'+encodeURIComponent(el.dataset.id));if(nav!==navigation||request!==detailRevision)return;
    if(action==='catalog-detail')showDetail('证据详情',catalogDetail(entry));
    else if(entry.action)showDetail('来源调用',`<div class="buttons">${btnCatalogBack(entry)}</div><h4>输入</h4>${readable(entry.action.arguments||'{}')}<h4>输出</h4>${toolOutput(entry.action.result)}${rawDetails('catalog-action-raw',entry.action,'完整调用记录')}`);
    else throw new Error('此证据没有对应调用记录');return;
  }
  if(action==='catalog-delete'||action==='catalog-delete-selected'){
    const nav=navigation,request=++detailRevision;const keys=new Set(state.catalogSelected||[]);const entries=action==='catalog-delete'?[await api('/evidence/'+encodeURIComponent(el.dataset.run)+'/'+encodeURIComponent(el.dataset.id))]:(state.catalog||[]).filter(entry=>keys.has(evidenceKey(entry)));if(nav!==navigation||request!==detailRevision)return;if(!entries.length)throw new Error('记录已更新，请刷新后重新选择');showDetail('删除关联证据',evidenceDeleteForm(entries));return;
  }
  if(action==='exploration-detail'){selectExplorationChain(el);showDetail('探索节点',renderExplorationDetail(el.dataset.current==='true'||(state.taskTab||'activity')==='activity'?state.snap:graphSnapshot(state),el.dataset.id));return}
  if(action==='exploration-chain'){selectExplorationChain(el);return}
  if(action==='exploration-control'){explorationControl(el);return}
  if(action==='exploration-locate'){if(!locateExploration())notice('没有匹配节点；输入目标、方向、观测内容或 ID');return}
  if(action==='integration-catalog'){$('#detail-dialog').close();return}
  if(action==='local-detail'||action==='local-tools'){const index=Number(el.dataset.index),group=localToolGroups(state.config,state.tools)[index];if(!group)throw new Error('工具目录已更新，请重新选择');const filter=$('#service-tool-filter');if(filter&&$('.service-tools'))serviceViews.set('local:'+group.key,{query:filter.value,top:$('.service-tools').scrollTop});showDetail(group.name,action==='local-detail'?localDetail(state.config,state.tools,index):localTools(state.config,state.tools,index));if(action==='local-tools'){const view=serviceViews.get('local:'+group.key),input=$('#service-tool-filter');if(view&&input){input.value=view.query;filterServiceTools();$('.service-tools').scrollTop=view.top}}return}
  if(action==='local-tool-detail'){const index=Number(el.dataset.index),group=localToolGroups(state.config,state.tools)[index],filter=$('#service-tool-filter');if(filter&&$('.service-tools'))serviceViews.set('local:'+group.key,{query:filter.value,top:$('.service-tools').scrollTop});showDetail(state.config.local_tools[Number(el.dataset.manifestIndex)].name,localToolDetail(state.config,state.tools,index,Number(el.dataset.manifestIndex)));return}
  if(action==='finding-review'){await mutate(`/runs/${id}/findings/${encodeURIComponent(el.dataset.id)}/review`);notice('独立审阅已记录');return}
  if(action==='finding-triage'){await mutate(`/runs/${id}/findings/${encodeURIComponent(el.dataset.id)}/triage`,{triage:'fixed'});notice('处置状态已记录；是否修复以复测证据为准');return}
  if(action==='mcp-status-refresh')return mcpStatus.refresh();
  if(action==='mcp-probe')return mcpStatus.start();
  if(action==='mcp-probe-cancel'){mcpStatus.cancel();return}
  if(action==='provider-probe')return providerProbe.start();
  if(action==='search-source-probe')return searchSourceProbe.start(el.dataset.source);
  if(action==='provider-probe-cancel'){providerProbe.cancel();return}
  if(action==='open-evidence'){conversationHistory.cancel();state.evidenceTab=el.dataset.tab||'evidence';state.taskTab=state.evidenceTab;state.historyDetail=true;state.page='history';navigation++;history.replaceState(null,'','#history/'+encodeURIComponent(state.selected));render();await refreshRun();if(state.taskTab==='evidence'){await loadCatalog();render()}return}
  if(action==='task-tab'){conversationHistory.cancel();const tab=el.dataset.tab||'activity';if(tab!==(state.taskTab||'activity'))runControllers.get(state.selected)?.abort();state.taskTab=tab;state.deepLink=null;history.replaceState(null,'',runLink(state.selected,{tab:state.taskTab}));render();await refreshRun();if(state.taskTab==='evidence'){await loadCatalog();render()}if(state.taskTab==='events')await loadEventHistory();return}
  if(action==='run-retry'){await selectRun(id,'history',{replace:true,force:true});return}
  if(action==='events-page'){await loadEventHistory(el.dataset.before);return}
  if(action==='events-mode'){state.eventsRaw=!state.eventsRaw;if(!state.eventsRaw&&['model_stream','tool_output','task_claimed'].includes(state.eventFilter))state.eventFilter='';await loadEventHistory();return}
  if(action==='child-preview'){const target=el.dataset.id;state.snap.childPreviews??={};if(state.snap.childPreviews[target]){delete state.snap.childPreviews[target];return}const parent=id,rev=revision,nav=navigation,child=await api('/runs/'+encodeURIComponent(target)+'?view=workspace');if(state.selected===parent&&rev===revision&&nav===navigation&&state.snap){state.snap.childPreviews??={};if(child.run.parent_run_id!==parent)throw new Error('子运行来源与当前运行不一致');state.snap.childPreviews[target]=child}return}
  if(action==='request-detail'){
    const request=el.dataset.request,key=id+':'+request,requestRev=revision,nav=navigation,token=++detailRevision;
    let detail=requestDetails.get(key);
    if(!detail){
      if(!requestLoads.has(key)){
        while(requestLoads.size>=4){const oldest=requestLoads.keys().next().value;requestLoads.get(oldest).controller.abort();requestLoads.delete(oldest)}
        const flight={controller:new AbortController()};
        flight.promise=api(`/runs/${id}/requests/${encodeURIComponent(request)}`,'GET',undefined,{signal:flight.controller.signal}).finally(()=>{if(requestLoads.get(key)===flight)requestLoads.delete(key)});requestLoads.set(key,flight);
      }
      const flight=requestLoads.get(key),full=await flight.promise;if(flight.controller.signal.aborted||requestRev!==revision||state.selected!==id||nav!==navigation)return;detail=full.request||full;requestDetails.set(key,detail);
    }
    if(requestRev!==revision||state.selected!==id||nav!==navigation||token!==detailRevision)return;
    showDetail('模型请求 / 工具 schema',`${rawDetails('request-body',detail,'完整模型请求与 schema')}${rawDetails('request-response',(state.snap.responses||[]).find(r=>r.request_id===request),'模型响应记录')}`);return;
  }
  if(action==='latest'){if(el.dataset.scrollTarget==='conversation'&&(state.snap?.historyWindow||state.snap?.messages_page?.has_more_after)){await conversationHistory.load({},'end');return}const pane=document.querySelector(`[data-scroll="${el.dataset.scrollTarget}"]`);if(pane)pane.scrollTop=pane.scrollHeight;return}
  if(action==='artifact-back'&&artifactOrigin){showDetail(artifactOrigin.title,artifactOrigin.content);artifactOrigin=null;return}
  if(action==='artifact'&&!el.dataset.offset)artifactOrigin=$('#detail-dialog').open?{title:$('#dialog-title').textContent,content:$('#dialog-content').innerHTML}:null;
  if(action==='jump-records'){const section=document.getElementById(el.dataset.target);section?.scrollIntoView({block:'start'});section?.focus({preventScroll:true});return}
  if(action==='budget-help'){showDetail('预算与成功条件',budgetHelp(state.snap.run));return}
  if(action==='graph-locate'){const matches=locateGraph();if(!matches.length){notice('没有匹配节点；检查搜索文本或覆盖图状态过滤');return}const node=matches[graphMatchIndex++%matches.length];for(let parent=node.parentElement;parent;parent=parent.parentElement)if(parent.tagName==='DETAILS')parent.open=true;node.scrollIntoView({block:'center',inline:'center'});node.focus({preventScroll:true});return}
  if(action==='continue'){if(state.page==='history'&&state.taskTab!=='activity'){state.taskTab='activity';render()}$('#message-content').focus();return}
  if(action==='select'){$('#detail-dialog').close();return selectRun(el.dataset.id,'history')}
  if(action==='run'||action==='resume'||action==='pause'||action==='cancel'){await mutate(`/runs/${id}/${action}`);notice(action==='cancel'?'取消请求已持久保存':'运行控制请求已提交');return}
  if(action==='branch'){const messageID=el.dataset.message||state.snap.messages?.at(-1)?.id;if(!messageID)throw new Error('当前运行尚无可分支消息。');const snap=await api(`/runs/${id}/branch`,'POST',{message_id:messageID});await refresh();await selectRun(snap.run.id,'history');notice('分支已创建，运行时将继续所选历史');return}
  if(action==='setting-tab'){state.settingTab=el.dataset.tab;navigation++;history.replaceState(null,'',`#${settingsRoutes[state.settingTab]}`);render();await loadSettings();render();return}
  if(action==='evidence-tab'){state.evidenceTab=el.dataset.tab||'evidence';render();return}
  if(action==='local-toggle'){const saved=await saveConfig(toggleLocalGroup(state.config,el.dataset.group),true);state.extensionFeedback={message:saved?.cleanup_error?'状态已保存；旧连接清理异常：'+saved.cleanup_error:state.settingsError?'项目状态已保存；目录刷新失败，请重试':'项目启用状态已保存',error:!!saved?.cleanup_error||!!state.settingsError};return}
  if(action==='clear-tools'){const ids=mergeToolSelection(state.snap?.run.tool_selection?.ids,state.tools,catalogTools(state.config,state.tools).map(t=>t.definition.function.name),[]);await mutate(`/runs/${id}/tools`,{ids,replace:true},'PUT');notice('本页接口选择已清空；其他来源选择保留');return}
  if(action==='mcp-detail'||action==='mcp-tools'){const current=$('#service-tool-filter'),service=$('.service-tools');if(current&&service)serviceViews.set(service.dataset.name,{query:current.value,top:service.scrollTop});const index=Number(el.dataset.index),m=state.config.mcp[index];showDetail(m.name,action==='mcp-detail'?mcpDetail(state.config,state.tools,index,state.mcpStatuses):mcpTools(state.config,state.tools,index));if(action==='mcp-tools'){const view=serviceViews.get(m.name),input=$('#service-tool-filter');if(view&&input){input.value=view.query;filterServiceTools();$('.service-tools').scrollTop=view.top}}return}
  if(action==='skill-toggle'){const saved=await api('/skills/'+encodeURIComponent(el.dataset.name)+'/availability','PUT',{disabled:el.dataset.disabled!=='true'},{headers:configHeaders(state.config?.revision)});settingsLoader.saved(saved.config,{skills:true});await loadSettings();state.extensionFeedback={message:saved?.cleanup_error?'状态已保存；旧连接清理异常：'+saved?.cleanup_error:state.settingsError?'Skill 状态已保存；目录刷新失败，请重试':'Skill 模型可用状态已保存',error:!!saved?.cleanup_error||!!state.settingsError};return}
  if(action==='mcp-toggle'){const index=Number(el.dataset.index),request=++detailRevision,nav=navigation,name=state.config.mcp[index].name,next=structuredClone(state.config);next.mcp[index].disabled=!next.mcp[index].disabled;const saved=await saveConfig(next,el.dataset.switch!==undefined);state.extensionFeedback={message:saved?.cleanup_error?'状态已保存；旧连接清理异常：'+saved?.cleanup_error:state.settingsError?'服务状态已保存；目录刷新失败，请重试':'服务启用状态已保存',error:!!saved?.cleanup_error||!!state.settingsError};if(request!==detailRevision||nav!==navigation||!$('#detail-dialog').open||$('#dialog-title').textContent!==name)return;if(state.settingsResources.tools.available===false){$('#detail-dialog').close();return}showDetail(name,mcpDetail(state.config,state.tools,index,state.mcpStatuses));return}
  if(action==='tool-detail'){const tool=el.dataset.toolName!==undefined?state.tools.find(t=>t.definition.function.name===el.dataset.toolName):state.tools[Number(el.dataset.index)];if(!tool)throw new Error('此工具已更新，请返回目录');const input=$('#service-tool-filter');if(input&&el.dataset.mcpIndex!==undefined)serviceViews.set(state.config.mcp[Number(el.dataset.mcpIndex)].name,{query:input.value,top:$('.service-tools').scrollTop});showDetail(tool.local_name||tool.remote_name||tool.definition.function.name,toolDetail(tool,el.dataset.mcpIndex!==undefined?`data-index="${esc(el.dataset.mcpIndex)}" data-service="${esc(state.config.mcp[Number(el.dataset.mcpIndex)].name)}"`:""));return}
  if(action==='skill-detail'){const request=++detailRevision,nav=navigation,skill=await api('/skills/'+encodeURIComponent(el.dataset.name)),meta=state.skills.find(t=>t.name===el.dataset.name)||{};if(request!==detailRevision||nav!==navigation)return;showDetail(el.dataset.name,`${sourceLinks(meta)}${integrationActions(meta,'skill',meta.name)}<pre>${esc(skill.content)}</pre>`);return}
  if(action==='copy-path'){await copyPath(el.dataset.path);notice('服务器路径已复制');return}
  if(action==='integration-check'||action==='integration-update'){const body={id:el.dataset.id||undefined,kind:el.dataset.kind,name:el.dataset.name};await runIntegration(action==='integration-check'?'check':'update',body);return}
  if(action==='evidence-detail'){const ev=(state.snap.evidence||[]).find(e=>e.id===el.dataset.id);if(!ev)throw new Error('本运行中未找到此证据，请从任务的证据运行进入。');showDetail('证据详情',evidenceDetail(ev,state.snap));return}
  if(['asset-detail','action-detail','task-detail'].includes(action)){const [list,key]=action==='asset-detail'?['assets','id']:action==='task-detail'?['plan','id']:['actions','call_id'];const matches=(list==='plan'?state.snap.plan?.tasks:action==='action-detail'?inspectionCalls(state.snap):state.snap[list])?.filter(e=>e[key]===el.dataset.id&&(action!=='action-detail'||el.dataset.request===undefined||(e.request_id||'')===el.dataset.request))||[];if(matches.length!==1)throw new Error(matches.length?'此调用 ID 在多个请求中复用，请从对应工具记录打开详情。':'记录已更新，请重新选择。');let item=matches[0];if(action==='action-detail'&&state.snap.messages_page&&((state.snap.actions||[]).some(a=>a.request_id===item.request_id&&a.call_id===item.call_id)||item.source_message)){const rev=revision,nav=navigation,token=++detailRevision;item=await api('/runs/'+id+'/actions/'+encodeURIComponent(item.call_id)+'?request='+encodeURIComponent(item.request_id||''));if(rev!==revision||nav!==navigation||token!==detailRevision||id!==state.selected)return}const detailSnap=action==='action-detail'?{...state.snap,evidence:(state.snap.evidence||[]).filter(e=>e.call_id!==item.call_id||(e.request_id?e.request_id===item.request_id:(state.snap.actions||[]).filter(a=>a.call_id===item.call_id).length===1))}:state.snap;showDetail(el.dataset.id,recordDetail(action,item,detailSnap));return}
  if(action==='artifact'){const previous=el.dataset.offset?$('#dialog-content').innerHTML:'',previousTitle=$('#dialog-title').textContent;showDetail('原始产物',previous||`${artifactOrigin?'<button class="small" data-action="artifact-back">← 返回来源记录</button>':''}<p class="note" role="status">正在读取产物…</p><a href="/api/artifacts/${esc(el.dataset.hash)}/download" download>下载原始产物 ↓</a>`);const request=detailRevision,nav=navigation;let part;try{part=await api(`/artifacts/${el.dataset.hash}?offset=${el.dataset.offset||0}`)}catch(error){if(request!==detailRevision||nav!==navigation||!$('#detail-dialog').open)return;if(previous)showDetail(previousTitle,previous);else showDetail('产物读取失败',`${artifactOrigin?'<button class="small" data-action="artifact-back">← 返回来源记录</button>':''}<p class="inline-error">${esc(error.message)}</p><a href="/api/artifacts/${esc(el.dataset.hash)}/download" download>下载原始产物 ↓</a>`);throw error}if(request!==detailRevision||nav!==navigation||!$('#detail-dialog').open)return;const bytes=Uint8Array.from(atob(part.data_base64||''),c=>c.charCodeAt(0));const content=new TextDecoder().decode(bytes);showDetail(`原始产物 / ${part.bytes} 字节`, `${artifactOrigin?'<button class="small" data-action="artifact-back">← 返回来源记录</button>':''}<p class="note mono">SHA256 ${esc(part.hash)} · 字节 ${part.offset}—${part.next_offset}</p><p class="note">此处按 UTF-8 预览；边界截断与二进制以原始下载为准。</p><pre>${esc(content)}</pre><div class="buttons section-gap">${part.offset>0?`<button class="small" data-action="artifact" data-hash="${esc(part.hash)}" data-offset="0">回到开头</button><button class="small" data-action="artifact" data-hash="${esc(part.hash)}" data-offset="${Math.max(0,part.offset-65536)}">← 上一段</button>`:''}<a href="/api/artifacts/${esc(part.hash)}/download" download>原始字节下载 ↓</a>${part.eof?'':`<button class="small" data-action="artifact" data-hash="${esc(part.hash)}" data-offset="${part.next_offset}">读取下一段 →</button>`}</div>`);return}
  if(action==='task-dispatch'){await mutate(`/runs/${id}/tasks/${encodeURIComponent(el.dataset.task)}/dispatch`);notice('Worker 已启动，点击子运行查看实时内容');return}
  if(action==='task-retry'){await mutate(`/runs/${id}/tasks/${encodeURIComponent(el.dataset.task)}/retry`);notice('任务已恢复到前沿；再次调度会新建 Worker，旧记录保留');return}
  if(action==='frontier-dispatch'){await mutate(`/runs/${id}/plan/dispatch`);notice('已按优先级和容量调度前沿任务');return}
  if(action==='worker-refresh'){await refreshRun(id);notice('Worker 只读状态已刷新');return}
  if(action==='worker-cancel'){await mutate(`/runs/${id}/workers/${el.dataset.worker}/cancel`);notice('Worker 取消请求已提交');return}
  if(action==='worker-recover'){await mutate(`/runs/${id}/workers/recover`);notice('过期 lease 与 worker 状态已核对');return}
  if(action==='worker-wait'){notice('正在等待 Worker；其余页面可继续操作');await api(`/runs/${id}/workers/wait`,'POST',{seconds:30});await refresh();notice('Worker 等待已返回，状态已刷新');return}
  if(action==='task-edit'||action==='task-new'){
    const task=state.snap.plan?.tasks.find(t=>t.id===el.dataset.id)||{};
    for(const [field,key]of [['id','id'],['goal','goal'],['completion','completion_criteria'],['predicate','success_contains'],['mode','mode'],['priority','priority']])setField(state,'task-'+field,task[key]??(field==='mode'?'explore':field==='priority'?0:''));
    document.querySelectorAll('[data-dependency]').forEach(input=>{input.checked=(task.dependencies||[]).includes(input.dataset.dependency);input.disabled=input.dataset.dependency===task.id;rememberInput(state,input)});
    document.querySelectorAll('[data-task-input]').forEach(input=>{input.checked=(task.input_attachment_ids||[]).includes(input.dataset.taskInput);rememberInput(state,input)});
    $('#task-id').readOnly=!!task.id;rememberInput(state,$('#task-id'));$('#task-editor').open=true;$('#task-goal').focus();return;
  }
  if(action==='asset-edit'){
    const asset=state.snap.assets.find(a=>a.id===el.dataset.id);$('#detail-dialog').close();if(state.page==='history'&&state.taskTab!=='assets'){state.taskTab='assets';render()}
    for(const [field,key]of [['id','id'],['kind','kind'],['value','value'],['coverage','coverage'],['predicate','predicate'],['parent','parent_id'],['source','source_run_id'],['scope','scope']])setField(state,'asset-'+field,asset[key]||(field==='scope'?'included':''));
    setField(state,'asset-evidence',(asset.evidence_ids||[]).join(', '));$('#asset-editor').open=true;$('#asset-id').focus();return;
  }
  if(action==='mcp-edit'||action==='mcp-new'){
    const m=action==='mcp-edit'?state.config.mcp?.[Number(el.dataset.index)]||{}:{};
    showDetail(m.name?'编辑 MCP · '+m.name:'添加 MCP 服务',mcpEditor());
    setField(state,'mcp-original',m.name?JSON.stringify(m):'');setField(state,'mcp-source','connect');
    for(const field of ['name','description','command','dir','url','github_url','local_path'])setField(state,'mcp-'+field,m[field]||'');
    $('#mcp-enabled').checked=!m.disabled;rememberInput(state,$('#mcp-enabled'));
    setField(state,'mcp-transport',m.url?'http':'stdio');setField(state,'mcp-args',(m.args||[]).join('\n'));
    for(const field of ['env','headers'])setField(state,'mcp-'+field,Object.entries(m[field]||{}).map(([k,v])=>k+'='+v).join('\n'));
    $('#mcp-name').readOnly=!!m.name;rememberInput(state,$('#mcp-name'));mcpTransport();$('#mcp-name').focus();return;
  }
  if(action==='plan-edit'){const editor=$('#plan-json');setField(state,'plan-json',JSON.stringify(state.snap.plan?.tasks||[],null,2));editor.focus();return}
  if(action==='snapshot'){const rev=revision,nav=navigation,request=++detailRevision,snap=await api('/runs/'+id);if(rev!==revision||nav!==navigation||request!==detailRevision)return;showDetail('持久运行快照',`<pre>${esc(JSON.stringify(snap,null,2))}</pre>`);return}
  if(action==='reload-settings'){await loadSettings(true);render(true);notice(state.settingsError||'已重新读取配置；未保存草稿仍保留',!!state.settingsError);return}
  if(action==='discard-config'){resetModelRows();state.providerEditor=providerEditorSelection(state.config);for(const id of ['config-form','mcp-form','integration-form']){forgetConfigRevision(state,$('#'+id));forgetForm(state,$('#'+id))}render(true);formFeedback(state,$('#config-form'),'本页草稿已丢弃，显示已保存配置。');notice('当前配置页草稿已丢弃');return}
}
async function submit(form){const notice=scopedNotice();
  if(form.id==='tool-observation-form'){
    const nav=navigation,request=detailRevision,id=form.dataset.run;
    await api('/runs/'+encodeURIComponent(id)+'/tool-observations','POST',{request_id:form.dataset.request,call_id:form.dataset.call,content:form.elements.content.value});
    if(nav===navigation&&request===detailRevision)$('#detail-dialog').close();
    await refresh();notice('核对记录已保存；未自动恢复运行');return;
  }
  const id=state.selected;
  if(form.id.startsWith('chain-')){await chainEvents.submit(form);return}
  if(form.id==='run-delete-form'){
    const nav=navigation,request=detailRevision,target=form.dataset.run,mode=form.querySelector('#run-delete-mode').value,result=await api('/runs/'+encodeURIComponent(target),'DELETE',{mode});for(const rid of result.run_ids||[target])forgetDeletedRun(rid);if(nav===navigation&&request===detailRevision)$('#detail-dialog').close();await refresh();notice(result.cleanup_warning||(result.permanent?'会话及 '+((result.run_ids||[]).length-1)+' 个 Worker 子运行已永久删除':'会话已隐藏，历史和证据保留；可在“已隐藏会话”中永久删除'),!!result.cleanup_warning,nav===navigation&&request===detailRevision);return;
  }
  if(form.id==='catalog-delete-form'){
    const nav=navigation,request=detailRevision,failures=[],keys=JSON.parse(form.dataset.keys);let deleted=0;
    for(const key of keys){const [run,evidence]=key.split('/');try{const result=await api('/evidence/'+encodeURIComponent(run)+'/'+encodeURIComponent(evidence),'DELETE',{});deleted++;if(result.cleanup_warning)failures.push(evidence+'：'+result.cleanup_warning)}catch(error){failures.push(evidence+'：'+error.message)}}
    await refresh();const message=`已删除 ${deleted} / ${keys.length} 条证据`;
    if(nav!==navigation||request!==detailRevision)return;
    if(failures.length){showDetail('证据清理结果',`<p>${esc(message)}</p><ul>${failures.map(text=>`<li>${esc(text)}</li>`).join('')}</ul>`);return}
    $('#detail-dialog').close();notice(message);return;
  }
  if(form.dataset.verificationOperation){const operation=form.dataset.verificationOperation,evidence_ids=[...form.querySelectorAll('input[name="evidence"]:checked')].map(el=>el.value);if(!evidence_ids.length)throw new Error('请选择实际执行证据');const path=operation==='check'?`/runs/${id}/checks`:`/runs/${id}/findings/${encodeURIComponent(form.dataset.finding)}/retest`;await api(path,'POST',{evidence_ids,requirement_id:form.dataset.requirement});await refresh();notice(operation==='check'?'验收结果已记录':'复测结果已记录');return}
  if(form.id==='new-run-form'){const current={...state},draftRevision=captureForm(current,form),nav=navigation,goal=$('#new-goal').value,start=$('#start-now').checked,requirements=readRequirements($('#new-requirements')),options=readCreationOptions(form),attachments=await uploadAttachments(current,form);const snap=await api('/runs','POST',{goal,start,attachments,requirements,...options});clearAttachments(current,form,attachments);acknowledgeForm(current,form,draftRevision);await refresh();if(nav===navigation)await selectRun(snap.run.id,'history');notice('任务已创建');return}
  if(form.id==='message-form'){
    const current={...state},draftRevision=captureForm(current,form),nav=navigation,kind=$('#message-kind').value,content=$('#message-content').value,attachments=await uploadAttachments(current,form);
    const ack=await runManagement.send(id,kind,{content,attachments});clearAttachments(current,form,attachments);acknowledgeForm(current,form,draftRevision,['message-content']);const messageAck={run:id,text:ack.start_error?'已保存 · 等待恢复运行：'+ack.start_error:ack.status==='cancelled'?'提交已确认 · 指令已取消':ack.status==='delivered'?'已受理 · 指令已投递':'已入队 · 等待运行边界实际投递'};if(nav===navigation&&state.selected===id)state.messageAck=messageAck;formFeedback(current,form,messageAck.text);render();refreshRun(id).catch(()=>{});return;
  }
  if(form.id==='tool-selection-form'){if(['config','tools','mcpStatuses'].some(field=>state.settingsResources?.[field]?.loading))throw new Error('工具目录正在更新，请稍后重试');const ids=mergeToolSelection(state.snap?.run.tool_selection?.ids,state.tools,catalogTools(state.config,state.tools).map(t=>t.definition.function.name),[...form.querySelectorAll('[data-tool-id]:checked')].map(input=>input.dataset.toolId));await mutate(`/runs/${id}/tools`,{ids,replace:true},'PUT');notice('工具选择已保存，下次模型请求生效');return}
  if(form.id==='task-form'){await mutate(`/runs/${id}/plan`,{tasks:readTask(form,state.snap.plan?.tasks||[])},'PUT');notice('任务已保存，可开始的任务已更新');return}
  if(form.id==='integration-form'){await runIntegration('import',readIntegration(form),configRevision(state,form));return}
  if(form.id==='mcp-form'){await saveConfig(readMCP(form,state.config),false,{revision:configRevision(state,form)});return}
  if(form.id==='config-form'){
    const models=state.settingTab==='provider',activateProvider=form.dataset.configActivate==='true',name=models?providerEditorName(state.config,$('#provider-editor-selection')?.value):'',next=readConfigForm(state.config,state.settingTab);
    if(models&&activateProvider){if(name!=='default'&&!Object.hasOwn(next.providers||{},name))throw new Error('请先填写有效模型配置，再保存并设为默认。');next.active_provider=name==='default'?'':name}
    const result=await saveConfig(next,false,{skills:state.settingTab==='skills',revision:configRevision(state,form)});
    if(models){const index=Object.keys(result.config.providers||{}).indexOf(name);state.providerEditor=name==='default'?'default':index>=0?String(index):providerEditorSelection(result.config);form.dataset.saveMessage=result.cleanup_error?`配置已保存；旧连接清理异常：${result.cleanup_error}`:state.settingsError?'配置已保存；本页资源刷新失败，请重试':activateProvider?`已保存并将「${name}」设为默认；新运行使用此配置。`:`本页配置已保存；当前默认为「${result.config.active_provider||'default'}」。`;form.dataset.saveError=String(!!result.cleanup_error||!!state.settingsError)}
    resetModelRows();return;
  }
  if(form.id==='budget-form'){const body=Object.fromEntries(['max_model_requests','max_actions','max_seconds'].map(key=>[key,Number(form.elements[key].value)]));await api(`/runs/${id}/budget`,'PUT',body);await refreshRun(id);return}
  if(form.id==='capacity-form'){await mutate(`/runs/${id}/plan/capacity`,{capacity:Number($('#plan-capacity').value)});notice('Worker 容量已保存');return}
  if(form.id==='plan-form'){await mutate(`/runs/${id}/plan`,{tasks:JSON.parse($('#plan-json').value)},'PUT');notice('计划已保存，依赖前沿已更新');return}
  if(form.id==='asset-form'){await mutate(`/runs/${id}/assets`,{id:$('#asset-id').value,parent_id:$('#asset-parent').value,source_run_id:$('#asset-source').value,scope:$('#asset-scope').value,kind:$('#asset-kind').value,value:$('#asset-value').value,coverage:$('#asset-coverage').value,predicate:$('#asset-predicate').value,evidence_ids:$('#asset-evidence').value.split(',').map(s=>s.trim()).filter(Boolean)});notice('资产覆盖已保存');return}
  if(form.id==='relation-form'){await mutate(`/runs/${id}/relations`,{from:$('#relation-from').value,to:$('#relation-to').value,kind:$('#relation-kind').value,evidence_ids:$('#relation-evidence').value.split(',').map(s=>s.trim()).filter(Boolean)});notice('资产关系已保存');return}
  if(form.dataset.worker){const current={...state},body=workerMessageInput(form),revision=captureWorkerForm(current,form);await api(`/runs/${id}/workers/${form.dataset.worker}/message`,'POST',{...body,operation_id:revision.operationID});acknowledgeWorkerForm(current,form,revision);await refresh();notice('Worker 消息已保存');return}
}
async function runIntegration(action,body,baseRevision=state.config?.revision){const notice=scopedNotice();
  state.integrationFeedback={busy:true,message:({import:'正在下载项目并接入…',check:'正在检查 GitHub 上游版本…',update:'正在更新项目并重新加载…'})[action]};render();
  const dialogOpen=$('#detail-dialog').open,dialogRequest=detailRevision;
  $('#integration-dialog-status')?.remove();$('#dialog-error')?.remove();
  if(dialogOpen){const status=document.createElement('p');status.id='integration-dialog-status';status.className='note';status.setAttribute('role','status');status.textContent=state.integrationFeedback.message;$('#dialog-content').prepend(status)}
  try{
    const result=await api('/integrations/'+action,'POST',body,action==='check'?{}:{headers:configHeaders(baseRevision)});
    if(action!=='check'){settingsLoader.saved(result.config||state.config,{skills:body.kind==='skill'});await loadSettings()}
    state.integrationFeedback={message:integrationResult(result)+(action!=='check'&&state.settingsError?'；配置已保存，本页资源刷新失败，请重试':''),error:!!result.cleanup_error||action!=='check'&&!!state.settingsError};
    if(action==='update'&&dialogOpen&&dialogRequest===detailRevision&&$('#detail-dialog').open&&$('#dialog-title').textContent===body.name){
      if(body.kind==='mcp'&&state.settingsResources.tools.available===false){$('#detail-dialog').close()}
      else if(body.kind==='mcp'){const index=state.config.mcp.findIndex(m=>m.name===body.name);if(index>=0)showDetail(body.name,mcpDetail(state.config,state.tools,index,state.mcpStatuses))}
      if(body.kind==='skill'){const meta=state.skills.find(t=>t.name===body.name);if(meta){const skill=await api('/skills/'+encodeURIComponent(body.name));if(dialogRequest===detailRevision&&$('#detail-dialog').open)showDetail(body.name,`${sourceLinks(meta)}${integrationActions(meta,'skill',meta.name)}<pre>${esc(skill.content)}</pre>`)}}
    }
    if(dialogOpen&&$('#detail-dialog').open){const status=$('#integration-dialog-status');if(status)status.textContent=state.integrationFeedback.message}
    notice(state.integrationFeedback.message,!!result.cleanup_error);
  }catch(error){state.integrationFeedback={message:error.message,error:true};$('#integration-dialog-status')?.remove();throw error}
}
function loadSettings(invalidate=false,tab=state.page==='settings'?state.settingTab:'provider'){return settingsLoader.load(tab,invalidate)}
// Provider-only loads change the shell, not the mounted transcript. Tool
// catalog loads still repaint pending calls if navigation overtakes them.
function settingsLoaded(tab='provider'){modelConnection();if(tab==='provider'&&state.page==='history'&&state.historyDetail){renderProviderLabel();providerProbe.sync()}else render()}

document.addEventListener('toggle',e=>{const el=e.target;
  if(el.matches?.('[data-worker-activity]')){
    if(el.isConnected===false||!state.historyDetail||state.taskTab!=='activity'||!state.snap||state.workerActivityExpanded===el.open)return;
    state.workerActivityExpanded=el.open;
    // Native redraw restores disclosures too; only a changed open state asks
    // for child details. Reuse workspace single-flight, abort and SSE guards.
    if(el.open&&state.snap.projection==='activity'){const notice=scopedNotice();refreshRun().catch(error=>notice(error.message,true,false))}return;
  }
  if(!el.matches?.('[data-lazy-tool]')||!el.open||!state.snap)return;const body=measureUI('tool-detail-serialize',()=>toolDisclosureBody(el,state.snap));if(body!==null)patchLive(el.querySelector('.tool-body'),body)
},true);
document.addEventListener('click',async e=>{
  if(e.target.closest('a[data-page="history"]')){cancelRunReads();source?.close();source=null;clearTimeout(refreshTimer);refreshTimer=null;navigation++;state.page='history';state.historyDetail=false;render()}
  const el=e.target.closest('[data-action]');if(!el)return;e.preventDefault();const key=operationKey(el);if(el.disabled||pending.has(key))return;
  const notice=scopedNotice(),dialogRequest=detailRevision,dialogNav=navigation;pending.add(key);applyPending();try{if(!await runManagement.dispatch(el.dataset.action,el)&&!await delivery.dispatch(el.dataset.action,el))await dispatch(el.dataset.action,el)}catch(error){if(el.dataset.action==='provider-profile-switch')formFeedback(state,el.closest('#config-form'),error.message,true);if(el.matches('[data-switch]'))state.extensionFeedback={message:error.message,error:true};else notice(error.message,true,dialogNav===navigation&&dialogRequest===detailRevision)}finally{pending.delete(key);if(['select','run-retry','catalog-open-run'].includes(el.dataset.action))applyPending();else render();if(["mcp-edit","mcp-new"].includes(el.dataset.action))mcpTransport()}
});
document.addEventListener('submit',async e=>{
  if(!e.target.closest('#page,#detail-dialog'))return;e.preventDefault();const form=e.target,key=operationKey(form);if(pending.has(key))return;
  const notice=scopedNotice(),scopeState={...state},dialogRequest=detailRevision,dialogNav=navigation,dialogForm=form.closest('#detail-dialog');if(form.id==='config-form')form.dataset.configActivate=String(e.submitter?.dataset.configActivate==='true');pending.add(key);applyPending();try{if(!await runManagement.submit(form))await submit(form);if(!form.dataset.worker&&!['message-form','new-run-form'].includes(form.id)){forgetForm(scopeState,form);forgetConfigRevision(scopeState,form)}if((form.id==='mcp-form'||form.id==='integration-form'&&form.dataset.kind==='mcp')&&dialogRequest===detailRevision&&dialogNav===navigation&&$('#detail-dialog').open)$('#detail-dialog').close();if(form.id!=='message-form')formFeedback(scopeState,form,form.id==='config-form'?form.dataset.saveMessage||'配置已保存':'已提交保存',form.dataset.saveError==='true')}catch(error){
    // beginDetail may advance its request token without replacing the form.
    const ownsDialog=dialogForm&&dialogForm===$('#detail-dialog')&&dialogForm.open&&dialogNav===navigation&&form.isConnected&&form.closest('#detail-dialog')===dialogForm;
    if(ownsDialog){$('#notice').hidden=true;$('#dialog-error')?.remove();formFeedback(scopeState,form,error.message,true)}
    else if(!dialogForm){formFeedback(scopeState,form,error.message,true);if(form.id!=='message-form')notice(error.message,true,false)}
  }finally{pending.delete(key);for(const control of form.querySelectorAll('[data-pending-disabled]')){control.disabled=control.dataset.pendingDisabled==='true';delete control.dataset.pendingDisabled;if(control.dataset.pendingLabel!==undefined){control.textContent=control.dataset.pendingLabel;delete control.dataset.pendingLabel}}render(true)}
});
document.addEventListener('change',e=>{
  if(e.target.id.startsWith('model-')||e.target.id.startsWith('caps-')){syncModelFields(state.config,e.target.id);if(/model-role-.+-(provider|model)$/.test(e.target.id))for(const suffix of ['model','effort'])rememberInput(state,document.getElementById(e.target.id.replace(/-(provider|model)$/,'-'+suffix)))}
  if(e.target.id.startsWith('recon-')){syncReconFields(state.config,e.target.id);if(e.target.id==='recon-provider')for(const id of ['recon-model','recon-effort'])rememberInput(state,document.getElementById(id))}
  if(e.target.id==='input-files'){try{chooseAttachments(state,e.target)}catch(error){notice(error.message,true)}return}
  if(e.target.id.startsWith('provider-')||e.target.id.startsWith('model-provider-'))providerProbe.sync();
  if(e.target.id==='theme-select'){applyTheme(e.target.value);return}
  if(e.target.id==='catalog-all'){const keys=new Set(state.catalogSelected||[]);for(const entry of catalogVisible(state)){if(e.target.checked)keys.add(evidenceKey(entry));else keys.delete(evidenceKey(entry))}state.catalogSelected=[...keys];render();return}
  if(e.target.dataset.catalogKey){const keys=new Set(state.catalogSelected||[]);if(e.target.checked)keys.add(e.target.dataset.catalogKey);else keys.delete(e.target.dataset.catalogKey);state.catalogSelected=[...keys];render();return}
  if(['catalog-run','catalog-source','catalog-status','catalog-relation'].includes(e.target.id)){state[{'catalog-run':'catalogRun','catalog-source':'catalogSource','catalog-status':'catalogStatus','catalog-relation':'catalogRelation'}[e.target.id]]=e.target.value;render();return}
  if(e.target.closest('#page,#detail-dialog')){rememberConfigRevision(state,e.target.form);rememberInput(state,e.target)}
  if(e.target.id==='replay-position'){const event=replayEvents(state.replaySnapshot)[Number(e.target.value)];if(event){state.replaySequence=event.sequence;render()}return}
  if(e.target.id==='run-selector')selectRun(e.target.value);
  if(e.target.id==='history-status'){state.historyStatus=e.target.value;render()}
  if(e.target.id==='evidence-relation'){state.evidenceRelation=e.target.value;render();return}
  if(e.target.id==='evidence-status'){state.evidenceStatus=e.target.value;render()}
  if(e.target.id==='event-filter'){state.eventFilter=e.target.value;if(['model_stream','tool_output','task_claimed'].includes(state.eventFilter))state.eventsRaw=true;loadEventHistory();return}
  if(e.target.id==='graph-filter'){state.graphFilter=e.target.value;render()}
  if(e.target.id==='tool-source'||e.target.dataset.toolId)filterTools();if(['mcp-transport','mcp-source'].includes(e.target.id))mcpTransport();
});
document.addEventListener('input',e=>{
  if(e.target.id.startsWith('provider-')||e.target.id.startsWith('model-provider-'))providerProbe.sync();
  if(e.target.closest('#page,#detail-dialog')){rememberConfigRevision(state,e.target.form);rememberInput(state,e.target)}
  if(e.target.id==='history-filter'){state.historyFilter=e.target.value;render()}
  if(e.target.id==='catalog-query'){state.catalogQuery=e.target.value;render();return}
  if(e.target.id==='chain-query'){state.chainQuery=e.target.value;clearTimeout(state.chainSearchTimer);state.chainSearchTimer=setTimeout(()=>chainEvents.load(true).then(()=>render()),250);return}
  if(e.target.id==='evidence-query'){state.evidenceQuery=e.target.value;render()}
  if(e.target.id==='event-query'){state.eventQuery=e.target.value;clearTimeout(state.eventSearchTimer);state.eventSearchTimer=setTimeout(()=>loadEventHistory(),300);return}
  if(e.target.id==='exploration-search')updateExplorationSearch(e.target);
  if(e.target.id==='integration-filter')filterIntegrationCatalog();
  if(e.target.id==='graph-search'){graphMatchIndex=0;locateGraph()}
  if(e.target.id==='tool-filter'||e.target.id==='skill-filter')filterTools();
  if(e.target.id==='service-tool-filter')filterServiceTools();
});
$('#close-dialog').onclick=()=>$('#detail-dialog').close();
$('#detail-dialog').addEventListener('close',()=>{detailRevision++;mcpStatus.close();delivery.close();if($('#mcp-editor'))for(const form of $('#dialog-content').querySelectorAll('form')){forgetForm(state,form);forgetConfigRevision(state,form);form.reset()}});
$('.skip').onclick=e=>{e.preventDefault();$('#workspace').focus();$('#workspace').scrollIntoView({block:'start'})};
$('#detail-dialog').addEventListener('click',e=>{if(e.target===$('#detail-dialog'))$('#detail-dialog').close()});
window.addEventListener('hashchange',()=>{cancelRunReads();navigation++;const route=resolveRoute(location.hash,state.selected);if(route.runID){selectRun(route.runID,'history',{replace:true,deepLink:route.deepLink});return}Object.assign(state,route);if(!state.historyDetail){source?.close();source=null;clearTimeout(refreshTimer);refreshTimer=null}render();if(state.page==='settings')loadSettings().then(()=>{modelConnection();render()});if(state.page==='evidence')refresh()});
function clock(){const now=new Date();$('#clock').textContent=now.toLocaleTimeString('zh-CN',{hour12:false});$('#day').textContent=now.toLocaleDateString('zh-CN',{year:'numeric',month:'2-digit',day:'2-digit'}).replaceAll('/',' . ')}clock();setInterval(clock,1000);
async function boot(){
  const route=resolveRoute(location.hash,state.selected);Object.assign(state,route);state.runsLoaded=false;if(!route.runID)render();
  const settingsTab=state.page==='settings'?state.settingTab:'provider';
  loadSettings(false,settingsTab).then(()=>settingsLoaded(settingsTab)).catch(error=>{state.settingsError=error.message;settingsLoaded(settingsTab)});
  refreshList().catch(()=>connection('工作台连接失败',false));
  if(route.runID)selectRun(route.runID,'history',{replace:true,deepLink:route.deepLink});
  if(state.page==='evidence')loadCatalog().then(()=>render());
}boot();
// A global tick can land just after entry while SSE is still connecting.
// Reuse the accepted read for this interval; semantic invalidations stay eager.
// Web ownership can end after the last durable event, so an open SSE alone
// cannot keep a stale execution_active=true projection fresh forever.
setInterval(()=>{if(document.hidden)return;if(!state.historyDetail||!state.selected)refreshList().catch(()=>{});else if(!state.runLoading&&!runLoads.has(state.selected)&&(state.workspaceReadAt===undefined||uiNow()-state.workspaceReadAt>=5000)&&(!source||source.readyState!==EventSource.OPEN||state.snap?.run.execution_active===true))refreshRun().catch(()=>{})},5000);
