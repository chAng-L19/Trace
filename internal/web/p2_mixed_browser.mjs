// Server-only: TestP2MixedBrowserAcceptance. Timestamp precedes provider.emit.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import {createRequire} from 'node:module';
import {pathToFileURL} from 'node:url';
const require=createRequire(import.meta.url);
const cloakPath=process.env.TRACE_CLOAKBROWSER_NODE_MODULE;
assert.ok(cloakPath,'fixed CloakBrowser module directory required');
assert.equal(JSON.parse(fs.readFileSync(path.join(cloakPath,'package.json'),'utf8')).version,'0.6.4');
const cloak=await import(pathToFileURL(require.resolve(cloakPath)));
assert.ok(process.env.CLOAKBROWSER_BINARY_PATH,'fixed CloakBrowser binary required');
const fixture=JSON.parse(process.env.TRACE_P2_FIXTURE),{base,ids,output}=fixture;
const mode=fixture.mode||'three-pages',singleActive=mode==='single-active';
assert.ok(['three-pages','single-active'].includes(mode),'unknown browser acceptance mode');
const stage=name=>{const line=new Date().toISOString()+' '+name+'\n';process.stderr.write(line);fs.appendFileSync(path.join(output,'mixed-stages.log'),line)};
const stats=values=>{const a=[...values].sort((x,y)=>x-y);return {count:a.length,p50_ms:a[Math.floor((a.length-1)/2)],p95_ms:a[Math.ceil(a.length*.95)-1],max_ms:a.at(-1)}};
stage('launch fixed headed CloakBrowser 0.6.4');
const browser=await cloak.launch({headless:false,args:['--no-sandbox']});
const context=await browser.newContext({viewport:{width:1440,height:1000}}),pages=[],background=[],errors=[],requests=[],report={mode,measurement:singleActive?'fixed ids[0] provider emit -> committed Store -> HTTP SSE -> DOM animation frame; other two runs use HTTP SSE, no DOM claim; three 50k histories + five real workers and original list/history pressure':'provider emit -> committed Store -> HTTP SSE -> DOM animation frame; three 50k histories + five real streaming workers; cold/semantic/history reads retained',fixture};

