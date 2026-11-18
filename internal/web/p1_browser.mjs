// Server-only real Store/API acceptance; responses may be delayed, never replaced.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import {createRequire} from 'node:module';
import {pathToFileURL} from 'node:url';

const require=createRequire(import.meta.url),entry=require.resolve(process.env.TRACE_CLOAKBROWSER_NODE_MODULE);
let dir=path.dirname(entry),version;
for(let i=0;i<6;i++){const p=path.join(dir,'package.json');if(fs.existsSync(p)){const pkg=JSON.parse(fs.readFileSync(p));if(pkg.name==='cloakbrowser'){version=pkg.version;break}}dir=path.dirname(dir)}
assert.equal(version,'0.6.4');assert.ok(process.env.DISPLAY&&fs.existsSync(process.env.CLOAKBROWSER_BINARY_PATH));
const {launch}=await import(pathToFileURL(entry));
const fixture=JSON.parse(process.env.TRACE_P1_FIXTURE),{base,run,otherRun,output,chain,toolMarker,messageID,mcpTool,localTool}=fixture;
const browser=await launch({headless:false,args:['--no-sandbox','--window-size=1440,1000']});
const receipt={sdk:version,headed:true,checks:[],scope:'real Store/API forms, bounded read-only projections and delayed actual responses; no external model requests'};
const contexts=[],pages=[];let currentPage;
const stage=name=>{const at=new Date().toISOString(),line=at+' '+name+'\n';receipt.stage=name;(receipt.stages??=[]).push({at,name});process.stderr.write(line);fs.appendFileSync(path.join(output,'p1-browser-stages.log'),line);fs.writeFileSync(path.join(output,'p1-progress.json'),JSON.stringify(receipt,null,2))};
const frame=page=>page.evaluate(()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve))));
async function boundedDiagnostic(operation){let timer;try{return await Promise.race([operation,new Promise((_,reject)=>{timer=setTimeout(()=>reject(new Error('failure diagnostic timed out after 5 seconds')),5000)})])}finally{clearTimeout(timer)}}
async function newPage(width=1440){
 const context=await browser.newContext({viewport:{width,height:1000}});context.setDefaultTimeout(15000);context.setDefaultNavigationTimeout(20000);contexts.push(context);const page=currentPage=await context.newPage();
 await page.addInitScript(()=>{
  window.__p1={errors:[],calls:[],hold:'',held:0,releases:[],returned:0};
  addEventListener('error',event=>window.__p1.errors.push(event.message));
  addEventListener('unhandledrejection',event=>window.__p1.errors.push(String(event.reason)));
  const originalFetch=window.fetch.bind(window);
  window.fetch=async(input,options)=>{const url=typeof input==='string'?input:input.url,method=options?.method||'GET';window.__p1.calls.push({url,method});const response=await originalFetch(input,options);if(window.__p1.hold&&url.includes(window.__p1.hold)){window.__p1.held++;await new Promise(resolve=>window.__p1.releases.push(resolve));window.__p1.returned++}return response};
 });pages.push(page);return page;
}
async function navigate(page,hash){await page.evaluate(value=>{location.hash=value},hash);await frame(page)}
async function readyRun(page,id){await page.waitForFunction(async expected=>{const {state}=await import('/app.js');return state.page==='history'&&state.selected===expected&&state.snap?.run.id===expected&&!state.runLoading},id);await frame(page)}
async function openDetails(page,selector){const item=page.locator(selector);if(!await item.evaluate(el=>el.open))await item.locator(':scope > summary').click()}
async function configSave(page,status=200){const response=page.waitForResponse(r=>new URL(r.url()).pathname==='/api/config'&&r.request().method()==='PUT');await page.locator('#config-form button[type=submit]').click();const result=await response;assert.equal(result.status(),status,await result.text());const body=await result.json();if(status===200)await page.waitForFunction(async revision=>{const {state}=await import('/app.js');return state.config?.revision===revision&&!state.settingsResources?.config?.loading},body.config.revision);await page.waitForFunction(()=>!document.querySelector('#config-form button[type=submit]').disabled);return body}
async function configRead(page){return page.evaluate(async()=>await(await fetch('/api/config')).json())}
async function modelCheckpoint(page,label){
 const data=await page.evaluate(async()=>{
  const {state}=await import('/app.js'),pick=c=>c?{revision:c.revision,provider:{model:c.provider?.model,image_input:c.provider?.image_input,reasoning_efforts:c.provider?.reasoning_efforts,model_capabilities:c.provider?.model_capabilities},role_models:c.role_models}:null;
  const response=await fetch('/api/config'),config=await response.json(),ids=['provider-model','provider-image-input','model-default-efforts',...['orchestrator','worker','recon'].flatMap(role=>['mode','provider','model','effort'].map(field=>'model-role-'+role+'-'+field))];
  const rows=Object.fromEntries(ids.map(id=>{const el=document.getElementById(id);return [id,el?{value:el.value,checked:el.type==='checkbox'?el.checked:undefined,base:el.dataset.configBase,checked_base:el.dataset.configChecked,conflict:el.dataset.configConflict,disabled:el.disabled,options:el.tagName==='SELECT'?[...el.options].map(option=>({value:option.value,selected:option.selected})):undefined}:null]}));
  let submitted;try{const {readConfigForm}=await import('/settings.js');submitted=pick(readConfigForm(state.config,'provider'))}catch{submitted={form_parse_failed:true}}
  return {http_status:response.status,config:pick(config),state:pick(state.config),submitted,form_revision:document.getElementById('config-form')?.dataset.configRevision,rows};
 });(receipt.model_checkpoints??=[]).push({label,...data});return data;
}
async function toolsSave(page){const response=page.waitForResponse(r=>new URL(r.url()).pathname===`/api/runs/${run}/tools`&&r.request().method()==='PUT');await page.locator('#tool-selection-form .primary').click();assert.equal((await response).status(),200);await page.waitForFunction(()=>!document.querySelector('#tool-selection-form .primary')?.disabled)}
async function selectedTools(page){return page.evaluate(async id=>(await(await fetch('/api/runs/'+id)).json()).run.tool_selection.ids,run)}
async function assertDialog(page,title){assert.equal(await page.getByRole('dialog',{name:title,exact:true}).count(),1);assert.equal(await page.locator('#detail-dialog').getAttribute('aria-labelledby'),'dialog-title');assert.ok(await page.locator('#detail-dialog').evaluate(el=>el.open));assert.ok(await page.locator('#detail-dialog').evaluate(el=>el.contains(document.activeElement)))}
async function assertChainHash(page){const summary=page.locator('#dialog-content summary').filter({hasText:/^整链来源与校验值$/});if(!await summary.evaluate(el=>el.parentElement.open))await summary.click();const raw=JSON.parse(await summary.evaluate(el=>el.parentElement.querySelector('pre').innerText));assert.equal(raw.source_run_id,chain.run);assert.equal(raw.chain.content_hash,chain.hash);assert.match(await page.locator('#dialog-content').innerText(),new RegExp(chain.hash))}
async function hold(page,route){await page.evaluate(route=>{window.__p1.hold=route;window.__p1.held=0;window.__p1.returned=0},route)}
async function release(page){await page.evaluate(()=>{window.__p1.hold='';for(const resolve of window.__p1.releases.splice(0))resolve()});await page.waitForFunction(()=>window.__p1.returned>0);await frame(page)}
async function remountSelectControls(){
 const page=await newPage();
 try{
  // Same-origin native document without app boot; exercise the production
  // redraw/draft modules across actual replacement of the mounted page DOM.
  await page.goto(base+'/interactions.js',{waitUntil:'domcontentloaded'});
  return await page.evaluate(async()=>{
   document.body.innerHTML='<main id="workspace"><div id="page"></div></main>';
   const {redraw,rememberInput}=await import('/interactions.js'),{modelSettings,readModelSettings,syncModelFields}=await import('/model-settings.js');
   const config={provider:{model:'main',reasoning_efforts:['high']},role_models:{worker:{provider:'default',model:'main',reasoning_effort:''}}},state={page:'settings',settingTab:'provider'};
   const form=(c,ephemeral)=>'<form id="config-form">'+modelSettings(c)+'<select id="retained-draft" data-config-base=""><option value=""></option><option value="high">high</option></select><select id="ephemeral-draft"><option value="'+ephemeral+'">'+ephemeral+'</option></select></form>';
   redraw(state,form(config,'old'));
   for(const [id,value] of [['model-role-worker-effort','high'],['retained-draft','high'],['ephemeral-draft','old']]){const field=document.getElementById(id);field.value=value;rememberInput(state,field)}
   state.settingTab='skills';redraw(state,'<p>Another settings page</p>');
   const withdrawn=structuredClone(config);withdrawn.provider.reasoning_efforts=[];
   state.settingTab='provider';redraw(state,form(withdrawn,'new'));
   const row=id=>{const el=document.getElementById(id);return {value:el.value,base:el.dataset.configBase,conflict:el.dataset.configConflict,selected:[...el.selectedOptions].map(option=>({text:option.textContent,disabled:option.disabled}))}};
   const beforeSync=row('model-role-worker-effort');syncModelFields(withdrawn);
   let validation='accepted';try{readModelSettings(withdrawn)}catch(error){validation=error.message}
   return {beforeSync,afterSync:row('model-role-worker-effort'),retained:row('retained-draft'),ephemeral:row('ephemeral-draft'),validation,errors:window.__p1.errors};
  });
 }finally{await page.close()}
}

