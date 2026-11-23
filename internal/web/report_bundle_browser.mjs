// Server-only module harness: production renderer + delivery controls + real CAS/ZIP HTTP.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import {createHash} from 'node:crypto';
import {createRequire} from 'node:module';
import {pathToFileURL} from 'node:url';

const require=createRequire(import.meta.url),sdk=process.env.TRACE_CLOAKBROWSER_NODE_MODULE;
assert.ok(sdk,'fixed installed CloakBrowser SDK is required');
const entry=require.resolve(sdk);
let directory=path.dirname(entry),manifest;
for(let i=0;i<5;i++){
  const file=path.join(directory,'package.json');
  if(fs.existsSync(file)){const pkg=JSON.parse(fs.readFileSync(file));if(pkg.name==='cloakbrowser'){manifest=pkg;break}}
  directory=path.dirname(directory);
}
assert.equal(manifest?.version,'0.6.4');
assert.ok(process.env.CLOAKBROWSER_BINARY_PATH);
assert.ok(fs.existsSync(process.env.CLOAKBROWSER_BINARY_PATH));
assert.equal(process.env.CLOAKBROWSER_AUTO_UPDATE,'false');
const fixture=JSON.parse(process.env.TRACE_REPORT_FIXTURE);
assert.ok(path.isAbsolute(fixture.screenshot));assert.ok(path.isAbsolute(fixture.download));
const {launch}=await import(pathToFileURL(entry));
const browser=await launch({headless:false,args:['--no-sandbox']});
const context=await browser.newContext({acceptDownloads:true}),page=await context.newPage();
page.setDefaultTimeout(20000);page.setDefaultNavigationTimeout(20000);
const pageErrors=[];
page.on('pageerror',error=>pageErrors.push(error.message));
try{
  await page.goto(fixture.base+'/report-bundle-fixture');
  const imageResponse=page.waitForResponse(response=>response.url()===fixture.base+'/api/artifacts/'+fixture.hash+'/download?preview=1');
  // Import and await the actual modules. A failed import, render or dispatch
  // rejects evaluate; pageerror events alone are not an exception oracle.
  await page.evaluate(async fixture=>{
    const {markdown}=await import('/presentation.js');
    const {reportDownloads,createDelivery}=await import('/delivery.js');
    const params=new URLSearchParams({format:'report',report_id:fixture.report.id,language:'zh-CN'});
    const response=await fetch('/api/runs/'+encodeURIComponent(fixture.run)+'/export?'+params);
    if(!response.ok)throw new Error('archived report HTTP '+response.status);
    const body=await response.text();
    if(!body.includes('中文截图报告'))throw new Error('Chinese archived body missing');
    document.querySelector('#report').innerHTML=markdown(body);
    const state={selected:fixture.run,snap:{run:{id:fixture.run},reports:[{...fixture.report,source_status:'current'}]}};
    const controls=document.querySelector('#downloads');
    controls.innerHTML=reportDownloads(state.snap);
    const {dispatch}=createDelivery({state,token:()=>({nav:0}),notice:text=>document.querySelector('#notice').textContent=text});
    controls.addEventListener('click',event=>{
      const button=event.target.closest('button[data-action="export"]');
      if(button)window.__reportDownload=dispatch('export',button);
    });
    window.__reportRendered=true;
  },fixture);
  await page.waitForFunction(()=>{
    const img=document.querySelector('#report img.artifact-media');
    return window.__reportRendered&&img?.complete&&img.naturalWidth===2&&img.naturalHeight===2;
  });
  assert.match(await page.locator('#report').innerText(),/中文截图报告/);
  const image=await imageResponse;
  assert.equal(image.status(),200);
  const imageHeaders=image.headers();
  assert.equal(imageHeaders['content-type'],'image/png');
  assert.match(imageHeaders['content-disposition'],/^inline;/);
  assert.equal(imageHeaders['x-content-type-options'],'nosniff');
  const digest=data=>createHash('sha256').update(data).digest('hex');
  assert.equal(digest(await image.body()),fixture.hash);
  await page.screenshot({path:fixture.screenshot,fullPage:true});
  const apiPath='/api/runs/'+encodeURIComponent(fixture.run)+'/export';
  const responsePromise=page.waitForResponse(response=>{
    const url=new URL(response.url());
    return url.pathname===apiPath&&url.searchParams.get('format')==='report_bundle'&&url.searchParams.get('report_id')===fixture.report.id&&url.searchParams.get('language')==='zh-CN';
  });
  const downloadPromise=page.waitForEvent('download');
  await page.locator('#downloads [data-format="report_bundle"][data-language="zh-CN"]').click();
  const download=await downloadPromise,response=await responsePromise;
  assert.equal(await page.evaluate(()=>window.__reportDownload),true);
  assert.equal(response.status(),200);assert.equal(response.headers()['content-type'],'application/zip');
  assert.equal(await download.failure(),null);
  assert.equal(download.suggestedFilename(),fixture.report.id+'.zh-CN.zip');
  await download.saveAs(fixture.download);
  const saved=fs.readFileSync(fixture.download);
  assert.equal(saved.subarray(0,2).toString(),'PK');
  assert.equal(digest(saved),digest(await response.body()),'browser saved different ZIP bytes');
  assert.ok(fs.statSync(fixture.screenshot).size>0);
  if(pageErrors.length)throw new Error('Observed page exceptions: '+pageErrors.join('; '));
  console.log(JSON.stringify({result:'PASS',headed:true,run:fixture.run,report_id:fixture.report.id,screenshot:fixture.screenshot,download:fixture.download,checks:['actual production Markdown module','Chinese archived body','CAS image naturalWidth/naturalHeight','sniffed PNG headers and hash','actual language/version ZIP click and saved bytes','Go caller validates ZIP members and original PNG'],limitations:['Module harness, not full application navigation','Fixed CloakBrowser pageerror delivery is incomplete; empty events do not establish absence of JavaScript exceptions']}));
}finally{
  try{await context.close()}finally{await browser.close()}
}
