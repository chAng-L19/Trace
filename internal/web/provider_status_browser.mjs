// Server-only isolated provider fixture; never reads production credentials.
import fs from 'node:fs';
import path from 'node:path';
import http from 'node:http';
import {spawn} from 'node:child_process';
import assert from 'node:assert/strict';
const {chromium}=await import(process.env.TRACE_PLAYWRIGHT);
const root=process.cwd(),dir=path.join(root,'../trace-test-archive-20261028/validation/provider-status-20261027'),base='http://127.0.0.1:18769';
fs.mkdirSync(dir,{recursive:true});
let requests=0;
const provider=http.createServer(async(req,res)=>{let data='';for await(const part of req)data+=part;const body=JSON.parse(data);requests++;res.setHeader('Content-Type','application/json');res.end(JSON.stringify({id:'probe-fixture',object:'chat.completion',model:body.model,choices:[{index:0,message:{role:'assistant',content:'OK'},finish_reason:'stop'}],usage:{prompt_tokens:2,completion_tokens:1,total_tokens:3}}))});
await new Promise(r=>provider.listen(18770,'127.0.0.1',r));
const configPath=path.join(dir,'config.json');fs.writeFileSync(configPath,JSON.stringify({provider:{url:'http://127.0.0.1:18770/v1',model:'configured-model',api_key:'fixture-private'},context:{},mcp:[],local_tools:[],skill_paths:[]}));
const out=fs.openSync(path.join(dir,'isolated-service.log'),'w');
const server=spawn(path.join(root,'trace-provider'),['serve','--addr','127.0.0.1:18769','--config',configPath],{cwd:root,env:{...process.env,TRACE_DATA_DIR:path.join(dir,'state'),TRACE_WORKSPACE:path.join(dir,'workspace')},stdio:['ignore',out,out]});
let browser,page;
try{
 for(let n=0;n<100;n++){try{if((await fetch(base)).ok)break}catch{}await new Promise(r=>setTimeout(r,100))}
 browser=await chromium.launch({executablePath:process.env.TRACE_CHROMIUM,headless:true,args:['--no-sandbox']});page=await browser.newPage({viewport:{width:1440,height:1000}});
 const errors=[];page.on('pageerror',e=>errors.push(e.message));
 await page.goto(base+'/#models');await page.locator('#provider-model').waitFor();
 assert.match(await page.locator('#model-connection').innerText(),/configured-model.*未检测/);assert.equal(requests,0);
 await page.locator('#provider-probe-button').click();await page.waitForFunction(()=>document.getElementById('model-connection').dataset.status==='success');
 assert.match(await page.locator('#model-connection').innerText(),/configured-model.*检测通过/);
 for(const route of ['#overview','#mcp','#models']){await page.locator('nav a[href="'+route+'"]').click();await page.waitForFunction(()=>document.getElementById('model-connection').dataset.status==='success');assert.match(await page.locator('#model-connection').innerText(),/configured-model/)}
 await page.locator('[data-action="reload-settings"]').click();await page.waitForFunction(()=>document.getElementById('model-connection').dataset.status==='success');
 assert.equal(requests,1);console.log('PASS saved model name and completed result survive navigation, background repaint and equivalent config reload without extra probes');
 await page.locator('#provider-model').fill('draft-model');await page.locator('#provider-key').fill('new-fixture-private');
 await page.locator('#provider-probe-button').click();await page.waitForFunction(()=>document.getElementById('model-connection').dataset.status==='success'&&document.getElementById('model-connection').textContent.includes('未保存'));
 assert.match(await page.locator('#model-connection').innerText(),/draft-model · 草稿/);assert.equal(JSON.parse(fs.readFileSync(configPath)).provider.model,'configured-model');
 console.log('PASS detected draft name and success explicitly shown as unsaved, production config untouched');
 await page.locator('#config-form button.primary').click();await page.waitForFunction(()=>document.getElementById('model-connection').dataset.status==='success'&&!document.getElementById('model-connection').textContent.includes('草稿'));
 assert.match(await page.locator('#model-connection').innerText(),/draft-model.*检测通过/);assert.equal(JSON.parse(fs.readFileSync(configPath)).provider.model,'draft-model');
 await page.locator('nav a[href="#overview"]').click();assert.match(await page.locator('#model-connection').innerText(),/draft-model.*检测通过/);assert.equal(requests,2);
 console.log('PASS probe then save preserves result after key redaction and navigation');
 await page.screenshot({path:path.join(dir,'success.png'),fullPage:true});
 await page.locator('nav a[href="#models"]').click();await page.locator('#provider-model').fill('untested-model');await page.locator('#config-form button.primary').click();
 await page.waitForFunction(()=>document.getElementById('model-connection').textContent.includes('untested-model')&&document.getElementById('model-connection').textContent.includes('未检测'));
 assert.equal(requests,2);console.log('PASS changed untested saved model invalidates previous success without sending requests');
 for(const theme of ['paper','white','black']){await page.locator('#theme-select').selectOption(theme);await page.screenshot({path:path.join(dir,theme+'.png'),fullPage:true})}
 assert.doesNotMatch(await page.locator('#model-connection').getAttribute('title'),/fixture-private/);assert.deepEqual(errors,[]);console.log('PASS three themes and no credential text in header; pageerror delivery is unavailable in the fixed CloakBrowser binary, so empty events do not prove zero exceptions');
}catch(error){if(page){console.log(await page.locator('#model-connection').innerText());console.log(await page.locator('#provider-probe-status').innerText());await page.screenshot({path:path.join(dir,'failure.png'),fullPage:true})}throw error}
finally{if(browser)await browser.close();server.kill('SIGINT');await new Promise(r=>server.once('exit',r));await new Promise(r=>provider.close(r));fs.closeSync(out)}