try{
 stage('remount select controls');
 const remount=await remountSelectControls();receipt.remount_select_controls=remount;
 assert.equal(remount.beforeSync.value,'high');assert.equal(remount.beforeSync.base,'');assert.equal(remount.beforeSync.conflict,'true');assert.equal(remount.beforeSync.selected[0].disabled,true);assert.match(remount.beforeSync.selected[0].text,/选项已移除/);
 assert.equal(remount.afterSync.value,'high');assert.equal(remount.afterSync.conflict,'true');assert.match(remount.validation,/未声明支持/);assert.equal(remount.retained.value,'high');assert.equal(remount.retained.conflict,undefined);assert.equal(remount.ephemeral.value,'new');assert.deepEqual(remount.errors,[]);
 stage('model rows and revision conflict');
 const a=await newPage(),aDocument=await a.goto(base+'/#models',{waitUntil:'commit'});assert.equal(aDocument?.status(),200,'model A document response');
 stage('model A ready');await a.bringToFront();await a.waitForSelector('#model-role-worker-mode');
 const b=await newPage(),bDocument=await b.goto(base+'/#models',{waitUntil:'commit'});assert.equal(bDocument?.status(),200,'model B document response');
 stage('model B ready');await b.bringToFront();await b.waitForSelector('#model-role-worker-mode');
 const selectControls=await a.evaluate(async()=>{
  const {patchLive}=await import('/interactions.js');
  const run=(initial,fresh,draft)=>{const host=document.createElement('div');host.innerHTML=initial;const select=host.querySelector('select');if(draft!==undefined)select.value=draft;patchLive(host,fresh);return {value:select.value,base:select.dataset.configBase,conflict:select.dataset.configConflict,selected:[...select.selectedOptions].map(option=>({text:option.textContent,disabled:option.disabled}))}};
  const markup=(base,values)=>'<select id="native-config-control" data-config-base="'+base+'">'+values.map(value=>'<option value="'+value+'"'+(value===base?' selected':'')+'>'+value+'</option>').join('')+'</select>';
  return {baseline:run(markup('',['']),markup('high',['','high'])),draft:run(markup('',['','low']),markup('high',['','low','high']),'low'),removed:run(markup('',['','high']),markup('',['','low']),'high')};
 });
 assert.equal(selectControls.baseline.value,'high');assert.equal(selectControls.baseline.conflict,undefined);assert.equal(selectControls.draft.value,'low');assert.equal(selectControls.draft.conflict,'true');assert.equal(selectControls.removed.value,'high');assert.equal(selectControls.removed.conflict,'true');assert.equal(selectControls.removed.selected[0].disabled,true);assert.match(selectControls.removed.selected[0].text,/选项已移除/);receipt.select_controls=selectControls;
 const initial=await configRead(a);assert.ok(initial.revision);assert.equal(await a.locator('#provider-key').inputValue(),'[stored secret]');
 assert.equal(await b.locator('#config-form').getAttribute('data-config-revision'),initial.revision);
 await b.locator('#provider-model').fill('p1-stale-draft');
 await a.locator('#model-role-worker-mode').selectOption('custom');
 await a.locator('#model-role-worker-effort').selectOption('high');
 await openDetails(a,'#caps-default');
 const capability=a.locator('[data-capability-row="default"]').last(),capIndex=await capability.getAttribute('data-row'),capPrefix='#caps-default-'+capIndex;
 await a.locator(capPrefix+'-model').fill('p1-row-model');await a.locator(capPrefix+'-image').selectOption('true');await a.locator(capPrefix+'-efforts').fill('high');
 const price=a.locator('[data-model-price]').last(),priceIndex=await price.getAttribute('data-model-price'),pricePrefix='#model-price-'+priceIndex;
 for(const [field,value] of Object.entries({model:'main',currency:'USD',input_per_million:'1.123456789123456789',output_per_million:'3',cache_read_per_million:'0.1',cache_write_per_million:'0',source:'P1 fixture quote',effective_at:'2026-11-10T00:00:00Z'}))await a.locator(pricePrefix+'-'+field).fill(value);
 await a.locator(pricePrefix+'-billing_basis').selectOption('disjoint_cache_v1');
 await openDetails(a,'#cost-config');await a.locator('#cost-currency').fill('USD');
 const saved=await configSave(a);assert.notEqual(saved.config.revision,initial.revision);
 const current=await configRead(a),rate=current.costs.rates.find(rate=>rate.source==='P1 fixture quote');
 assert.equal(current.provider.api_key,'[stored secret]');assert.equal(current.role_models.worker.reasoning_effort,'high');assert.equal(current.role_models.worker.model,'main');
 assert.equal(current.provider.model_capabilities['p1-row-model'].image_input,true);assert.deepEqual(current.provider.model_capabilities['p1-row-model'].reasoning_efforts,['high']);
 assert.equal(rate.input_per_million,'1.123456789123456789');assert.equal(rate.cache_write_per_million,'0');assert.equal(rate.billing_basis,'disjoint_cache_v1');
 await modelCheckpoint(b,'B before stale save');
 const conflict=await configSave(b,409);assert.equal(conflict.code,'config_revision_conflict');assert.equal(await b.locator('#provider-model').inputValue(),'p1-stale-draft');
 assert.equal((await configRead(b)).provider.model,'main');
 const reload=b.waitForResponse(r=>new URL(r.url()).pathname==='/api/config'&&r.request().method()==='GET');await b.locator('[data-action=reload-settings]').click();await reload;
 await b.waitForFunction(async revision=>{const {state}=await import('/app.js');return state.config?.revision===revision&&!state.settingsResources.config.loading},current.revision);
 await modelCheckpoint(b,'B after reload with draft');
 assert.equal(await b.locator('#provider-model').inputValue(),'p1-stale-draft');await configSave(b,409);
 await b.locator('[data-action=discard-config]').click();assert.equal(await b.locator('#provider-model').inputValue(),'main');await modelCheckpoint(b,'B after discard');await b.locator('#provider-image-input').check();await modelCheckpoint(b,'B before successful save');await configSave(b);
 const afterB=await modelCheckpoint(b,'B after successful save');assert.equal((await configRead(b)).provider.api_key,'[stored secret]');assert.equal(afterB.config.role_models?.worker?.reasoning_effort,'high','B unrelated provider image edit must retain worker effort');assert.equal(afterB.config.role_models?.worker?.model,'main');
 receipt.checks.push({name:'ordinary rows and A/B revision',passed:true,precision:rate.input_per_million,staleStatuses:[409,409],maskedSecret:true});

 stage('MCP and local catalogs');currentPage=a;
 await navigate(a,'#history/'+run);await readyRun(a,run);
 await navigate(a,'#mcp');await a.waitForSelector('[data-integration-kind=mcp]');
 assert.equal(await a.locator('[data-integration-kind=local]').count(),0);assert.equal(await a.locator('#local-json').count(),0);
 await openDetails(a,'#run-tool-selection');assert.equal(await a.locator('[data-tool-id="'+localTool+'"]').count(),0);
 await a.locator('[data-tool-id="'+mcpTool+'"]').uncheck();await toolsSave(a);
 assert.ok(!(await selectedTools(a)).includes(mcpTool));assert.ok((await selectedTools(a)).includes(localTool),'MCP selection preserves local selection');
 await a.locator('[data-tool-id="'+mcpTool+'"]').check();await toolsSave(a);
 await navigate(a,'#local-tools');await a.waitForSelector('[data-integration-kind=local]');assert.equal(await a.locator('[data-integration-kind=mcp]').count(),0);assert.equal(await a.locator('#mcp-json').count(),0);
 await openDetails(a,'#run-tool-selection');assert.equal(await a.locator('[data-tool-id="'+mcpTool+'"]').count(),0);
 await a.locator('[data-tool-id="'+localTool+'"]').uncheck();await toolsSave(a);assert.ok((await selectedTools(a)).includes(mcpTool),'local selection preserves MCP selection');
 await a.locator('[data-tool-id="'+localTool+'"]').check();await toolsSave(a);
 await a.locator('[data-action=local-detail]').first().click();const localTitle=await a.locator('#dialog-title').innerText();await assertDialog(a,localTitle);
 const localConfig=(await configRead(a)).local_tools[0];if(!localConfig.local_path)assert.doesNotMatch(await a.locator('#dialog-content').innerText(),/项目目录/);
 await a.keyboard.press('Escape');assert.equal(await a.locator('#detail-dialog').evaluate(el=>el.open),false);
 receipt.checks.push({name:'MCP/local independent catalogs and selections',passed:true});

 stage('skills');
 await navigate(a,'#skills');await a.waitForSelector('[data-action=skill-detail]');
 for(const name of ['asc','frida','web-analysis','cloakbrowser']){const card=a.locator('[data-action=skill-detail][data-name="'+name+'"]');assert.equal(await card.count(),1);const description=await card.locator('p').innerText();assert.match(description,/[\u4e00-\u9fff]/);assert.match(description,/[A-Za-z]{5}/)}
 assert.equal(await a.evaluate(()=>window.__p1.calls.filter(call=>/^\/api\/skills\//.test(call.url)).length),0,'skill bodies stay on demand');
 await a.locator('[data-action=skill-detail][data-name=asc]').click();await a.waitForFunction(()=>document.querySelector('#detail-dialog').open);await assertDialog(a,'asc');assert.match(await a.locator('#dialog-content').innerText(),/description:/);await a.keyboard.press('Escape');
 receipt.checks.push({name:'four bilingual skill descriptions and on-demand body',passed:true});

 stage('lazy tool and delivery lifecycle');
 await navigate(a,'#history/'+run);await readyRun(a,run);
 const lazy=a.locator('[data-lazy-tool][data-tool-key="p1-request:p1-long"]');await lazy.waitFor({state:'attached'});
 assert.doesNotMatch(await a.locator('.conversation').innerHTML(),new RegExp(toolMarker));
 const ancestors=await lazy.evaluate(el=>{const ids=[];for(let parent=el.parentElement;parent;parent=parent.parentElement)if(parent.tagName==='DETAILS')ids.push(parent.id);return ids.reverse()});for(const id of ancestors)await openDetails(a,'details[id='+JSON.stringify(id)+']');
 await lazy.locator(':scope > summary').click();await a.waitForFunction(marker=>document.querySelector('.conversation')?.textContent.includes(marker),toolMarker);
 const toolBody=lazy.locator('.tool-body');await toolBody.evaluate(el=>{el.scrollTop=Math.min(80,Math.max(0,el.scrollHeight-el.clientHeight-30))});const scroll=await toolBody.evaluate(el=>({top:el.scrollTop,height:el.scrollHeight,client:el.clientHeight}));
 assert.ok(scroll.height>scroll.client,'long result owns a nested scroll container');
 await openDetails(a,'#run-mailbox');assert.equal(await a.evaluate(()=>window.__p1.calls.filter(call=>call.url.includes('/message-deliveries')).length),0,'delivery receipts stay on demand');
 await a.locator('[data-action=message-deliveries]').click();await a.waitForFunction(()=>document.querySelector('#detail-dialog').open);await assertDialog(a,'消息投递与归档回执');
 const deliveryRow=a.locator('#dialog-content article').filter({hasText:messageID});assert.equal(await deliveryRow.count(),1);assert.match(await deliveryRow.innerText(),/已进入接收方历史/);assert.match(await deliveryRow.innerText(),/已出现在归档请求/);assert.match(await deliveryRow.innerText(),/context_summary/);assert.equal(await deliveryRow.locator('a[href*="request=p1-summary"]').count(),1);assert.match(await a.locator('#dialog-content').innerText(),/不代表 Provider 收到/);
 assert.ok(Math.abs((await toolBody.evaluate(el=>el.scrollTop))-scroll.top)<=1,'dialog render preserves nested tool scroll');
 await hold(a,'/message-deliveries');await a.locator('#dialog-content [data-action=message-deliveries]').first().click();await a.waitForFunction(()=>window.__p1.held===1);await a.keyboard.press('Escape');await release(a);assert.equal(await a.locator('#detail-dialog').evaluate(el=>el.open),false,'late refresh must not reopen closed dialog');
 await hold(a,'/message-deliveries');await a.locator('#run-mailbox [data-action=message-deliveries]').click();await a.waitForFunction(()=>window.__p1.held===1);await navigate(a,'#history/'+otherRun);await readyRun(a,otherRun);await release(a);assert.equal(await a.locator('#detail-dialog').evaluate(el=>el.open),false,'late prior-run receipt must not populate the new run');
 assert.equal(await a.locator('[data-tool-run="'+run+'"]').count(),0,'prior-run lazy tool body is absent');
 receipt.checks.push({name:'lazy long result, nested scroll, delivery detail and late replies',passed:true});

 stage('fixed chain source');
 const chainHash='#history/'+run+'?tab=evidence&chain='+encodeURIComponent(chain.id)+'&chain_run='+encodeURIComponent(chain.run)+'&chain_hash='+encodeURIComponent(chain.hash);
 await navigate(a,chainHash);await readyRun(a,run);await a.waitForFunction(()=>document.querySelector('#detail-dialog').open);await assertDialog(a,'操作证据链');
 const fixedLink=await a.locator('#dialog-content [data-action=copy-link][data-href*="chain_hash="]').getAttribute('data-href');assert.ok(fixedLink.includes('chain_hash='+encodeURIComponent(chain.hash)));await assertChainHash(a);
 const originalChain=await a.evaluate(async c=>await(await fetch('/api/evidence-chains/'+encodeURIComponent(c.run)+'/'+encodeURIComponent(c.id)+'?hash='+encodeURIComponent(c.hash))).json(),chain);assert.equal(originalChain.verification,'unverified');assert.equal(originalChain.source_status,'unverified');assert.equal(originalChain.history_readable,true);
 const fixedReads=await a.evaluate(()=>window.__p1.calls.filter(call=>call.url.startsWith('/api/evidence-chains/')&&call.method==='GET'));
 assert.ok(fixedReads.some(call=>new URL(call.url,base).searchParams.get('hash')===chain.hash));await a.keyboard.press('Escape');
 assert.equal(await a.evaluate(async()=>{const response=await fetch('/_p1/change-source',{method:'POST'});return response.status}),200);
 await a.locator('[data-action=chain-refresh]').click();await a.waitForFunction(()=>document.querySelector('.catalog-list')?.textContent.includes('来源失效'));
 await a.locator('[data-action=chain-detail][data-hash="'+chain.hash+'"]').first().click();await a.waitForFunction(()=>document.querySelector('#detail-dialog').open);assert.match(await a.locator('#dialog-content').innerText(),/来源失效/);await assertChainHash(a);
 const changedChain=await a.evaluate(async c=>await(await fetch('/api/evidence-chains/'+encodeURIComponent(c.run)+'/'+encodeURIComponent(c.id)+'?hash='+encodeURIComponent(c.hash))).json(),chain);assert.equal(changedChain.verification,'invalid');assert.equal(changedChain.source_status,'invalid');assert.equal(changedChain.history_readable,true);assert.equal(changedChain.chain.content_hash,chain.hash);
 const mismatch=await a.evaluate(async c=>(await fetch('/api/evidence-chains/'+encodeURIComponent(c.run)+'/'+encodeURIComponent(c.id)+'?hash=wrong-fixed-hash')).status,chain);assert.equal(mismatch,409);
 receipt.checks.push({name:'fixed chain hash, invalidated source and readable history',passed:true});await a.keyboard.press('Escape');
 await a.screenshot({path:path.join(output,'p1-desktop.png'),fullPage:true,timeout:10000});

 stage('mobile model forms');
 const mobile=await newPage(390),mobileDocument=await mobile.goto(base+'/#models',{waitUntil:'commit'});assert.equal(mobileDocument?.status(),200,'mobile model page document response');await mobile.waitForSelector('#model-role-worker-mode');
 const mobileConfig=await modelCheckpoint(mobile,'mobile before effort assertion');assert.equal(mobileConfig.config.role_models?.worker?.reasoning_effort,'high','same-origin API must retain high before the mobile DOM check');
 assert.equal(await mobile.locator('#provider-key').inputValue(),'[stored secret]');assert.equal(await mobile.locator('#model-role-worker-effort').inputValue(),'high');
 const overflow=await mobile.evaluate(()=>({document:document.documentElement.scrollWidth,viewport:innerWidth}));assert.ok(overflow.document<=overflow.viewport+3,'row forms overflow mobile viewport: '+JSON.stringify(overflow));
 stage('mobile local catalog');
 await navigate(mobile,'#local-tools');await mobile.waitForSelector('[data-integration-kind=local]');
 stage('mobile local detail');
 await mobile.locator('[data-integration-kind=local] [data-action=local-detail]').first().click();await assertDialog(mobile,await mobile.locator('#dialog-title').innerText());await mobile.screenshot({path:path.join(output,'p1-mobile.png'),fullPage:true,timeout:10000});
 stage('runtime error checks');
 for(const page of [a,b,mobile])assert.deepEqual(await page.evaluate(()=>window.__p1.errors),[]);
 receipt.checks.push({name:'390px row forms and named dialog',passed:true});receipt.result='PASS';
}catch(error){
 receipt.result='FAIL';receipt.error=String(error);receipt.failedStage=receipt.stage;stage('failed '+error);
 receipt.pageDiagnostics=[];for(const [index,page] of pages.entries()){if(page.isClosed())continue;try{receipt.pageDiagnostics.push({index,...await boundedDiagnostic(page.evaluate(()=>{const el=document.getElementById('model-role-worker-mode'),style=el&&getComputedStyle(el),rect=el?.getBoundingClientRect();return {url:location.href,readyState:document.readyState,worker:el?{display:style.display,visibility:style.visibility,rect:{x:rect.x,y:rect.y,width:rect.width,height:rect.height},hidden:el.hidden,ancestors:[...function*(node){for(;node;node=node.parentElement)yield node}(el)].map(node=>({tag:node.tagName,id:node.id,display:getComputedStyle(node).display,visibility:getComputedStyle(node).visibility,contentVisibility:getComputedStyle(node).contentVisibility}))}:null,errors:window.__p1?.errors,calls:window.__p1?.calls}}))})}catch(diagnosticError){receipt.pageDiagnostics.push({index,error:String(diagnosticError)})}}
 try{receipt.failureState=await boundedDiagnostic(currentPage?.evaluate(async()=>{const {state}=await import('/app.js');return {hash:location.hash,page:state.page,tab:state.settingTab,resources:state.settingsResources,localCount:state.config?.local_tools?.length,localCards:document.querySelectorAll('[data-integration-kind=local]').length,localDetails:document.querySelectorAll('[data-action=local-detail]').length,dialogOpen:document.querySelector('#detail-dialog')?.open,errors:window.__p1?.errors}}))}catch(diagnosticError){receipt.failureState={error:String(diagnosticError)}}
 try{await currentPage?.screenshot({path:path.join(output,'p1-failure.png'),fullPage:true,timeout:10000})}catch{}
 throw error;
}finally{fs.writeFileSync(path.join(output,'p1-browser.json'),JSON.stringify(receipt,null,2));console.log(JSON.stringify(receipt));stage('closing contexts');for(const context of contexts)await context.close();stage('closing browser');await browser.close();stage('closed')}
