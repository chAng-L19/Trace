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
  const n=document.querySelector('#workspace-notice>#notice'),h=document.querySelector('.workspace-dialogue-head');
  const box=el=>el.getBoundingClientRect().toJSON();
  return {notice:box(n),head:box(h),pane:box(document.querySelector('.conversation')),editor:box(document.querySelector('.task-inspector .workspace-composer')),
   first:h.firstElementChild===n.parentElement,hit:n.contains(document.elementFromPoint(box(n).left+20,box(n).top+8))};
 });
 assert.ok(position.first&&position.hit,'visible live notice must be the first, hittable item in the reader');
 assert.ok(Math.abs(position.notice.left-position.pane.left)<=2&&Math.abs(position.notice.right-position.pane.right)<=2);
 assert.ok(Math.abs(position.notice.top-position.head.top)<=2&&position.notice.right<=position.editor.left);
 assert.ok(position.notice.height>0&&position.notice.height<=40,'single-line notice must be compact');
 await page.evaluate(async id=>{const r=await fetch('/_p0/report?id='+id,{method:'POST'});if(!r.ok)throw Error('fixture update failed')},ids[0]);
 const flow=await page.evaluate(()=>{
  const owner=document.getElementById('workspace'),notice=document.getElementById('notice');
  const top=owner.scrollTop,y=notice.getBoundingClientRect().top;
  owner.scrollTop=top+80;
  const scroll=owner.scrollTop-top,move=y-notice.getBoundingClientRect().top;
  owner.scrollTop=top;
  return {scroll,move};
 });
 assert.ok(flow.scroll>=40&&Math.abs(flow.move-flow.scroll)<=2,'the inline notice must scroll with the reader');
 await page.waitForFunction(()=>document.querySelector('.workspace-secondary')?.textContent.includes('task-second-report'));
 assert.equal(await page.evaluate(()=>document.querySelectorAll('#notice').length===1&&document.getElementById('notice')===window.__notice&&document.getElementById('notice').parentElement.id==='workspace-notice'),true);
 await page.locator('[data-action="archive-back"]').click();
 await page.waitForSelector('#notice-home>#notice',{state:'attached'});
 assert.equal(await page.evaluate(()=>document.getElementById('notice')===window.__notice),true);
 await page.locator(`[data-action="select"][data-id="${ids[0]}"]`).first().click();
 await page.waitForSelector('#workspace-notice>#notice',{state:'attached'});
 assert.equal(await page.evaluate(()=>document.querySelectorAll('#notice').length===1&&document.getElementById('notice')===window.__notice&&document.getElementById('notice').getAttribute('aria-live')==='polite'&&document.getElementById('notice').getAttribute('role')==='status'),true);
 await page.waitForFunction(()=>window.__notice.hidden);
 await page.waitForSelector('.task-inspector #message-form');
 const hidden=await page.locator('#workspace-notice').evaluate(el=>({host:el.offsetHeight,notice:el.firstElementChild.offsetHeight}));
 assert.deepEqual(hidden,{host:0,notice:0},'a hidden notice reserves no space');
 await page.screenshot({path:path.join(output,'notice-desktop.png')});
 fs.writeFileSync(path.join(output,'notice-browser.json'),JSON.stringify({passed:true,sqlite,position,flow,hidden,sse_repaint:true,navigation_identity:true,unique_live_region:true},null,2));
 console.log('Notice: desktop position, SSE repaint, navigation and live-region identity passed.');
}finally{await browser.close()}
