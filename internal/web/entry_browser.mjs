// Same fixture and observer for baseline/candidate; fixed headed browser only.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import {createRequire} from 'node:module';
import {pathToFileURL} from 'node:url';
const require=createRequire(import.meta.url),sdk=process.env.TRACE_CLOAKBROWSER_NODE_MODULE;
assert.ok(sdk&&process.env.DISPLAY&&process.env.CLOAKBROWSER_BINARY_PATH);
assert.equal(JSON.parse(fs.readFileSync(path.join(sdk,'package.json'),'utf8')).version,'0.6.4');
assert.ok(fs.existsSync(process.env.CLOAKBROWSER_BINARY_PATH));
const {launch}=await import(pathToFileURL(require.resolve(sdk)));
const {base,ids,output,sqlite}=JSON.parse(process.env.TRACE_P0_FIXTURE);
const report={sdk:'0.6.4',headed:true,sqlite,scope:'session entry; four 50k-event histories, 300 archived requests/run, 120 messages/run, five presentation worker records; not B9 or actual worker execution',entries:[],checks:[],errors:[]};
const browser=await launch({headless:false,args:['--no-sandbox','--window-size=1440,1000']});
let context;
const delay=ms=>new Promise(resolve=>setTimeout(resolve,ms));
const stats=values=>{const a=[...values].sort((x,y)=>x-y);return {count:a.length,p50_ms:a[Math.floor((a.length-1)/2)],p95_ms:a[Math.ceil(a.length*.95)-1],max_ms:a.at(-1)}};
async function instrument(ctx){
 await ctx.addInitScript(()=>{
  window.__entry={calls:[],opens:[],probes:[]};
  const originalFetch=window.fetch.bind(window),Native=window.EventSource;
  window.fetch=async(input,...args)=>{const row={url:String(typeof input==='string'?input:input.url),start:performance.now()};window.__entry.calls.push(row);try{const response=await originalFetch(input,...args);row.headers=performance.now();row.status=response.status;return response}catch(error){row.error=String(error);throw error}};
  window.EventSource=class extends Native{constructor(...args){super(...args);const row={url:String(args[0]),start:performance.now()};window.__entry.calls.push({...row,kind:'sse'});this.addEventListener('open',()=>window.__entry.opens.push({...row,open:performance.now()}))}};
  const scan=()=>{
   const probe=window.__entry.current;if(!probe)return;
   if(document.querySelector('.workspace-loading'))probe.skeleton??=performance.now();
   const pane=document.querySelector('.conversation');
   if(!location.hash.includes(probe.id)||document.querySelector('.workspace-loading')||!pane?.querySelector('[data-message-id]'))return;
   if(probe.dom===undefined){probe.dom=performance.now();probe.messages=pane.querySelectorAll('[data-message-id]').length;requestAnimationFrame(()=>{probe.frame=performance.now()})}
  };
  window.__entry.begin=(id,label,start=performance.now())=>{const p={id,label,start};window.__entry.current=p;window.__entry.probes.push(p)};
  document.addEventListener('click',event=>{const el=event.target.closest?.('[data-action="select"]');if(el)window.__entry.begin(el.dataset.id,'click')},true);
  if(location.hash.startsWith('#history/'))window.__entry.begin(location.hash.split('/')[1].split('?')[0],'cold',0);
  new MutationObserver(scan).observe(document,{subtree:true,childList:true,characterData:true});
 });
}
async function ready(page,id){
 await page.waitForFunction(async expected=>{const {state}=await import('/app.js');return state.snap?.run.id===expected&&!state.runLoading&&!!document.querySelector('.conversation [data-message-id]')},id);
 await page.waitForFunction(expected=>{const probe=window.__entry.current;return probe?.id===expected&&window.__entry.opens.some(row=>row.start>=probe.start&&row.url.includes('/'+expected+'/events'))},id);
 await page.evaluate(()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve))));
}
async function take(page){
 // Record the deferred management read too, so faster SSE cannot hide a
 // slower capacity read. This waits on the real response, including errors.
 await page.waitForFunction(()=>{const p=window.__entry.current;return p&&window.__entry.calls.some(r=>r.start>=p.start&&r.url==='/api/runs/'+p.id+'/worker-capacity'&&Number.isFinite(r.headers))});
 const row=await page.evaluate(async()=>{const probe=window.__entry.probes.at(-1),{performanceDiagnostics}=await import('/app.js');const rows=window.__entry.calls.filter(r=>r.start>=probe.start),open=window.__entry.opens.find(r=>r.start>=probe.start&&r.url.includes('/'+probe.id+'/events')),capacity=rows.find(r=>r.url==='/api/runs/'+probe.id+'/worker-capacity'),diagnostics=performanceDiagnostics();return {...probe,dom_ms:probe.dom-probe.start,frame_ms:probe.frame-probe.start,skeleton_ms:probe.skeleton===undefined?null:probe.skeleton-probe.start,sse_ms:open?open.open-probe.start:null,capacity_headers_ms:capacity?.headers-probe.start,capacity_status:capacity?.status,calls:rows,render_count:diagnostics.enabled&&window.__entry.diagnosticsSince<=probe.start?diagnostics.samples.filter(r=>r.phase==='page-serialize'&&r.at>=probe.start).length:null}});
 for(const key of ['dom_ms','frame_ms','sse_ms','capacity_headers_ms'])assert.ok(typeof row[key]==='number'&&Number.isFinite(row[key])&&row[key]>=0,'missing entry sample: '+key);
 return row;
}
async function warm(page,id){
 await page.locator('[data-action=archive-back]').click();await page.locator(`[data-action=select][data-id="${id}"]`).click();await ready(page,id);await delay(100);const row=await take(page);assert.ok(row.messages>0&&row.messages<=40);row.workspace_requests=row.calls.filter(r=>r.url.includes('/runs/'+id+'?view=workspace')).length;assert.ok(row.workspace_requests>=1);report.entries.push(row);
}
try{
 context=await browser.newContext({viewport:{width:1440,height:1000}});await instrument(context);
 const page=await context.newPage();page.on('pageerror',error=>report.errors.push(String(error)));
 await page.goto(base+'/#history/'+ids[0],{waitUntil:'domcontentloaded'});await ready(page,ids[0]);report.entries.push(await take(page));
 await page.evaluate(async()=>{const {performanceDiagnostics}=await import('/app.js');performanceDiagnostics(true);window.__entry.diagnosticsSince=performance.now()});
 await page.locator('#message-content').fill('ENTRY retained draft');
 for(let pass=0;pass<2;pass++)for(const id of ids)await warm(page,id);
 await warm(page,ids[0]);assert.equal(await page.locator('#message-content').inputValue(),'ENTRY retained draft');assert.ok(await page.locator('.exploration-preview svg a').count()>=10);report.checks.push('bounded transcript, retained draft and inline graph');
 // Delay only the real configuration request, never substitute its contents.
 const slow=await context.newPage();let configurationArrived=false,releaseConfiguration;
 const configurationGate=new Promise(resolve=>releaseConfiguration=resolve);
 await slow.route('**/api/config',async route=>{await configurationGate;await route.continue()});
 slow.on('response',response=>{if(new URL(response.url()).pathname==='/api/config')configurationArrived=true});
 await slow.goto(base+'/#history/'+ids[1],{waitUntil:'domcontentloaded'});await ready(slow,ids[1]);
 await slow.evaluate(async()=>{const {performanceDiagnostics}=await import('/app.js');performanceDiagnostics(true);window.__entry.diagnosticsSince=performance.now()});
 const slowBefore=await slow.evaluate(async()=>{const {performanceDiagnostics}=await import('/app.js');return performanceDiagnostics().samples.filter(r=>r.phase==='page-serialize').length});
 assert.equal(configurationArrived,false,'configuration must remain gated until workspace is ready');
 const configurationResponse=slow.waitForResponse(response=>new URL(response.url()).pathname==='/api/config');releaseConfiguration();await configurationResponse;
 await slow.waitForFunction(async()=>!!(await import('/app.js')).state.config);await delay(100);
 const slowAfter=await slow.evaluate(async()=>{const {performanceDiagnostics}=await import('/app.js');return performanceDiagnostics().samples.filter(r=>r.phase==='page-serialize').length});
 report.slow_config={arrived:configurationArrived,page_renders_after_workspace:slowAfter-slowBefore,entry:await take(slow)};await slow.close();
 // Event locator must use its original sequence and remain a real API read.
 await page.goto(base+'/#history/'+ids[2]+'?tab=events&event=40',{waitUntil:'domcontentloaded'});await page.waitForSelector('#event-40');
 const eventCalls=await page.evaluate(()=>window.__entry.calls.filter(r=>r.url.includes('/events/history')));report.event_locator={calls:eventCalls};assert.ok(eventCalls.some(r=>r.url.includes('before=41')));report.checks.push('event locator preserves sequence');
 // A late aborted workspace must never overwrite the newly selected run.
 let delayOnce=true;await page.route('**/api/runs/'+ids[0]+'?view=workspace*',async route=>{if(delayOnce){delayOnce=false;await delay(350)}await route.continue()});
 await page.evaluate(id=>{window.__entry.begin(id,'hash');location.hash='#history/'+id},ids[0]);await delay(30);await page.evaluate(id=>{window.__entry.begin(id,'hash');location.hash='#history/'+id},ids[3]);await ready(page,ids[3]);await delay(450);
 const selected=await page.evaluate(async()=>{const {state}=await import('/app.js');return {selected:state.selected,run:state.snap?.run.id}});assert.equal(selected.selected,ids[3]);assert.equal(selected.run,ids[3]);await page.unroute('**/api/runs/'+ids[0]+'?view=workspace*');report.checks.push('late aborted workspace remains isolated');
 await page.screenshot({path:path.join(output,'entry-final.png')});
 report.warm_dom=stats(report.entries.filter(r=>r.label==='click').map(r=>r.dom_ms));report.warm_sse=stats(report.entries.filter(r=>r.label==='click').map(r=>r.sse_ms));report.warm_capacity=stats(report.entries.filter(r=>r.label==='click').map(r=>r.capacity_headers_ms));report.warm_renders=report.entries.filter(r=>r.label==='click').map(r=>r.render_count);report.passed=true;
}catch(error){report.passed=false;report.error=String(error?.stack||error);throw error}
finally{fs.mkdirSync(output,{recursive:true});fs.writeFileSync(path.join(output,'entry-browser.json'),JSON.stringify(report,null,2));await context?.close();await browser.close()}
