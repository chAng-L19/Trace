// Called by TestMediaMultiSourceStoreReopenHTTP against its actual reopened Store.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import {createHash} from 'node:crypto';
import {createRequire} from 'node:module';
import {pathToFileURL} from 'node:url';
const require=createRequire(import.meta.url),modulePath=process.env.TRACE_CLOAKBROWSER_NODE_MODULE;
assert.ok(modulePath,'TRACE_CLOAKBROWSER_NODE_MODULE must identify fixed installed SDK');
const entry=require.resolve(modulePath);let dir=path.dirname(entry),manifest;
for(let i=0;i<5;i++){const file=path.join(dir,'package.json');if(fs.existsSync(file)){const p=JSON.parse(fs.readFileSync(file));if(p.name==='cloakbrowser'){manifest=p;break}}dir=path.dirname(dir)}
assert.equal(manifest?.version,'0.6.4');assert.ok(process.env.CLOAKBROWSER_BINARY_PATH);assert.ok(fs.existsSync(process.env.CLOAKBROWSER_BINARY_PATH));
const {launch}=await import(pathToFileURL(entry));
const fixture=JSON.parse(process.env.TRACE_MEDIA_FIXTURE),{base,run,hash,requests}=fixture;
const browser=await launch({headless:false,args:['--no-sandbox']});
const context=await browser.newContext({acceptDownloads:true}),page=await context.newPage(),errors=[];
page.on('pageerror',error=>errors.push(error.message));
await context.addInitScript(()=>{
  const create=URL.createObjectURL.bind(URL),revoke=URL.revokeObjectURL.bind(URL);window.__mediaURLs={created:[],revoked:[]};
  URL.createObjectURL=blob=>{const url=create(blob);window.__mediaURLs.created.push(url);return url};
  URL.revokeObjectURL=url=>{window.__mediaURLs.revoked.push(url);return revoke(url)};
});
const target=base+'/api/artifacts/'+hash+'/download';
const open=async()=>{await page.goto(base+'/#history/'+run+'?tab=artifacts');await page.locator('#artifact-records [data-action=artifact]').first().waitFor()};
const preview=()=>page.locator('#artifact-records [data-action=artifact]').first().click();
let releaseResponse=()=>{};
try{
  await open();await preview();
  await page.waitForFunction(()=>{const img=document.querySelector('#detail-dialog img.artifact-media');return img?.complete&&img.naturalWidth===1&&img.naturalHeight===1});
  const text=await page.locator('#dialog-content').innerText();for(const request of requests)assert.ok(text.includes(request));assert.ok(text.includes('shared-call'));
  const downloadPromise=page.waitForEvent('download');await page.locator('#dialog-content a[download]').click();const download=await downloadPromise;
  assert.equal(await download.failure(),null);assert.equal(createHash('sha256').update(fs.readFileSync(await download.path())).digest('hex'),hash);
  await page.locator('#close-dialog').click();await page.waitForFunction(()=>window.__mediaURLs.created.every(url=>window.__mediaURLs.revoked.includes(url)));
  // Delay an actual HTTP response, not a synthetic snapshot or media payload.
  let entered,rejectObserved,release,finished;const observed=new Promise((r,j)=>{entered=r;rejectObserved=j}),gate=new Promise(r=>release=r),completed=new Promise(r=>finished=r);releaseResponse=release;observed.catch(()=>{});
  await page.route(target,async route=>{try{const response=await route.fetch({timeout:10000});entered();await gate;try{await route.fulfill({response})}catch{}}catch(error){rejectObserved(error)}finally{finished()}});
  await preview();await observed;const before=await page.evaluate(()=>window.__mediaURLs.created.length);
  await page.evaluate(()=>location.hash='#overview');await page.waitForFunction(async()=>{const {state}=await import('/app.js');return location.hash==='#overview'&&state.page==='overview'});release();
  await completed;await page.evaluate(()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve))));assert.equal(await page.evaluate(()=>window.__mediaURLs.created.length),before,'late old scope allocated a blob');
  assert.equal(await page.locator('#detail-dialog img.artifact-media').count(),0);await page.unroute(target);
  if(await page.locator('#detail-dialog').evaluate(el=>el.open))await page.locator('#close-dialog').click();
  await open();
  // Actual missing CAS endpoint drives the real UI failure branch.
  await page.route(target,async route=>{const response=await route.fetch({url:base+'/api/artifacts/'+'0'.repeat(64)+'/download'});assert.equal(response.status(),404);await route.fulfill({response})});
  await preview();await page.waitForFunction(()=>document.querySelector('#dialog-content .inline-error')?.textContent.includes('404'));
  assert.equal(await page.locator('#dialog-content a[download]').count(),1);assert.equal(await page.locator('#dialog-content img').count(),0);
  assert.deepEqual(errors,[]);console.log(JSON.stringify({result:'PASS',chain:'Agent -> CAS -> reopened Store -> HTTP -> actual UI',sources:requests.length,hash,headed:true,limitations:['Fixed CloakBrowser suppresses pageerror events; empty events do not prove zero exceptions'],checks:['natural dimensions','multi-source','download hash','blob revoke','late scope HTTP','real HTTP 404']}));
}finally{releaseResponse();try{await context.close()}finally{await browser.close()}}
