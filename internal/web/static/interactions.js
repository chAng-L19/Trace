// Drafts live only in this browser tab; never persist connection secrets to storage.
// Only reloadable presentation state is evicted, never drafts or send identities.
export function boundedCache(limit,maxBytes=1024*1024){
  const cache=new Map(),sizes=new Map();let bytes=0;
  const remove=cache.delete.bind(cache),read=cache.get.bind(cache),write=cache.set.bind(cache);
  cache.delete=key=>{if(!cache.has(key))return false;bytes-=sizes.get(key);sizes.delete(key);return remove(key)};
  cache.clear=()=>{for(const key of cache.keys())cache.delete(key)};
  cache.get=key=>{const value=read(key);if(cache.has(key)){remove(key);write(key,value)}return value};
  cache.set=(key,value)=>{const encoded=JSON.stringify(value);cache.delete(key);if(encoded===undefined)return cache;const size=2*(String(key).length+encoded.length);if(size>maxBytes)return cache;write(key,value);sizes.set(key,size);bytes+=size;while(cache.size>limit||bytes>maxBytes)cache.delete(cache.keys().next().value);return cache};
  return cache;
}
const drafts=new Map(),views=boundedCache(64),feedback=boundedCache(128),workerSends=new Map();
let renderedScope='',renderedOuterScope='',lastHTML='';
export const viewScope=s=>s.page==='overview'?'overview':s.page==='settings'?`settings:${s.settingTab}`:s.page==='evidence'?'evidence:manager':s.page==='history'?(s.historyDetail===false?'history:archive':`history:${s.selected}:${s.runLoading?'loading':s.taskTab||'activity'}`):`${s.page}:${s.selected}`;
// Keep activity scrolling scoped to the selected run.
const outerScope=s=>s.page==='history'&&s.historyDetail!==false&&!s.runLoading&&(s.taskTab||'activity')==='activity'?`history:${s.selected}:activity`:viewScope(s);
const fieldKey=(s,el)=>`${el.form?.id==='message-form'?'message:'+s.selected:viewScope(s)}:${el.dataset.toolId?s.selected+':':''}${el.id}`;
// Keep keyed messages, disclosures, and scroll containers alive during SSE paints.
// Native details.open belongs to the reader once mounted, not to the renderer.
const nodeKey=node=>node.nodeType===1&&(node.id||node.getAttribute('data-scroll'));
function restoreSelectValue(element,value,config=element.hasAttribute('data-config-base')){
  if([...element.options].some(option=>option.value===value))element.value=value;
  else if(config){const missing=element.ownerDocument.createElement('option');missing.value=value;missing.textContent=(value||'空值')+'（选项已移除，请重新选择）';missing.disabled=true;missing.dataset.configMissing='true';element.appendChild(missing);element.value=value;element.dataset.configConflict='true'}
}
function reconcile(parent,next,first=parent.firstChild,end=null){
  const keyed=new Map();for(let node=first;node&&node!==end;node=node.nextSibling){const key=nodeKey(node);if(key)keyed.set(key,node)}
  let cursor=first;
  for(const fresh of [...next.childNodes]){
    const key=nodeKey(fresh);let old=key?keyed.get(key):cursor&&cursor!==end&&!nodeKey(cursor)?cursor:null;
    if(!old||old.nodeType!==fresh.nodeType||old.nodeName!==fresh.nodeName){old=fresh.cloneNode(true);parent.insertBefore(old,cursor)}
    else{
      if(old!==cursor)parent.insertBefore(old,cursor);
      if(old.nodeType===3){if(old.data!==fresh.data)old.data=fresh.data}
      else if(old.nodeType===1){
        const previousBase=old.getAttribute('data-config-base'),nextBase=fresh.getAttribute('data-config-base');
        let selectedValue=old.tagName==='SELECT'?old.value:undefined;
        const checkedBase=old.getAttribute('data-config-checked'),nextChecked=fresh.getAttribute('data-config-checked');
        if(nextChecked!==null&&checkedBase!==nextChecked){if(String(old.checked)===checkedBase||String(old.checked)===nextChecked){old.checked=nextChecked==='true';delete old.dataset.configConflict}else old.dataset.configConflict='true'}
        if(nextBase!==null&&previousBase!==nextBase){if(old.value===previousBase||old.value===nextBase){if(old.tagName==='SELECT')selectedValue=nextBase;else old.value=nextBase;delete old.dataset.configConflict}else old.dataset.configConflict='true'}
        const phaseChanged=old.tagName==='DETAILS'&&fresh.hasAttribute('data-reasoning-phase')&&old.getAttribute('data-reasoning-phase')!==fresh.getAttribute('data-reasoning-phase');
        for(const attr of [...old.attributes])if(!fresh.hasAttribute(attr.name)&&attr.name!=='data-config-conflict'&&!(old.tagName==='DETAILS'&&attr.name==='open'))old.removeAttribute(attr.name);
        for(const attr of fresh.attributes)if(!(old.tagName==='DETAILS'&&attr.name==='open')&&old.getAttribute(attr.name)!==attr.value)old.setAttribute(attr.name,attr.value);
        if(phaseChanged)old.open=fresh.open;
        if(!['INPUT','TEXTAREA','SELECT'].includes(old.tagName))reconcile(old,fresh);
        else if(old.tagName==='SELECT'){
          // A new baseline may not exist in the old option list. Apply the
          // chosen baseline/draft only after installing the current options.
          if(old.innerHTML!==fresh.innerHTML)old.innerHTML=fresh.innerHTML;
          restoreSelectValue(old,selectedValue,nextBase!==null);
        }
      }
    }
    cursor=old.nextSibling;
  }
  while(cursor&&cursor!==end){const nextSibling=cursor.nextSibling;cursor.remove();cursor=nextSibling}
}
// Independently adapted follow intent / message anchor principles from Harness
// fcc0ad462172 use-scroll-follow and Muteki 3b8fd7abe4ee reading position.
// Programmatic compensation is not a reader request to resume following.
const scrollIntent=new WeakMap();
export function captureScroll(el){
 const previous=scrollIntent.get(el),moved=!previous||Math.abs(el.scrollTop-previous.top)>.5;
 const follow=moved?el.scrollHeight-el.scrollTop-el.clientHeight<45:previous.follow;
 // Following restores from scrollHeight; historical geometry is only needed
 // for a reader who has left the live edge.
 const edge=!follow&&el.dataset?.scroll==='conversation'?el.getBoundingClientRect?.().top:undefined;
 const node=el.dataset?.scroll==='conversation'&&edge!==undefined?[...el.querySelectorAll('[data-message-id]')].find(n=>n.getBoundingClientRect().bottom>=edge):null;
 return {top:el.scrollTop,left:el.scrollLeft,follow,id:node?.dataset.messageId,offset:node?node.getBoundingClientRect().top-edge:0};
}
export function restoreScroll(el,saved,{follow=saved.follow}={}){
 const node=saved.id?[...el.querySelectorAll('[data-message-id]')].find(n=>n.dataset.messageId===saved.id):null;
 el.scrollTop=follow?el.scrollHeight:node?el.scrollTop+node.getBoundingClientRect().top-el.getBoundingClientRect().top-saved.offset:saved.top;
 el.scrollLeft=saved.left||0;
 scrollIntent.set(el,{follow,top:el.scrollTop});
}
export function patchLive(element,html,first=element?.firstChild,end=null,manageScroll=true){
  if(!element)return;
  // 保存焦点状态（修复对话输入失焦问题）
  const active=document.activeElement;
  const focusId=active?.id;
  const selection=active&&['INPUT','TEXTAREA'].includes(active.tagName)?[active.selectionStart,active.selectionEnd]:null;
  
  // A stream patch walks only its sibling range, including nested scrollboxes.
  const scrollNodes=start=>{const nodes=[element];for(let node=start;node&&node!==end;node=node.nextSibling)if(node.nodeType===1){if(node.hasAttribute('data-scroll'))nodes.push(node);nodes.push(...node.querySelectorAll('[data-scroll]'))}return nodes};
  const before=first?.previousSibling,scrolls=scrollNodes(first).filter(el=>manageScroll||el!==element).map(el=>[el.dataset.scroll,captureScroll(el)]);
  const template=document.createElement('template');template.innerHTML=html;reconcile(element,template.content,first,end);
  const current=new Map(scrollNodes(before?before.nextSibling:element.firstChild).map(el=>[el.dataset.scroll,el]));
  for(const [key,saved] of scrolls){const el=current.get(key);if(el)restoreScroll(el,saved)}
  
  // 恢复焦点（如果焦点元素仍存在）
  if(focusId){
    const restored=document.getElementById(focusId);
    if(restored&&restored!==active){
      restored.focus({preventScroll:true});
      if(selection&&['INPUT','TEXTAREA'].includes(restored.tagName)){
        try{restored.setSelectionRange(...selection)}catch{}
      }
    }
  }
}
export function patchConversationPreview(element,patch){
  if(!element)return false;
  if(patch.full!==undefined){patchLive(element,patch.full);return true}
  const find=id=>{const node=document.getElementById(id);return node&&element.contains(node)?node:null};
  const start=find('conversation-live-start'),end=find('conversation-live-end');
  if(!start||!end)return false;
  const saved=captureScroll(element);
  for(const group of patch.groups){
    const node=find(group.id);if(!node)continue;
    if(group.summary!==undefined){const summary=node.querySelector('summary');if(summary)patchLive(summary,group.summary)}
    for(const call of group.calls){const target=find(call.id);if(target)patchLive(target.parentNode,call.html,target,target.nextSibling,false)}
  }
  patchLive(element,patch.tail,start.nextSibling,end,false);
  restoreScroll(element,saved);
  return true;
}
export function formFeedback(s,form,message,error=false){if(!form)return;feedback.set(`${viewScope(s)}:${form.id||form.dataset.worker}`,{message,error});paintFeedback(s,form)}
function paintFeedback(s,form){const item=feedback.get(`${viewScope(s)}:${form.id||form.dataset.worker}`);if(!item)return;let el=form.querySelector('[data-form-feedback]');if(!el){el=document.createElement('p');el.dataset.formFeedback='true';el.setAttribute('role','status');form.append(el)}el.className=item.error?'inline-error':'note';el.textContent=item.message}
export function rememberInput(s,el){if(el.id&&el.dataset.ephemeral!=='true'){drafts.set(fieldKey(s,el),{value:el.value,checked:el.checked,readOnly:el.readOnly,base:drafts.get(fieldKey(s,el))?.base??el.dataset.configChecked??el.dataset.configBase});if(el.form)formFeedback(s,el.form,'有未保存的草稿 · 切换页面会保留，关闭此标签页后丢弃。')}}
export function forgetForm(s,form){if(!form)return;for(const el of form.querySelectorAll('input[id],textarea[id],select[id]')){drafts.delete(fieldKey(s,el));if(el.dataset.configBase!==undefined){el.value=el.dataset.configBase;delete el.dataset.configConflict}if(el.dataset.configChecked!==undefined){el.checked=el.dataset.configChecked==='true';delete el.dataset.configConflict}}feedback.delete(`${viewScope(s)}:${form.id||form.dataset.worker}`)}
export function captureForm(s,form){return {scope:viewScope(s),fields:[...form.querySelectorAll('input[id],textarea[id],select[id]')].map(el=>({id:el.id,key:fieldKey(s,el),draft:drafts.get(fieldKey(s,el)),value:el.value,checked:el.checked}))}}
// ACK clears only the revision actually submitted, even after A -> B -> A.
export function acknowledgeForm(s,form,revision,clearIDs){
  if(revision.fields.some(f=>drafts.get(f.key)!==f.draft))return false;
  const current=renderedScope===revision.scope?document.getElementById(form.id):null;
  if(current){const fields=new Map([...current.querySelectorAll('input[id],textarea[id],select[id]')].map(el=>[el.id,el]));if(revision.fields.some(f=>!fields.has(f.id)||fields.get(f.id).value!==f.value||fields.get(f.id).checked!==f.checked))return false}
  for(const field of revision.fields){if(clearIDs&&!clearIDs.includes(field.id))continue;drafts.delete(field.key);const el=current?.querySelector('#'+field.id);if(el&&clearIDs)el.value=''}
  feedback.delete(`${revision.scope}:${form.id||form.dataset.worker}`);return true;
}
// Draft object identity records edits, including a value changed and changed back.
// Remounting a form restores those objects without creating a new draft revision.
export function captureWorkerForm(s,form){
  const revision={scope:viewScope(s),formID:form.id,worker:form.dataset.worker,contentID:form.querySelector('[name="content"]')?.id,replyTo:form.querySelector('[name="reply_to"]')?.value,
    fields:[...form.querySelectorAll('input[id],textarea[id],select[id]')].map(el=>({id:el.id,key:fieldKey(s,el),draft:drafts.get(fieldKey(s,el)),value:el.value,checked:el.checked}))};
  const key=JSON.stringify([revision.scope,revision.formID]),prior=workerSends.get(key);
  if(prior&&prior.worker===revision.worker&&prior.replyTo===revision.replyTo&&prior.fields.length===revision.fields.length&&prior.fields.every((f,i)=>{const n=revision.fields[i];return f.id===n.id&&f.draft===n.draft&&f.value===n.value&&f.checked===n.checked}))revision.operationID=prior.operationID;
  else revision.operationID=globalThis.crypto?.randomUUID?.()||`worker-${Date.now()}-${Math.random().toString(16).slice(2)}`;
  workerSends.set(key,revision);
  return revision;
}
export function acknowledgeWorkerForm(s,form,revision){
  const key=JSON.stringify([revision.scope,revision.formID]);
  if(workerSends.get(key)?.operationID===revision.operationID)workerSends.delete(key);
  if(revision.scope!==viewScope(s)||revision.formID!==form.id||revision.fields.some(field=>drafts.get(field.key)!==field.draft))return;
  const content=revision.fields.find(field=>field.id===revision.contentID);if(!content)return;
  const currentForm=renderedScope===revision.scope?document.getElementById(revision.formID):null;
  let current;
  if(currentForm){
    const fields=[...currentForm.querySelectorAll('input[id],textarea[id],select[id]')],byID=new Map(fields.map(el=>[el.id,el]));
    if(currentForm.dataset.worker!==revision.worker||currentForm.querySelector('[name="reply_to"]')?.value!==revision.replyTo||fields.length!==revision.fields.length||revision.fields.some(field=>{const el=byID.get(field.id);return !el||el.value!==field.value||el.checked!==field.checked}))return;
    current=byID.get(revision.contentID);
  }
  drafts.delete(content.key);if(current)current.value='';
}
export function setField(s,id,value){const el=document.getElementById(id);if(!el)return;el.value=value;rememberInput(s,el)}
function syncDialogueDock(){
  const panel=document.querySelector('.task-workbench .workspace-panel');if(!panel)return;
  const tabs=document.querySelector('.task-tabs')?.getBoundingClientRect(),edge=tabs?.bottom??0;
  // Measure from the stationary workbench, not the already-stuck panel.
  const style=document.getElementById('workspace').style,offset=Math.max(0,edge-panel.closest('.task-workbench').getBoundingClientRect().top)+'px';
  const tabHeight=(tabs?.height??0)+'px';if(style.getPropertyValue('--dialogue-tabs')!==tabHeight)style.setProperty('--dialogue-tabs',tabHeight);
  const headHeight=panel.querySelector('.workspace-dialogue-head').getBoundingClientRect().height+'px';
  if(style.getPropertyValue('--dialogue-head-height')!==headHeight)style.setProperty('--dialogue-head-height',headHeight);
  if(style.getPropertyValue('--dialogue-offset')===offset)return;
  const pane=panel.querySelector('[data-scroll="conversation"]'),atEnd=pane&&pane.scrollHeight-pane.clientHeight-pane.scrollTop<=1;
  style.setProperty('--dialogue-offset',offset);
  if(atEnd)pane.scrollTop=pane.scrollHeight;
}
if(typeof window!=='undefined'){
  window.addEventListener('scroll',e=>{if(e.target===document||e.target.id==='workspace')syncDialogueDock()},{capture:true,passive:true});
  window.addEventListener('resize',syncDialogueDock);
}
export function redraw(s,html,force=false){
  const page=document.getElementById('page'),workspace=document.getElementById('workspace'),scope=viewScope(s),same=scope===renderedScope,outer=outerScope(s),sameOuter=outer===renderedOuterScope,outerTop=workspace.scrollTop;
  if(same&&html===lastHTML&&!force)return false;
  const enteringDialogue=!s.runError&&s.snap?.run?.id===s.selected&&renderedScope===`history:${s.selected}:loading`&&scope===`history:${s.selected}:activity`;
  const loadingInput=enteringDialogue?document.querySelector('.workspace-loading textarea')?.getBoundingClientRect():null;
  const notice=document.getElementById('notice'),noticeHome=document.getElementById('notice-home');
  if(notice&&noticeHome)noticeHome.append(notice);
  const active=document.activeElement,focus=same?active?.id:'',selection=[active?.selectionStart,active?.selectionEnd];
  if(renderedScope)views.set(renderedScope,{top:workspace.scrollTop,details:[...page.querySelectorAll('details')].map(el=>[el.id||el.querySelector('summary')?.textContent,el.open,el.dataset?.reasoningPhase]),scrolls:[...page.querySelectorAll('[data-scroll]')].map(el=>[el.dataset.scroll,captureScroll(el)])});
  if(same&&page.ownerDocument?.createElement){const template=document.createElement('template');template.innerHTML=html;reconcile(page,template.content)}else page.innerHTML=html;
  if(notice)(document.getElementById('workspace-notice')||noticeHome)?.append(notice);
  lastHTML=html;renderedScope=scope;renderedOuterScope=outer;
  for(const el of page.querySelectorAll('input[id],textarea[id],select[id]')){
    const draft=drafts.get(fieldKey(s,el));if(!draft)continue;
    const base=el.dataset.configChecked??el.dataset.configBase,draftValue=el.dataset.configChecked!==undefined?String(draft.checked):draft.value;
    if(base!==undefined){if(draftValue===base){drafts.delete(fieldKey(s,el));delete el.dataset.configConflict;continue}if(draft.base!==base)el.dataset.configConflict='true'}
    // A status change can remove a message mode; keep the valid new default.
    if(el.value!==draft.value){if(el.tagName==='SELECT')restoreSelectValue(el,draft.value);else el.value=draft.value}
    if(el.type==='checkbox')el.checked=draft.checked;if('readOnly' in el)el.readOnly=!!draft.readOnly;
  }
  for(const form of page.querySelectorAll('form'))paintFeedback(s,form);
  const view=views.get(scope);
  if(view){const details=new Map(view.details.map(([key,open,phase])=>[key,{open,phase}]));for(const el of page.querySelectorAll('details')){const saved=details.get(el.id||el.querySelector('summary')?.textContent);if(saved&&saved.phase===el.dataset?.reasoningPhase)el.open=saved.open}
    const scrolls=new Map([...page.querySelectorAll('[data-scroll]')].map(el=>[el.dataset.scroll,el]));for(const [key,saved]of view.scrolls){const el=scrolls.get(key);if(el)restoreScroll(el,saved,{follow:saved.follow&&!['execution-lineage','workspace-lineage'].includes(key)})}
    workspace.scrollTo({top:sameOuter?outerTop:view.top,left:0,behavior:'instant'});
  }else{if(!sameOuter)workspace.scrollTo({top:0,left:0,behavior:'instant'});for(const el of page.querySelectorAll('[data-scroll="conversation"],[data-scroll="stream"],[data-scroll="events"]'))el.scrollTop=el.scrollHeight;if(sameOuter)workspace.scrollTo({top:outerTop,left:0,behavior:'instant'})}
  // Replacing the skeleton inserts the run controls above the dialogue. Keep
  // its visible input in place on first entry; a saved reading position wins.
  if(loadingInput&&!view&&loadingInput.top>=0&&loadingInput.bottom<=window.innerHeight){
    const owner=window.matchMedia('(min-width:701px)').matches?workspace:document.scrollingElement;
    syncDialogueDock();owner.scrollTop+=document.getElementById('message-content').getBoundingClientRect().top-loadingInput.top;
  }
  syncDialogueDock();
  const input=focus&&document.getElementById(focus);if(input&&input!==active){input.focus({preventScroll:true});try{input.setSelectionRange(...selection)}catch{}}
  document.querySelectorAll('[data-dependency]').forEach(el=>el.disabled=el.dataset.dependency===document.getElementById('task-id')?.value);
  filterTools();mcpTransport();locateGraph();return true;
}
export function locateGraph(){
  const query=(document.getElementById('graph-search')?.value||'').trim().toLowerCase(),nodes=[...document.querySelectorAll('[data-node-search]')];
  const matches=nodes.filter(node=>query&&node.dataset.nodeSearch.includes(query));
  nodes.forEach(node=>node.classList.toggle('search-match',matches.includes(node)));
  const count=document.getElementById('graph-match-count');if(count)count.textContent=query?`匹配 ${matches.length} 个节点`:'按名称、值或完整 ID 定位，保留上下游关系';
  return matches;
}
export function filterTools(){
  const skillQuery=(document.getElementById('skill-filter')?.value||'').trim().toLowerCase(),skills=[...document.querySelectorAll('[data-skill-search]')];for(const card of skills)card.hidden=!!(skillQuery&&!card.dataset.skillSearch.includes(skillQuery));const skillCount=document.getElementById('skill-match-count');if(skillCount)skillCount.textContent=`匹配 ${skills.filter(card=>!card.hidden).length} / ${skills.length}`;
  const query=(document.getElementById('tool-filter')?.value||'').trim().toLowerCase(),source=document.getElementById('tool-source')?.value||'';
  for(const card of document.querySelectorAll('[data-tool-search]'))card.hidden=!!((query&&!card.dataset.toolSearch.includes(query))||(source&&card.dataset.toolSource!==source));
  const selected=document.querySelector('#tool-selection-form'),all=[...document.querySelectorAll(selected?'#tool-selection-form [data-tool-search]':'[data-action="tool-detail"][data-tool-search]')],count=document.getElementById('tool-match-count');
  if(count)count.textContent=`匹配 ${all.filter(el=>!el.hidden).length} / ${all.length} · 已勾选 ${document.querySelectorAll('[data-tool-id]:checked').length}`;
}
export function filterServiceTools(){
  const query=(document.getElementById('service-tool-filter')?.value||'').trim().toLowerCase(),cards=[...document.querySelectorAll('[data-service-search]')];
  for(const card of cards)card.hidden=!!(query&&!card.dataset.serviceSearch.includes(query));const count=document.getElementById('service-tool-count');if(count)count.textContent=`匹配 ${cards.filter(card=>!card.hidden).length} / ${cards.length} 个接口`;const empty=document.getElementById('service-tool-empty');if(empty)empty.hidden=!cards.length||cards.some(card=>!card.hidden);
}
export function mcpTransport(){
  const source=document.getElementById('mcp-source'),editing=!!document.getElementById('mcp-original')?.value;if(source){source.disabled=editing;if(editing)source.value='connect';document.querySelectorAll('[data-mcp-source]').forEach(el=>el.hidden=el.dataset.mcpSource!==source.value)}
  const http=document.getElementById('mcp-transport')?.value==='http';
  document.querySelectorAll('[data-mcp-stdio]').forEach(el=>el.hidden=http);document.querySelectorAll('[data-mcp-http]').forEach(el=>el.hidden=!http);
  const command=document.getElementById('mcp-command'),url=document.getElementById('mcp-url');if(command){command.required=!http;command.disabled=http}if(url){url.required=http;url.disabled=!http;url.type='url'}
}
