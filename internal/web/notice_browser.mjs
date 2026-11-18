// Narrow desktop acceptance over the existing real Store/HTTP/SSE fixture.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import {createRequire} from 'node:module';
import {pathToFileURL} from 'node:url';
const require=createRequire(import.meta.url),sdk=process.env.TRACE_CLOAKBROWSER_NODE_MODULE;
assert.ok(process.env.DISPLAY&&process.env.CLOAKBROWSER_BINARY_PATH);
assert.equal(JSON.parse(fs.readFileSync(path.join(sdk,'package.json'),'utf8')).version,'0.6.4');
const {launch}=await import(pathToFileURL(require.resolve(sdk)));
const {base,ids,output,sqlite}=JSON.parse(process.env.TRACE_P0_FIXTURE);
const browser=await launch({headless:false,args:['--no-sandbox','--window-size=1440,1000']});
try{
 const context=await browser.newContext({viewport:{width:1440,height:1000}}),page=await context.newPage();
 await context.grantPermissions(['clipboard-read','clipboard-write']);
 await page.goto(base+'/#history/'+ids[0]);
 await page.waitForSelector('#workspace-notice>#notice',{state:'attached'});
 await page.evaluate(()=>{window.__notice=document.getElementById('notice')});
 await page.locator('[data-action="copy-link"]').first().click();
 await page.waitForFunction(()=>!document.getElementById('notice').hidden);
 const position=await page.evaluate(()=>{
  const n=document.getElementById('notice').getBoundingClientRect(),c=document.querySelector('.workspace-panel').getBoundingClientRect();
  return {notice:{left:n.left,top:n.top},conversation:{right:c.right,top:c.top}};
 });
 assert.ok(position.notice.left>=position.conversation.right,'notice must sit beside the conversation');
 assert.ok(Math.abs(position.notice.top-position.conversation.top)<4,'notice must align with the conversation top');
 await page.evaluate(async id=>{const r=await fetch('/_p0/report?id='+id,{method:'POST'});if(!r.ok)throw Error('fixture update failed')},ids[0]);
 await page.waitForFunction(()=>document.querySelector('.workspace-secondary')?.textContent.includes('task-second-report'));
 assert.equal(await page.evaluate(()=>document.querySelectorAll('#notice').length===1&&document.getElementById('notice')===window.__notice&&document.getElementById('notice').parentElement.id==='workspace-notice'),true);
 await page.locator('[data-action="archive-back"]').click();
 await page.waitForSelector('#notice-home>#notice',{state:'attached'});
 assert.equal(await page.evaluate(()=>document.getElementById('notice')===window.__notice),true);
 await page.locator(`[data-action="select"][data-id="${ids[0]}"]`).first().click();
 await page.waitForSelector('#workspace-notice>#notice',{state:'attached'});
 assert.equal(await page.evaluate(()=>document.querySelectorAll('#notice').length===1&&document.getElementById('notice')===window.__notice&&document.getElementById('notice').getAttribute('aria-live')==='polite'&&document.getElementById('notice').getAttribute('role')==='status'),true);
 await page.waitForFunction(()=>window.__notice.hidden);
 await page.screenshot({path:path.join(output,'notice-desktop.png')});
 fs.writeFileSync(path.join(output,'notice-browser.json'),JSON.stringify({passed:true,sqlite,position,sse_repaint:true,navigation_identity:true,unique_live_region:true},null,2));
 console.log('Notice: desktop position, SSE repaint, navigation and live-region identity passed.');
}finally{await browser.close()}