async function openBackgroundFeed(id){
 const begin=Date.now(),viewResponse=await fetch(base+'/api/runs/'+id+'/messages?limit=40');assert.ok(viewResponse.ok);const view=await viewResponse.json();
 const abort=new AbortController(),result={run:id,initial_http_ms:Date.now()-begin,initial_sequence:view.run.event_seq,sequences:[],samples:[]};
 let resolveDone,rejectDone;const done=new Promise((resolve,reject)=>{resolveDone=resolve;rejectDone=reject});done.catch(()=>{});
 const timer=setTimeout(()=>{result.error='background SSE timed out';rejectDone(Error(result.error));abort.abort()},150000);
 const feed={result,done,abort,task:null,timer};background.push(feed);
 const response=await fetch(base+'/api/runs/'+id+'/events?after='+view.run.event_seq,{signal:abort.signal});assert.equal(response.status,200);assert.match(response.headers.get('content-type')||'',/text\/event-stream/);
 let last=view.run.event_seq;const seen=new Set();
 const consume=frame=>{
  const lines=frame.split('\n').map(line=>line.endsWith('\r')?line.slice(0,-1):line),type=lines.find(line=>line.startsWith('event:'))?.slice(6).trim();if(type!=='trace')return;
  const sse=Date.now(),event=JSON.parse(lines.filter(line=>line.startsWith('data:')).map(line=>line.slice(5).trimStart()).join('\n'));
  const sequence=Number(lines.find(line=>line.startsWith('id:'))?.slice(3).trim());assert.equal(sequence,event.sequence);assert.equal(sequence,last+1,'background SSE sequence gap or replay');last=sequence;result.sequences.push(sequence);
  if(event.type!=='model_stream')return;
  for(const hit of JSON.stringify(event.data).matchAll(/P2FRAME_([a-zA-Z0-9-]+)_(\d+)_(\d+)/g)){
   assert.equal(hit[1],event.data.request_id,'background source request identity');const key=hit[1]+':'+hit[2];assert.ok(!seen.has(key),'duplicate background source frame');seen.add(key);
   const source=Number(hit[3]);result.samples.push({request:hit[1],index:Number(hit[2]),source,sse,latency:sse-source,sequence});
  }
  if(result.samples.length>=60){clearTimeout(timer);resolveDone()}
 };
 feed.task=(async()=>{const reader=response.body.getReader(),decoder=new TextDecoder();let pending='';try{for(;;){const {value,done}=await reader.read();if(done){if(result.samples.length<60)throw Error('background SSE closed before all source frames');break}pending+=decoder.decode(value,{stream:true});let end;while((end=pending.indexOf('\n\n'))>=0){consume(pending.slice(0,end));pending=pending.slice(end+2)}}}catch(error){if(!abort.signal.aborted){result.error=String(error);rejectDone(error)}}finally{reader.releaseLock();clearTimeout(timer)}})();
 stage('background HTTP SSE ready '+id);return feed;
}
try{
 await context.addInitScript(()=>{
  window.__p2={seen:{},received:{},mutated:{},samples:[],maxMessages:0,maxNodes:0};
  // Observe the application's original connection without adding a consumer,
  // changing event delivery, or moving the source/DOM measurement boundaries.
  const NativeEventSource=window.EventSource;
  window.EventSource=class extends NativeEventSource{constructor(...args){super(...args);this.addEventListener('trace',event=>{const received=Date.now();let created;try{const parsed=JSON.parse(event.data),time=Date.parse(parsed.created_at);if(Number.isFinite(time))created=time}catch{}for(const hit of event.data.matchAll(/P2FRAME_([a-zA-Z0-9-]+)_(\d+)_(\d+)/g))window.__p2.received[hit[1]+':'+hit[2]]??={received,created}})}};
  const scan=()=>{
   const pane=document.querySelector('.conversation');if(!pane)return;
   window.__p2.maxMessages=Math.max(window.__p2.maxMessages,pane.querySelectorAll('[data-message-id]').length);
   window.__p2.maxNodes=Math.max(window.__p2.maxNodes,pane.querySelectorAll('*').length);
   for(const hit of pane.textContent.matchAll(/P2FRAME_([a-zA-Z0-9-]+)_(\d+)_(\d+)/g)){
    const key=hit[1]+':'+hit[2];if(window.__p2.seen[key])continue;window.__p2.seen[key]=true;
    const dom=Date.now(),source=Number(hit[3]),receipt=window.__p2.received[key],sse=receipt?.received,event_created=receipt?.created,mutation=window.__p2.mutated[key];
    window.__p2.samples.push({request:hit[1],index:Number(hit[2]),source,dom,latency:dom-source,sse,event_created,mutation,source_to_mutation:mutation===undefined?undefined:mutation-source,sse_to_mutation:mutation===undefined||sse===undefined?undefined:mutation-sse,mutation_to_raf:mutation===undefined?undefined:dom-mutation,source_to_event_created:event_created===undefined?undefined:event_created-source,event_created_to_sse:event_created===undefined||sse===undefined?undefined:sse-event_created,source_to_sse:sse===undefined?undefined:sse-source,sse_to_dom:sse===undefined?undefined:dom-sse,visibility:document.visibilityState});
   }
  };
  let frame=false;new MutationObserver(()=>{
   const mutation=Date.now(),pane=document.querySelector('.conversation');
   if(pane)for(const hit of pane.textContent.matchAll(/P2FRAME_([a-zA-Z0-9-]+)_(\d+)_(\d+)/g))window.__p2.mutated[hit[1]+':'+hit[2]]??=mutation;
   if(frame)return;frame=true;requestAnimationFrame(()=>{frame=false;scan()})
  }).observe(document,{childList:true,subtree:true,characterData:true});
 });
 const enter=[];
 for(const id of singleActive?ids.slice(0,1):ids){stage('enter '+id);const p=await context.newPage();pages.push(p);p.on('pageerror',e=>errors.push(String(e)));p.on('request',r=>requests.push(r.url()));const start=Date.now();await p.goto(base+'/#history/'+id);await p.waitForSelector('.conversation [data-message-id]');enter.push(Date.now()-start);await p.evaluate(async()=>{const {performanceDiagnostics}=await import('/app.js');performanceDiagnostics(true)});stage('entered '+id+' in '+enter.at(-1)+'ms')}
 report.initial_workspace_ms=enter;
 stage('paging and draft controls');
 const page=pages[0];await page.locator('#message-content').fill('P2 unsent draft');
 for(let i=0;i<3;i++){const before=await page.locator('[data-action=messages-page][data-before]').getAttribute('data-before');await page.locator('[data-action=messages-page][data-before]').click();await page.waitForFunction(previous=>document.querySelector('[data-action=messages-page][data-before]')?.dataset.before!==previous,before)}
 assert.ok(await page.locator('.conversation [data-message-id]').count()<=40);
 assert.equal(await page.locator('#message-content').inputValue(),'P2 unsent draft');
 stage('deep link control');
 await page.goto(base+'/#history/'+ids[0]+'?message=history-17');await page.waitForSelector('[data-message-id=history-17]');
 assert.equal(await page.locator('[data-message-id=history-17]').count(),1);
 await page.locator('[data-action=messages-latest]').click();await page.waitForSelector('[data-message-id=history-199]');
 let delayed=false;
 stage('stale request abort control');
 await page.route('**/api/runs/'+ids[0]+'/messages?**',async route=>{delayed=true;await new Promise(resolve=>setTimeout(resolve,500));await route.continue().catch(()=>{})});
 await page.locator('[data-action=messages-page][data-before]').click({noWaitAfter:true});
 await page.goto(base+'/#history/'+ids[1]);await page.waitForSelector('.conversation [data-message-id]');await page.waitForTimeout(650);
 assert.ok(delayed);assert.equal(await page.evaluate(async()=>{const {state}=await import('/app.js');return state.snap.run.id}),ids[1]);
 await page.unroute('**/api/runs/'+ids[0]+'/messages?**');
 await page.goto(base+'/#history/'+ids[0]);await page.waitForSelector('.conversation [data-message-id]');
 // Returning to this run deliberately restores its reading cursor. Explicitly
 // return every measured page to the live edge before releasing the source.
 for(const p of pages){await p.locator('[data-action=latest][data-scroll-target=conversation]').click();await p.waitForSelector('[data-message-id=history-199]');await p.evaluate(async()=>{const {performanceDiagnostics}=await import('/app.js');performanceDiagnostics(true)})}
 stage('three themes');
 for(const theme of ['paper','white','black']){await page.selectOption('#theme-select',theme);await page.waitForTimeout(200);await page.screenshot({path:path.join(output,'p2-'+theme+'.png'),fullPage:false});assert.equal(await page.evaluate(()=>document.documentElement.dataset.theme),theme)}
 if(singleActive){for(const id of ids.slice(1))await openBackgroundFeed(id)}
 stage('start three runs and five workers');
 const start=await fetch(base+'/_p2/start',{method:'POST'});assert.equal(start.status,200,await start.text());
 stage('provider entered; waiting semantic workspace refresh');
 await Promise.all(pages.map(p=>p.waitForFunction(async()=>{const {state}=await import('/app.js');return state.snap.requests.some(r=>r.id!=='archived')},{},{timeout:120000})));
 if(singleActive){await page.bringToFront();assert.equal(await page.evaluate(async()=>{const {state}=await import('/app.js');return state.snap.run.id}),ids[0]);assert.equal(await page.evaluate(()=>document.visibilityState),'visible');report.active_run=ids[0]}
 stage('release source timestamps');
 const release=await fetch(base+'/_p2/release',{method:'POST'});assert.equal(release.status,200);
 const listSamples=[],historySamples=[];
 const mixed=(async()=>{for(let i=0;i<20;i++){
  let begin=Date.now();const list=await fetch(base+'/api/runs?include_archived=true&include_deleted=true');assert.ok(list.ok);await list.json();listSamples.push(Date.now()-begin);
  begin=Date.now();const response=await fetch(base+'/api/runs/'+ids[i%3]+'/messages?around=history-'+(i*7));assert.ok(response.ok);const data=await response.json();assert.ok(data.messages.length<=40);historySamples.push(Date.now()-begin);
  await new Promise(resolve=>setTimeout(resolve,300));
 }})();mixed.catch(()=>{});
 stage('mixed list/history and source-to-DOM measurement');
 await Promise.all([...pages.map(p=>p.waitForFunction(()=>window.__p2.samples.length>=60,{},{timeout:150000})),...background.map(feed=>feed.done)]);await mixed;
 stage('all source frames observed; checking acceptance');
 const samples=await Promise.all(pages.map(p=>p.evaluate(()=>window.__p2)));
 for(const result of samples){assert.ok(result.samples.length>=60);assert.ok(result.maxMessages<=40);assert.ok(result.maxNodes<12000);for(let i=0;i<60;i++)assert.ok(result.samples.some(s=>s.index===i),'missing source frame '+i)}
 report.ui_diagnostics=await Promise.all(pages.map(p=>p.evaluate(async()=>{const {performanceDiagnostics}=await import('/app.js');return performanceDiagnostics()})));report.samples=samples;report.source_to_dom=stats(samples.flatMap(s=>s.samples.map(v=>v.latency)));report.list=stats(listSamples);report.message_history=stats(historySamples);report.errors=errors;report.request_count=requests.length;
 report.source_to_sse=stats(samples.flatMap(s=>s.samples.map(v=>v.source_to_sse).filter(Number.isFinite)));report.sse_to_dom=stats(samples.flatMap(s=>s.samples.map(v=>v.sse_to_dom).filter(Number.isFinite)));
 report.source_to_event_created=stats(samples.flatMap(s=>s.samples.map(v=>v.source_to_event_created).filter(Number.isFinite)));report.event_created_to_sse=stats(samples.flatMap(s=>s.samples.map(v=>v.event_created_to_sse).filter(Number.isFinite)));
 report.source_to_mutation=stats(samples.flatMap(s=>s.samples.map(v=>v.source_to_mutation).filter(Number.isFinite)));report.sse_to_mutation=stats(samples.flatMap(s=>s.samples.map(v=>v.sse_to_mutation).filter(Number.isFinite)));report.mutation_to_raf=stats(samples.flatMap(s=>s.samples.map(v=>v.mutation_to_raf).filter(Number.isFinite)));
 if(singleActive){assert.equal(background.length,2);assert.equal(samples.length,1);assert.equal(samples[0].samples.length,60);assert.equal(new Set(samples[0].samples.map(sample=>sample.request)).size,1);for(const {result} of background){assert.equal(result.error,undefined);assert.equal(result.samples.length,60);assert.equal(new Set(result.samples.map(sample=>sample.request)).size,1);for(let i=0;i<60;i++)assert.ok(result.samples.some(sample=>sample.index===i),'missing background source frame '+i);result.source_to_sse=stats(result.samples.map(sample=>sample.latency));result.sequence_verified=true}report.background_sse=background.map(feed=>feed.result);report.background_source_to_sse=stats(background.flatMap(feed=>feed.result.samples.map(sample=>sample.latency)))}
 report.negative_controls={late_page_aborted:delayed,deep_link:true,bounded_dom:true,draft_preserved:true,three_themes:true,all_source_frames_observed:true};
 assert.deepEqual(errors,[]);assert.ok(report.source_to_dom.p95_ms<500,'mixed full source-to-DOM p95 must be <500 ms: '+JSON.stringify(report.source_to_dom));report.passed=true;
}catch(error){report.passed=false;report.error=String(error);stage('failed: '+error);for(let i=0;i<pages.length;i++){try{await pages[i].screenshot({path:path.join(output,'p2-failure-'+i+'.png'),fullPage:false})}catch{}}throw error}
finally{
 for(const feed of background){clearTimeout(feed.timer);feed.abort.abort()}await Promise.all(background.map(feed=>feed.task));if(singleActive)report.background_sse=background.map(feed=>feed.result);
 if(!report.samples){report.samples=[];for(const p of pages){try{report.samples.push(await p.evaluate(()=>window.__p2))}catch{}}}
 if(!report.source_to_dom)report.source_to_dom=stats(report.samples.flatMap(s=>s?.samples?.map(v=>v.latency)||[]));
 report.errors=errors;report.error_event_limitations=['Fixed CloakBrowser suppresses pageerror events; empty events do not prove zero exceptions'];fs.writeFileSync(path.join(output,'mixed-browser.json'),JSON.stringify(report,null,2));console.log(JSON.stringify(report));await browser.close()
}
