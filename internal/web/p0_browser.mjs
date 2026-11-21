// Server-only companion to TestP0BrowserAcceptance. No browser download/fallback.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import {execFileSync} from 'node:child_process';
import {createRequire} from 'node:module';
import {pathToFileURL} from 'node:url';
import {workspaceInteractions} from './workspace_browser.mjs';
const require=createRequire(import.meta.url),modulePath=process.env.TRACE_CLOAKBROWSER_NODE_MODULE;
assert.ok(modulePath&&process.env.CLOAKBROWSER_BINARY_PATH&&process.env.DISPLAY,'fixed SDK/binary and headed DISPLAY required');
assert.ok(fs.existsSync(process.env.CLOAKBROWSER_BINARY_PATH),'fixed browser binary is absent');
const entry=require.resolve(modulePath);let dir=path.dirname(entry),manifest;
for(let i=0;i<5;i++){const file=path.join(dir,'package.json');if(fs.existsSync(file)){const p=JSON.parse(fs.readFileSync(file));if(p.name==='cloakbrowser'){manifest=p;break}}dir=path.dirname(dir)}
assert.equal(manifest?.version,'0.6.4');
const {launch}=await import(pathToFileURL(entry));
const {base,ids,output,sqlite}=JSON.parse(process.env.TRACE_P0_FIXTURE);
fs.mkdirSync(output,{recursive:true});
const report={sdk:manifest.version,headed:true,sqlite,scope:'U3/U4 real Store -> HTTP/SSE -> application DOM; no provider quality, scheduling or B9 latency claim',viewports:[],errors:[],limitations:['Fixed CloakBrowser suppresses pageerror events; empty events do not prove zero exceptions','Fixture worker records test presentation, not actual worker execution','Native browser zoom is accepted only when both devicePixelRatio and CSS viewport change; CSS zoom/deviceScaleFactor are never substitutes']};
const stage=name=>{const line=new Date().toISOString()+' '+name+String.fromCharCode(10);process.stderr.write(line);fs.appendFileSync(path.join(output,'p0-browser-stages.log'),line)};
const browser=await launch({headless:false,args:['--no-sandbox','--window-size=1440,1000']});
const contexts=[];let current;
const frame=page=>page.evaluate(()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve))));
const getTraffic=async()=>{const response=await fetch(base+'/_p0/traffic');assert.ok(response.ok);return response.json()};
async function instrument(context){
 await context.addInitScript(()=>{
  window.__p0={frames:[],events:[],calls:[],errors:[],unhandled:[]};
  addEventListener('error',e=>window.__p0.errors.push(e.message));
  addEventListener('unhandledrejection',e=>window.__p0.unhandled.push(String(e.reason)));
  const Native=window.EventSource,originalFetch=window.fetch.bind(window);
  window.fetch=(input,...args)=>{window.__p0.calls.push({kind:'fetch',url:typeof input==='string'?input:input.url});return originalFetch(input,...args)};
  window.EventSource=class extends Native{constructor(...args){super(...args);window.__p0.calls.push({kind:'sse',url:String(args[0])});window.__p0.events.push({kind:'subscribe',url:String(args[0]),at:performance.now()});this.addEventListener('open',()=>window.__p0.events.push({kind:'open',url:String(args[0]),at:performance.now()}));this.addEventListener('trace',event=>{for(const hit of event.data.matchAll(/P0FRAME_([a-zA-Z0-9-]+)_([0-9]+)/g))window.__p0.frames.push({batch:hit[1],index:Number(hit[2]),sequence:Number(event.lastEventId)})})}};
 });
}
async function waitWorkspace(page,id){
 await page.waitForSelector('.conversation [data-message-id]');
 await page.waitForFunction(async expected=>{const {state}=await import('/app.js');return state.snap?.run.id===expected&&!state.runLoading},id);
 await page.waitForFunction(expected=>window.__p0.events.some(e=>e.kind==='open'&&e.url.includes('/'+expected+'/events')),id);
 await frame(page);
}
async function overlayState(page){
 return page.evaluate(()=>{
  const describe=el=>el?{tag:el.tagName,id:el.id,class:typeof el.className==='string'?el.className:''}:null;
  const box=selector=>{const el=document.querySelector(selector),r=el.getBoundingClientRect(),s=getComputedStyle(el);return {...describe(el),top:r.top,bottom:r.bottom,left:r.left,right:r.right,position:s.position,cssTop:s.top,focused:el===document.activeElement}};
  const pane=document.querySelector('.conversation'),r=pane.getBoundingClientRect(),nav=document.querySelector('.workspace-dialogue-head')?.getBoundingClientRect(),tabs=document.querySelector('.task-tabs').getBoundingClientRect();
  const top=Math.max(0,r.top,nav?.bottom||0,tabs.top<=1?tabs.bottom:0)+8,bottom=Math.min(innerHeight,r.bottom)-8,left=Math.max(0,r.left)+16,right=Math.min(innerWidth,r.right)-16,hits=[];
  if(bottom>top&&right>left)for(const ratio of [.1,.5,.9])for(const xRatio of [.25,.75]){const x=left+(right-left)*xRatio,y=top+(bottom-top)*ratio,hit=document.elementFromPoint(x,y);hits.push({x,y,inside:hit===pane||pane.contains(hit),hit:describe(hit)})}
  const skip=document.querySelector('.skip');let range=document.createRange();range.selectNodeContents(skip);const sr=range.getBoundingClientRect(),skipHit=sr.bottom>0&&sr.top<innerHeight?document.elementFromPoint(Math.max(0,Math.min(innerWidth-1,sr.left+sr.width/2)),Math.max(0,Math.min(innerHeight-1,sr.top+sr.height/2))):null;
  return {scroll:{x:scrollX,y:scrollY,workspace:document.querySelector('#workspace').scrollTop},active:describe(document.activeElement),skip:box('.skip'),skipText:{top:sr.top,bottom:sr.bottom,visible:sr.bottom>0&&sr.top<innerHeight},skipHit:describe(skipHit),tabs:box('.task-tabs'),pane:{top:r.top,bottom:r.bottom},hits};
 });
}
async function layout(page,label){
 const preview=page.locator('.exploration-preview');
 await preview.waitFor({state:'visible'});
 const initialGraph=await page.evaluate(()=>{
  const pane=document.querySelector('.conversation'),head=document.querySelector('.workspace-dialogue-head'),canvas=document.querySelector('.exploration-preview .graph-shell'),inspector=document.querySelector('.task-inspector');
  const hr=head.getBoundingClientRect(),bounds=document.querySelector('#workspace').getBoundingClientRect();
  const visible=el=>{const r=el.getBoundingClientRect(),top=Math.max(0,bounds.top,hr.bottom,r.top),bottom=Math.min(innerHeight,bounds.bottom,r.bottom);return {height:Math.max(0,bottom-top),hit:bottom>top&&el.contains(document.elementFromPoint(r.left+r.width/2,(top+bottom)/2))}};
  return {width:innerWidth,height:innerHeight,head:hr.height,conversation:visible(pane),canvas:visible(canvas),editorRight:inspector.getBoundingClientRect().left>=pane.getBoundingClientRect().right};
 });
 if(initialGraph.width>=1024&&initialGraph.height>=900){
  assert.ok(initialGraph.head<=88&&initialGraph.editorRight,label+': reader header or inspector misplaced: '+JSON.stringify(initialGraph));
  assert.ok(initialGraph.conversation.height>=300&&initialGraph.conversation.hit,label+': insufficient visible transcript: '+JSON.stringify(initialGraph));
  // The enlarged transcript may place the graph below the fold; reachability is checked after scrolling.
 }
 await preview.scrollIntoViewIfNeeded();await frame(page);
 assert.ok(await preview.evaluate(el=>{const r=el.getBoundingClientRect();return Math.min(r.bottom,innerHeight)-Math.max(r.top,0)>=100}),label+': exploration preview is not reachable');
 assert.ok(await preview.locator('svg a').count()>=10,label+': inline graph nodes missing');
 assert.equal(await preview.locator('a.child_report').count(),1,label+': child report absent from workspace graph');
 assert.equal(await preview.locator('a.source').count(),1,label+': fixed source absent from workspace graph');
 assert.ok(await preview.locator('.exploration-edge').count()>=9,label+': persisted graph edges missing');
 const graphBox=await preview.locator('.graph-shell').evaluate(el=>({height:el.getBoundingClientRect().height,width:el.clientWidth,scrollWidth:el.scrollWidth,overflow:getComputedStyle(el).overflow}));
 assert.ok(graphBox.height>=200&&graphBox.height<=245,label+': preview height is unstable');
 assert.equal(graphBox.overflow,'auto',label+': graph has no independent scroll owner');
 const graphClick={label};(report.graphClicks??=[]).push(graphClick);
 const childReport=preview.locator('a.child_report');
 await childReport.evaluate(el=>{
  window.__p0.graphPointer=null;
  el.addEventListener('pointerdown',event=>{window.__p0.graphPointer={x:event.clientX,y:event.clientY,onNode:el.contains(document.elementFromPoint(event.clientX,event.clientY))}},{once:true});
 });
 try{await childReport.click();graphClick.clicked=true}
 finally{graphClick.geometry=await page.evaluate(()=>{
  const box=selector=>{const r=document.querySelector(selector).getBoundingClientRect();return {top:r.top,bottom:r.bottom,left:r.left,right:r.right,height:r.height}};
  const workspace=box('#workspace'),tabs=box('.task-tabs'),head=box('.workspace-dialogue-head'),pane=box('.conversation');
  return {viewport:{width:innerWidth,height:innerHeight,dpr:devicePixelRatio},workspace,tabs,head,pane,graph:box('.exploration-preview .graph-shell'),pointer:window.__p0.graphPointer,scrollPadding:getComputedStyle(document.querySelector('#workspace')).scrollPaddingTop};
 })}
 assert.ok(graphClick.geometry.pointer?.onNode,label+': native pointer did not hit the graph node');
 await page.waitForSelector('.exploration-detail');
 assert.match(await page.locator('.exploration-detail').textContent(),/P0 inline child report/);
 await page.keyboard.press('Escape');
 const result=await page.evaluate(()=>{
  const box=selector=>{const el=document.querySelector(selector),r=el.getBoundingClientRect(),s=getComputedStyle(el);return {x:r.x,y:r.y,left:r.left,right:r.right,top:r.top,bottom:r.bottom,width:r.width,height:r.height,position:s.position,overflow:s.overflowY,scrollHeight:el.scrollHeight,clientHeight:el.clientHeight}};
  const pane=document.querySelector('.conversation'),composer=document.querySelector('.workspace-composer'),inspector=document.querySelector('.workspace-secondary');
  const overflows=[...document.querySelectorAll('body *')].map(el=>{const r=el.getBoundingClientRect(),s=getComputedStyle(el);return {tag:el.tagName,id:el.id,class:typeof el.className==='string'?el.className:'',left:r.left,right:r.right,width:r.width,height:r.height,overflowX:s.overflowX,position:s.position,minWidth:s.minWidth,display:s.display}}).filter(r=>r.width&&r.height&&(r.right>innerWidth+3||r.left< -3)).slice(0,40);
  const panel=pane.closest('.workspace-panel'),column=pane.closest('.workspace-column'),graph=document.querySelector('.task-exploration');
  return {viewport:{width:innerWidth,height:innerHeight,dpr:devicePixelRatio,documentWidth:document.documentElement.scrollWidth},masthead:box('.masthead'),rail:box('.rail'),conversation:box('.conversation'),composer:box('.workspace-composer'),secondary:box('.workspace-secondary'),overflows,ownership:{editorInInspector:composer.parentElement.matches('.task-inspector')&&composer.contains(document.getElementById('message-form'))&&composer.contains(document.getElementById('run-mailbox')),readerOnly:!panel.querySelector('.workspace-dialogue-head').querySelector('form,textarea,input,select,#run-mailbox')&&!!panel.querySelector('.workspace-dialogue-head>#workspace-notice>#notice')&&!!panel.querySelector('.workspace-dialogue-head>.conversation-pages'),graphAfterConversation:graph.parentElement===column&&!!(pane.compareDocumentPosition(graph)&Node.DOCUMENT_POSITION_FOLLOWING),statusSeparate:composer.nextElementSibling===inspector},messages:pane.querySelectorAll('[data-message-id]').length};
 });
 result.initialGraph=initialGraph;
 report.layoutChecks??=[];report.layoutChecks.push({label,...result});
 const overlaps=(a,b)=>Math.min(a.right,b.right)-Math.max(a.left,b.left)>2&&Math.min(a.bottom,b.bottom)-Math.max(a.top,b.top)>2;
 for(const [key,value] of Object.entries(result.ownership))assert.ok(value,label+': workspace ownership '+key);
 assert.ok(!overlaps(result.conversation,result.composer),label+': composer covers conversation');
 assert.ok(!overlaps(result.composer,result.secondary),label+': secondary status covers composer');
 for(const key of ['conversation','composer','secondary']){const r=result[key];assert.ok(r.width>100&&r.height>40,label+': collapsed '+key);assert.ok(!['absolute','fixed'].includes(r.position),label+': overlay '+key)}
 assert.ok(['auto','scroll'].includes(result.conversation.overflow),label+': missing conversation scroll owner');
 for(const key of ['composer','secondary'])assert.ok(['auto','scroll'].includes(result[key].overflow),label+': missing independent scroll owner '+key);
 assert.ok(result.conversation.scrollHeight>result.conversation.clientHeight+200,label+': no scrollable history');
 assert.ok(result.messages>0&&result.messages<=40,label+': unbounded message window');
 assert.ok(result.viewport.documentWidth<=result.viewport.width+3,label+': document horizontal overflow '+JSON.stringify(result.viewport));
 if(result.viewport.width<=700){assert.ok(result.rail.height<120,label+': mobile navigation retained desktop height');assert.ok(result.masthead.height<220,label+': mobile header is not compact')}
 // The editor stacks below the graph on phones. Scroll to it as a user would.
 if(result.viewport.width<=900)await page.locator('#message-content').scrollIntoViewIfNeeded();
 await frame(page);
 const hit=await page.locator('#message-content').evaluate(el=>{const r=el.getBoundingClientRect(),x=Math.min(innerWidth-1,Math.max(0,r.x+r.width/2)),y=Math.min(innerHeight-1,Math.max(0,r.y+r.height/2)),at=document.elementFromPoint(x,y);return at===el||el.contains(at)});
 assert.ok(hit,label+': composer textarea blocked by overlay');
 result.overlayAfterComposerScroll=await overlayState(page);
 await page.screenshot({path:path.join(output,'p0-'+label+'-composer-viewport.png')});
 await page.locator('.conversation').scrollIntoViewIfNeeded();await frame(page);
 result.overlayConversation=await overlayState(page);
 assert.ok(result.overlayConversation.hits.length===6&&result.overlayConversation.hits.every(hit=>hit.inside),label+': live conversation is covered: '+JSON.stringify(result.overlayConversation));
 assert.ok(result.overlayConversation.skip.focused||!result.overlayConversation.skipText.visible,label+': unfocused skip link is visible in viewport: '+JSON.stringify(result.overlayConversation));
 await page.screenshot({path:path.join(output,'p0-'+label+'-conversation-viewport.png')});
 // Full-page capture after an implicit element scroll paints fixed/sticky
 // controls at the scrolled document offset. Normalize only capture scroll.
 await page.evaluate(()=>{window.scrollTo(0,0);document.querySelector('#workspace').scrollTop=0});await frame(page);
 result.overlayAtDocumentTop=await overlayState(page);
 await page.screenshot({path:path.join(output,'p0-'+label+'.png'),fullPage:true});
 return result;
}
async function middleAnchor(page){
 await page.locator('.conversation').evaluate(el=>el.scrollTop=Math.floor((el.scrollHeight-el.clientHeight)*.45));await frame(page);
 return page.evaluate(()=>{const pane=document.querySelector('.conversation'),p=pane.getBoundingClientRect(),message=[...pane.querySelectorAll('[data-message-id]')].find(el=>el.getBoundingClientRect().bottom>p.top+20);return {id:message.dataset.messageId,offset:message.getBoundingClientRect().top-p.top,top:pane.scrollTop,gap:pane.scrollHeight-pane.clientHeight-pane.scrollTop}});
}
async function inlineExpansion(page,id){
 const preview=page.locator('.exploration-preview'),pane=preview.locator('.graph-shell');
 await preview.scrollIntoViewIfNeeded();
 const task=preview.locator('a.child_report').first();
 await task.focus();
 const before=await pane.evaluate(el=>{el.scrollTop=el.scrollHeight;el.scrollLeft=180;return {top:el.scrollTop,left:el.scrollLeft,height:el.getBoundingClientRect().height,focused:document.activeElement.dataset.id}});
 const response=await fetch(base+'/_p0/report?id='+id,{method:'POST'});assert.equal(response.status,204);
 await page.waitForFunction(()=>document.querySelectorAll('.exploration-preview a.child_report').length===2);await frame(page);
 const after=await pane.evaluate(el=>({top:el.scrollTop,left:el.scrollLeft,height:el.getBoundingClientRect().height,focused:document.activeElement.dataset.id}));
 assert.deepEqual(after,before,'new report changed graph viewport or focused identity');
 assert.equal(await preview.locator('a.source').count(),2,'new report did not expand a fixed source node');
 await preview.locator('a.child_report').last().click();
 assert.match(await page.locator('.exploration-detail').textContent(),/P0 second inline report/);
 await page.keyboard.press('Escape');
 return {before,after,reports:2,sources:2};
}
async function stream(page,id,batch){
 const response=await fetch(base+'/_p0/stream?id='+id+'&batch='+batch,{method:'POST'});assert.equal(response.status,200,await response.text());
 await page.waitForFunction(batch=>new Set(window.__p0.frames.filter(f=>f.batch===batch).map(f=>f.index)).size===8,batch);
 await page.waitForFunction(batch=>document.querySelector('.conversation')?.textContent.includes('P0FRAME_'+batch+'_7'),batch);await frame(page);
 const frames=await page.evaluate(batch=>window.__p0.frames.filter(f=>f.batch===batch),batch);
 assert.equal(frames.length,8,'SSE duplicated source frames');for(let i=0;i<8;i++){assert.equal(frames[i].index,i);if(i)assert.ok(frames[i].sequence>frames[i-1].sequence,'nonmonotonic SSE')}
 return frames;
}
async function anchorAndDraft(page,id,label){
 const draft='P0 unsent '+label+' 中文补充草稿';
 await page.locator('#message-content').fill(draft);
 const before=await middleAnchor(page);assert.ok(before.top>0&&before.gap>100);
 const frames=await stream(page,id,label);
 const after=await page.evaluate(id=>{const pane=document.querySelector('.conversation'),message=pane.querySelector('[data-message-id="'+id+'"]');return {found:!!message,offset:message?.getBoundingClientRect().top-pane.getBoundingClientRect().top,top:pane.scrollTop,gap:pane.scrollHeight-pane.clientHeight-pane.scrollTop}},before.id);
 assert.ok(after.found,'reading anchor evicted by streaming');assert.ok(Math.abs(after.offset-before.offset)<=3,'stream moved middle reading anchor: '+JSON.stringify({before,after}));assert.ok(after.gap>100,'stream forced follow-to-bottom');
 assert.equal(await page.locator('#message-content').inputValue(),draft);
 const latest={label},readLatest=()=>page.evaluate(async()=>{const {state}=await import('/app.js'),pane=document.querySelector('[data-scroll=conversation]'),button=document.querySelector('[data-action=latest][data-scroll-target=conversation]');return {run:state.snap?.run.id,historyWindow:state.snap?.historyWindow,hasMoreAfter:state.snap?.messages_page?.has_more_after,loading:state.messagesLoading,error:state.messagesError,busy:button?.getAttribute('aria-busy'),top:pane?.scrollTop,height:pane?.scrollHeight,client:pane?.clientHeight,gap:pane?pane.scrollHeight-pane.clientHeight-pane.scrollTop:null,viewport:{width:innerWidth,height:innerHeight,dpr:devicePixelRatio}}});
 (report.latestChecks??=[]).push(latest);latest.before=await readLatest();
 await page.locator('[data-action=latest][data-scroll-target=conversation]').click();
 // The real click handler can load the latest message window before its final
 // reconciliation; two animation frames alone do not establish its completion.
 try{await page.waitForFunction(async id=>{const {state}=await import('/app.js'),pane=document.querySelector('[data-scroll=conversation]'),button=document.querySelector('[data-action=latest][data-scroll-target=conversation]');return state.snap?.run.id===id&&!state.messagesLoading&&!state.snap.historyWindow&&!state.snap.messages_page?.has_more_after&&button?.getAttribute('aria-busy')!=='true'&&pane&&pane.scrollHeight-pane.clientHeight-pane.scrollTop<5},id,{timeout:15000});await frame(page)}finally{latest.after=await readLatest()}
 const gap=await page.locator('.conversation').evaluate(el=>el.scrollHeight-el.clientHeight-el.scrollTop);assert.ok(gap<5,'explicit latest did not reach live edge');
 const stable=await page.evaluate(async()=>{const rows=[];for(let i=0;i<8;i++){await new Promise(requestAnimationFrame);const el=document.querySelector('.conversation');rows.push({top:el.scrollTop,height:el.scrollHeight,client:el.clientHeight,gap:el.scrollHeight-el.clientHeight-el.scrollTop})}return rows});
 latest.stable=stable;assert.ok(stable.every(row=>row.gap<5),'explicit latest moved away after layout: '+JSON.stringify(stable));
 return {before,after,sourceFrames:frames,explicitLatestGap:gap,draftPreserved:true};
}
async function paging(page){
 const draft=await page.locator('#message-content').inputValue(),windows=[];
 for(let i=0;i<2;i++){
  const button=page.locator('[data-action=messages-page][data-before]'),before=await button.getAttribute('data-before');await button.click();
  await page.waitForFunction(previous=>document.querySelector('[data-action=messages-page][data-before]')?.dataset.before!==previous,before);
  const rows=await page.locator('.conversation [data-message-id]').evaluateAll(els=>els.map(el=>el.dataset.messageId));assert.ok(rows.length>0&&rows.length<=40);windows.push(rows);assert.equal(await page.locator('#message-content').inputValue(),draft);
 }
 assert.notDeepEqual(windows[0],windows[1],'paging repeated the same window');
 await page.locator('[data-action=messages-latest]').click();await page.waitForSelector('[data-message-id=p0-history-119]');
 assert.equal(await page.locator('#message-content').inputValue(),draft);
 return {windows:windows.map(rows=>({first:rows[0],last:rows.at(-1),count:rows.length})),draftPreserved:true};
}
async function workerAndPlan(page){
 const draft=await page.locator('#message-content').inputValue();
 await page.locator('#task-tab-planner').click();await page.waitForSelector('[data-task-group=pending]');
 const result=await page.evaluate(()=>{
  const ids=selector=>[...document.querySelectorAll(selector)].map(el=>el.dataset.task||el.dataset.dispatchTask);
  const cards=[...document.querySelectorAll('.worker-card')];
  return {pending:ids('[data-task-group=pending] [data-action=task-dispatch]'),workers:cards.map(el=>({id:el.querySelector('[data-worker]')?.dataset.worker,history:!!el.closest('#worker-history')})),historyOpen:document.querySelector('#worker-history')?.open,planHistoryOpen:document.querySelector('#plan-history')?.open,dispatch:ids('[data-dispatch-task]')};
 });
 assert.deepEqual(result.pending.sort(),['todo-capability','todo-dependent','todo-ready']);
 assert.deepEqual(result.workers.slice(0,3).map(w=>w.id),['worker-running','worker-starting','worker-cancelling']);
 assert.equal(result.workers.find(w=>w.id==='worker-completed')?.history,true);assert.equal(result.historyOpen,false);assert.equal(result.planHistoryOpen,false);assert.ok(result.dispatch.includes('dispatch-pending'));
 await page.locator('#task-tab-activity').click();await page.waitForSelector('#message-content');
 assert.equal(await page.locator('#message-content').inputValue(),draft,'tab switch lost supplementary draft');
 return result;
}
async function restoredHistorySubscribesFirst(page,id,other,diagnostics){
 const readState=()=>page.evaluate(async()=>{const {state}=await import('/app.js');return {selected:state.selected,run:state.snap?.run.id,cursor:state.messageCursor,window:state.snap?.messages.map(m=>m.id),historyWindow:state.snap?.historyWindow,messagesPage:state.snap?.messages_page,messagesLoading:state.messagesLoading,messagesError:state.messagesError,runError:state.runError,historyDetail:state.historyDetail,dom:[...document.querySelectorAll('.conversation [data-message-id]')].map(el=>el.dataset.messageId)}});
 const draft=await page.locator('#message-content').inputValue();
 await page.locator('[data-action=messages-page][data-before]').click();
 await page.waitForFunction(async id=>{const {state}=await import('/app.js');return state.selected===id&&!!state.messageCursor&&!state.messagesLoading},id);
 const saved=diagnostics.saved=await readState();
 assert.ok(saved.historyWindow&&saved.messagesPage.has_more_after&&saved.cursor?.around,'fixture did not establish an older cursor: '+JSON.stringify(saved));
 await page.evaluate(other=>location.hash='#history/'+other,other);await waitWorkspace(page,other);
 const mark=(await getTraffic()).length,clientMark=await page.evaluate(()=>window.__p0.calls.length),openMark=await page.evaluate(id=>window.__p0.events.filter(e=>e.kind==='open'&&e.url.includes('/'+id+'/events')).length,id);let release;const gate=new Promise(resolve=>release=resolve);
 let historySeen=false,observeHistory;const historyRequested=new Promise(resolve=>observeHistory=resolve);
 const route=async request=>{diagnostics.requestURL=request.request().url();historySeen=true;observeHistory();await gate;await request.continue().catch(error=>diagnostics.routeError=String(error))};
 await page.route('**/api/runs/'+id+'/messages?**',route);
 const responsePending=page.waitForResponse(response=>{const url=new URL(response.url());return url.pathname==='/api/runs/'+id+'/messages'&&url.searchParams.get('around')===saved.cursor.around},{timeout:20000}).catch(error=>({error:String(error)}));
 try{
  await page.evaluate(id=>location.hash='#history/'+id,id);
  await page.waitForFunction(async id=>{const {state}=await import('/app.js');return state.selected===id&&state.messagesLoading},id);
  await page.waitForFunction(({id,openMark})=>window.__p0.events.filter(e=>e.kind==='open'&&e.url.includes('/'+id+'/events')).length>openMark,{id,openMark});
  await Promise.race([historyRequested,new Promise((_,reject)=>setTimeout(()=>reject(Error('restored cursor never requested history')),10000))]);
  assert.ok(historySeen,'restored cursor never requested history');
  assert.equal(new URL(diagnostics.requestURL).searchParams.get('around'),saved.cursor.around,'restored request used a different cursor');
  const calls=await page.evaluate(mark=>window.__p0.calls.slice(mark),clientMark),sseCall=calls.findIndex(c=>c.kind==='sse'&&c.url.includes('/'+id+'/events')),historyCall=calls.findIndex(c=>c.kind==='fetch'&&c.url.includes('/'+id+'/messages?'));
  diagnostics.clientCallOrder=calls;
  assert.ok(sseCall>=0&&historyCall>sseCall,'history initiated before SSE subscription: '+JSON.stringify(calls));
  const frames=await stream(page,id,'history-gated');
  diagnostics.streamFramesWhileHistoryBlocked=frames.length;diagnostics.beforeRelease=await readState();
  assert.ok(diagnostics.beforeRelease.messagesLoading,'history load ended while its response was gated: '+JSON.stringify(diagnostics.beforeRelease));
  stage('release restored history');release();
  const response=await responsePending;assert.ok(!response.error,'restored history response missing: '+JSON.stringify(response));
  const payload=await response.json();
  diagnostics.response={url:response.url(),status:response.status(),run:payload.run?.id,messagesPage:payload.messages_page,window:payload.messages?.map(m=>m.id),error:payload.error};
  assert.ok(response.ok(),'restored history HTTP failure: '+JSON.stringify(diagnostics.response));
  assert.ok(payload.run?.id===id&&payload.messages_page?.has_more_after&&payload.messages?.some(m=>m.id===saved.cursor.around),'restored history response has wrong window: '+JSON.stringify(diagnostics.response));
  stage('restored history response received');
  await page.waitForFunction(async({id,around})=>{const {state}=await import('/app.js');return state.selected===id&&state.snap?.run.id===id&&!state.messagesLoading&&(state.messagesError||state.runError||(state.snap.historyWindow&&state.snap.messages.some(m=>m.id===around)&&document.querySelector('.conversation [data-message-id="'+around+'"]')))},{id,around:saved.cursor.around});
  const restored=diagnostics.restored=await readState();
  assert.ok(!restored.messagesError&&!restored.runError&&restored.historyWindow&&restored.window.includes(saved.cursor.around)&&restored.dom.includes(saved.cursor.around),'saved historical cursor not restored: '+JSON.stringify(restored));
  assert.equal(await page.locator('#message-content').inputValue(),draft,'run switch lost supplementary draft');
  const traffic=(await getTraffic()).slice(mark).filter(r=>r.path.startsWith('/api/runs/'+id));
  const sse=traffic.find(r=>r.path.endsWith('/events')),history=traffic.find(r=>r.path.endsWith('/messages'));
  assert.ok(sse&&history&&sse.index<history.index,'server observed history before SSE: '+JSON.stringify(traffic));
  Object.assign(diagnostics,{sseIndex:sse.index,historyIndex:history.index,traffic});stage('restored history committed');
 }finally{diagnostics.finalState=await readState().catch(error=>({error:String(error)}));diagnostics.traffic=(await getTraffic()).slice(mark).filter(r=>r.path.startsWith('/api/runs/'+id));release();await page.unroute('**/api/runs/'+id+'/messages?**',route)}
}
// XTest sends OS keyboard events to the real headed browser, unlike renderer
// keyboard emulation. Existing libX11/libXtst only; no install or browser change.
const nativeKeySource=String.raw`import ctypes as c,json,sys,time,os,pathlib
x=c.CDLL('libX11.so.6');xt=c.CDLL('libXtst.so.6');D=c.c_void_p;W=c.c_ulong
class Attributes(c.Structure):
 _fields_=[('x',c.c_int),('y',c.c_int),('width',c.c_int),('height',c.c_int),('border_width',c.c_int),('depth',c.c_int),('visual',D),('root',W),('window_class',c.c_int),('bit_gravity',c.c_int),('win_gravity',c.c_int),('backing_store',c.c_int),('backing_planes',W),('backing_pixel',W),('save_under',c.c_int),('colormap',W),('map_installed',c.c_int),('map_state',c.c_int),('all_event_masks',c.c_long),('your_event_mask',c.c_long),('do_not_propagate_mask',c.c_long),('override_redirect',c.c_int),('screen',D)]
def bind(lib,name,args,result):
 f=getattr(lib,name);f.argtypes=args;f.restype=result;return f
bind(x,'XOpenDisplay',[c.c_char_p],D);bind(x,'XDefaultRootWindow',[D],W)
bind(x,'XFetchName',[D,W,c.POINTER(c.c_char_p)],c.c_int)
bind(x,'XInternAtom',[D,c.c_char_p,c.c_int],W)
bind(x,'XGetWindowProperty',[D,W,W,c.c_long,c.c_long,c.c_int,W,c.POINTER(W),c.POINTER(c.c_int),c.POINTER(W),c.POINTER(W),c.POINTER(D)],c.c_int)
bind(x,'XGetWindowAttributes',[D,W,c.POINTER(Attributes)],c.c_int)
bind(x,'XGetInputFocus',[D,c.POINTER(W),c.POINTER(c.c_int)],c.c_int)
bind(x,'XQueryTree',[D,W,c.POINTER(W),c.POINTER(W),c.POINTER(c.POINTER(W)),c.POINTER(c.c_uint)],c.c_int)
bind(x,'XFree',[D],c.c_int);bind(x,'XRaiseWindow',[D,W],c.c_int)
bind(x,'XSetInputFocus',[D,W,c.c_int,W],c.c_int);bind(x,'XSync',[D,c.c_int],c.c_int)
bind(x,'XStringToKeysym',[c.c_char_p],W);bind(x,'XKeysymToKeycode',[D,W],c.c_ubyte)
bind(x,'XCloseDisplay',[D],c.c_int);bind(xt,'XTestFakeKeyEvent',[D,c.c_uint,c.c_int,W],c.c_int)
d=x.XOpenDisplay(None);out={'key':sys.argv[2],'display':os.environ.get('DISPLAY'),'windows':[],'browserProcesses':[]}
def prop(w,name):
 atom=x.XInternAtom(d,name.encode(),1)
 if not atom:return None
 actual=W();fmt=c.c_int();count=W();after=W();data=D()
 if x.XGetWindowProperty(d,w,atom,0,4096,0,0,c.byref(actual),c.byref(fmt),c.byref(count),c.byref(after),c.byref(data)) or not data:return None
 try:
  if fmt.value==8:return c.string_at(data,count.value).decode('utf-8','replace')
  if fmt.value==32:return [c.cast(data,c.POINTER(W))[i] for i in range(count.value)]
 finally:x.XFree(data)
def find(w,depth=0):
 title=c.c_char_p();name=''
 if x.XFetchName(d,w,c.byref(title)) and title:
  name=title.value.decode('utf-8','replace');x.XFree(c.cast(title,D))
 a=Attributes();x.XGetWindowAttributes(d,w,c.byref(a));pid=prop(w,'_NET_WM_PID')
 row={'window':w,'title':name,'netName':prop(w,'_NET_WM_NAME'),'wmClass':prop(w,'WM_CLASS'),'pid':pid[0] if pid else None,'width':a.width,'height':a.height,'viewable':a.map_state==2,'overrideRedirect':bool(a.override_redirect),'depth':depth}
 found=[row]
 if depth<8:
  root=W();parent=W();children=c.POINTER(W)();count=c.c_uint()
  if x.XQueryTree(d,w,c.byref(root),c.byref(parent),c.byref(children),c.byref(count)):
   ids=[children[i] for i in range(count.value)]
   if children:x.XFree(c.cast(children,D))
   for child in ids:found.extend(find(child,depth+1))
 return found
try:
 if not d:raise RuntimeError('XOpenDisplay failed')
 parent=int(sys.argv[1]);parents={};processes=[]
 for p in pathlib.Path('/proc').iterdir():
  if not p.name.isdigit():continue
  try:
   pid=int(p.name);status=(p/'status').read_text();parents[pid]=int(next(line for line in status.splitlines() if line.startswith('PPid:')).split()[1])
   if os.path.samefile(p/'exe',os.environ['CLOAKBROWSER_BINARY_PATH']):processes.append(pid)
  except (OSError,ValueError,StopIteration):pass
 for pid in processes:
  chain=[];current=pid
  while current in parents and current not in chain and current!=parent:chain.append(current);current=parents[current]
  if current==parent:out['browserProcesses'].append({'pid':pid,'parentChain':chain})
 owned={p['pid'] for p in out['browserProcesses']};focus=W();revert=c.c_int();x.XGetInputFocus(d,c.byref(focus),c.byref(revert));out['originalFocus']={'window':focus.value,'revert':revert.value}
 for attempt in range(20):
  out['windows']=find(x.XDefaultRootWindow(d))
  matches=[w for w in out['windows'] if w['pid'] in owned and w['viewable'] and not w['overrideRedirect'] and w['width']>200 and w['height']>200 and any(s in (w['wmClass'] or '').lower() for s in ['chrome','chromium','cloak'])]
  if matches:break
  time.sleep(.05)
 if len(matches)!=1:raise RuntimeError('expected unique mapped browser PID/class window; found '+str(len(matches)))
 selected=matches[0];window=selected['window'];out['selected']=selected;out['selection']='mapped non-override window with browser WM_CLASS and _NET_WM_PID owned by this Node process and fixed executable'
 x.XRaiseWindow(d,window);x.XSetInputFocus(d,window,1,0);x.XSync(d,0);time.sleep(.1)
 control=x.XKeysymToKeycode(d,x.XStringToKeysym(b'Control_L'));key=x.XKeysymToKeycode(d,x.XStringToKeysym(sys.argv[2].encode()))
 if not control or not key:raise RuntimeError('missing native keycode')
 try:
  if not xt.XTestFakeKeyEvent(d,control,1,0):raise RuntimeError('XTest control failed')
  xt.XTestFakeKeyEvent(d,key,1,0);xt.XTestFakeKeyEvent(d,key,0,0)
 finally:
  xt.XTestFakeKeyEvent(d,control,0,0);x.XSync(d,0)
 out.update(controlCode=control,keyCode=key,sent=True)
except Exception as error:out['error']=str(error)
finally:
 if d:x.XCloseDisplay(d)
print(json.dumps(out))
`;
async function nativeZoom(page){
 await page.bringToFront();await page.locator('.workspace-heading').click();
 const result={steps:[],nativeKeys:[],verified:false,method:'X11 XTest Control_L+0 then five Control_L+equal native shortcuts; no CSS zoom/deviceScaleFactor/CDP emulation'};
 const key=value=>{const output=execFileSync('python3',['-c',nativeKeySource,String(process.pid),value],{encoding:'utf8',timeout:8000}),observation=JSON.parse(output);result.nativeKeys.push(observation);assert.ok(observation.sent,'native keyboard: '+(observation.error||'no send confirmation'))};
 const measure=()=>page.evaluate(()=>({dpr:devicePixelRatio,width:innerWidth,height:innerHeight,visualScale:visualViewport?.scale}));
 try{
  key('0');await page.waitForTimeout(150);await frame(page);result.baseline=await measure();
  for(let i=0;i<5;i++){key('equal');await page.waitForTimeout(150);result.steps.push(await measure())}
  result.actual=result.steps.at(-1);result.verified=Math.abs(result.actual.dpr/result.baseline.dpr-2)<.06&&Math.abs(result.actual.width/result.baseline.width-.5)<.06;
 }catch(error){result.error=String(error)}
 return result;
}
try{
 if(process.argv[2]==='workspace'){
  report.scope='Focused workspace UI: real Store/API and browser input, no tool execution';
  const context=await browser.newContext({viewport:{width:1440,height:1000}});contexts.push(context);
  context.setDefaultTimeout(10000);await instrument(context);
  const page=current=await context.newPage();
  report.interactions={};
  await workspaceInteractions({page,base,id:ids[0],output,waitWorkspace,frame,stage,result:report.interactions});
  report.runtime=await page.evaluate(()=>({errors:window.__p0.errors,unhandled:window.__p0.unhandled}));
  assert.deepEqual(report.runtime,{errors:[],unhandled:[]});
  report.result='PASS';
 }else{
 for(const [index,width] of [1440,1024,390].entries()){
  stage('viewport '+width);const context=await browser.newContext({viewport:{width,height:1000}});context.setDefaultTimeout(15000);context.setDefaultNavigationTimeout(20000);contexts.push(context);await instrument(context);
  const page=current=await context.newPage();page.on('pageerror',error=>report.errors.push(String(error)));
  let release;const gate=new Promise(resolve=>release=resolve);let held=false;
  await page.route('**/api/runs/'+ids[index]+'?view=workspace*',async route=>{if(!held){held=true;await gate}await route.continue()});
  const navigation=page.goto(base+'/#history/'+ids[index]);
  try{await page.waitForSelector('.workspace-loading[aria-busy=true]');await page.waitForSelector('.workspace-loading .workspace-dialogue-head [data-action=latest]:disabled');assert.equal(await page.locator('.workspace-loading form,.workspace-loading textarea').count(),0);release();await navigation;await waitWorkspace(page,ids[index])}finally{release();await page.unroute('**/api/runs/'+ids[index]+'?view=workspace*')}
  const row={width,skeleton:true};report.viewports.push(row);
  row.layout=await layout(page,'width-'+width);stage('layout ready '+width);
  row.streaming=await anchorAndDraft(page,ids[index],'width-'+width);
  stage('stream and anchor ready '+width);
  row.paging=await paging(page);
  stage('paging and draft ready '+width);
  if(index===0){row.inlineExpansion=await inlineExpansion(page,ids[index]);row.workerPlan=await workerAndPlan(page);row.order={};await restoredHistorySubscribesFirst(page,ids[index],ids[3],row.order)}
  row.runtime=await page.evaluate(()=>({errors:window.__p0.errors,unhandled:window.__p0.unhandled}));assert.deepEqual(row.runtime.errors,[]);assert.deepEqual(row.runtime.unhandled,[]);
  await context.close();contexts.pop();current=null;
 }
 stage('native 200 percent zoom');
 // A real window is required: fixed viewport emulation masks native zoom.
 const zoomContext=await browser.newContext({viewport:null});zoomContext.setDefaultTimeout(15000);zoomContext.setDefaultNavigationTimeout(20000);contexts.push(zoomContext);await instrument(zoomContext);
 const zoomPage=current=await zoomContext.newPage();zoomPage.on('pageerror',error=>report.errors.push(String(error)));
 await zoomPage.goto(base+'/#history/'+ids[3]);await waitWorkspace(zoomPage,ids[3]);report.zoom=await nativeZoom(zoomPage);
 if(report.zoom.verified){report.zoom.layout=await layout(zoomPage,'native-zoom-200');report.zoom.streaming=await anchorAndDraft(zoomPage,ids[3],'zoom-200')}
 else report.zoom.uncovered='Native shortcut did not establish 200% browser zoom; no simulated zoom accepted';
 report.zoom.runtime=await zoomPage.evaluate(()=>({errors:window.__p0.errors,unhandled:window.__p0.unhandled}));assert.deepEqual(report.zoom.runtime.errors,[]);assert.deepEqual(report.zoom.runtime.unhandled,[]);
 await zoomContext.close();contexts.pop();current=null;
 assert.deepEqual(report.errors,[]);report.executedChecksPassed=true;report.completeCoverage=report.zoom?.verified===true;report.result=report.completeCoverage?'PASS':'PASS_WITH_UNCOVERED_NATIVE_ZOOM';
 }
}catch(error){report.result='FAIL';report.error=String(error);stage('failed '+error);try{await current?.screenshot({path:path.join(output,'p0-failure.png'),fullPage:true})}catch{}throw error}
finally{
 report.http=await getTraffic().catch(error=>({error:String(error)}));
 fs.writeFileSync(path.join(output,'p0-browser.json'),JSON.stringify(report,null,2));console.log(JSON.stringify(report));
 for(const context of contexts)await context.close().catch(()=>{});await browser.close();
}
