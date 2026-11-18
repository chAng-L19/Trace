// Companion acceptance runs only on the designated server, fixed headed SDK.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import {createRequire} from 'node:module';
import {pathToFileURL} from 'node:url';
const require=createRequire(import.meta.url),entry=require.resolve(process.env.TRACE_CLOAKBROWSER_NODE_MODULE);
let dir=path.dirname(entry),version;
for(let i=0;i<6;i++){const p=path.join(dir,'package.json');if(fs.existsSync(p)){const pkg=JSON.parse(fs.readFileSync(p));if(pkg.name==='cloakbrowser'){version=pkg.version;break}}dir=path.dirname(dir)}
assert.equal(version,'0.6.4');assert.ok(process.env.DISPLAY&&fs.existsSync(process.env.CLOAKBROWSER_BINARY_PATH));
const {launch}=await import(pathToFileURL(entry)),{base,run,output}=JSON.parse(process.env.TRACE_U567_FIXTURE);
const browser=await launch({headless:false,args:['--no-sandbox','--window-size=1440,1000']});
const receipt={sdk:version,headed:true,viewports:[],scope:'native source configuration, explicit account probe, asynchronous tree capacity and bilingual immutable file delivery; fixture providers only'};
try{
 for(const width of [1440,390]){
  const context=await browser.newContext({viewport:{width,height:1000}}),page=await context.newPage();
  await page.addInitScript(()=>{window.__errors=[];addEventListener('error',e=>window.__errors.push(e.message));addEventListener('unhandledrejection',e=>window.__errors.push(String(e.reason)))});
  await page.goto(base+'/#models');await page.waitForSelector('#model-role-recon-mode');
  assert.equal(await page.locator('#source-fofa-api_key').inputValue(),'[stored secret]');
  assert.equal(await page.locator('#model-role-recon-model').isDisabled(),width===1440);
  if(width===1440){
   await page.locator('#model-role-recon-mode').selectOption('custom');await page.locator('#model-role-recon-effort').selectOption('high');
   await page.locator('#config-form button[type=submit]').click();
   await page.waitForFunction(async()=>{const c=await(await fetch('/api/config')).json();return c.role_models?.recon?.reasoning_effort==='high'});
   await page.locator('#source-fofa > summary').click();
   await page.locator('#source-fofa-api_key').fill('unsaved-draft-key');
   await page.locator('#source-fofa-probe').click();
   await page.waitForFunction(()=>document.querySelector('#source-fofa-probe-status')?.dataset.status==='authenticated');
   assert.equal(await page.locator('#source-fofa-api_key').inputValue(),'unsaved-draft-key','account check does not save or discard drafts');
   const saved=await page.evaluate(async()=>await(await fetch('/api/config')).json());
   assert.equal(saved.search_sources.fofa.api_key,'[stored secret]');assert.equal(saved.role_models.recon.model,'main');
  }
  await page.goto(base+'/#history/'+run);
  await page.waitForFunction(()=>document.querySelector('[data-worker-tree-capacity]')?.textContent.includes('0 / 5'));
  await page.locator('[data-tab=artifacts]').click();
  await page.waitForFunction(()=>document.querySelector('#artifact-records')?.textContent.includes('actual-deliverable.txt'));
  assert.match(await page.locator('#artifact-records').innerText(),/总结报告/);
  assert.match(await page.locator('#artifact-records').innerText(),/actual-deliverable.txt/);
  assert.doesNotMatch(await page.locator('#artifact-records').innerText(),/RAW_CLI_OUTPUT|INTERNAL_OUTPUT_NOT_A_REPORT/);
  const links=await page.locator('#artifact-records a[href*="report_id="]').evaluateAll(nodes=>nodes.map(a=>a.href));
  assert.equal(links.length,2);
  const files=await Promise.all(links.map(async url=>{const response=await fetch(url);assert.equal(response.status,200);return {url,body:await response.text()}}));
  assert.match(files.find(f=>f.url.includes('language=en')).body,/The check did not meet/);
  assert.match(files.find(f=>f.url.includes('language=zh-CN')).body,/此次检查未满足/);
  for(const f of files)assert.match(f.body,/"business_status": "failed"/);
  assert.deepEqual(await page.evaluate(()=>window.__errors),[]);
  await page.screenshot({path:path.join(output,`u567-${width}.png`),fullPage:true});
  receipt.viewports.push({width,passed:true,files:files.length});await context.close();
 }
 const status=await(await fetch(base+'/_u567/status')).json();assert.equal(status.probes,1);
 receipt.result='PASS';
}catch(error){receipt.result='FAIL';receipt.error=String(error);throw error}
finally{fs.writeFileSync(path.join(output,'u567-browser.json'),JSON.stringify(receipt,null,2));console.log(JSON.stringify(receipt));await browser.close()}
