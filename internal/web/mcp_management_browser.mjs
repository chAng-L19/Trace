// Server-only acceptance; isolated mock Provider, state, config and workspace.
import fs from 'node:fs';
import path from 'node:path';
import {spawn} from 'node:child_process';
import assert from 'node:assert/strict';
const {chromium}=await import(process.env.TRACE_PLAYWRIGHT);
const root=process.cwd(),dir=path.join(root,'../trace-test-archive-20261028/validation/mcp-dialog-20261027'),base='http://127.0.0.1:18767';
fs.mkdirSync(dir,{recursive:true});
const fixture=path.join(dir,'fixture.py'),configPath=path.join(dir,'config.json');
fs.writeFileSync(fixture,[
 'import sys,json',
 'for line in sys.stdin:',
 ' r=json.loads(line)',
 ' if "id" not in r: continue',
 ' m=r.get("method")',
 ' result={"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"fixture","version":"1"}} if m=="initialize" else {"tools":[{"name":"echo","description":"Fixture echo","inputSchema":{"type":"object","properties":{}}}]} if m=="tools/list" else {}',
 ' print(json.dumps({"jsonrpc":"2.0","id":r["id"],"result":result}),flush=True)'
].join(String.fromCharCode(10)));
fs.writeFileSync(configPath,JSON.stringify({provider:{},context:{},mcp:[],local_tools:[],skill_paths:[]}));
const out=fs.openSync(path.join(dir,'isolated-service.log'),'w');
const server=spawn(path.join(root,'trace-mcp'),['serve','--addr','127.0.0.1:18767','--config',configPath],{cwd:root,env:{...process.env,TRACE_DATA_DIR:path.join(dir,'state'),TRACE_WORKSPACE:path.join(dir,'workspace')},stdio:['ignore',out,out]});
let browser,page;
try{
 for(let n=0;n<100;n++){try{if((await fetch(base)).ok)break}catch{}await new Promise(r=>setTimeout(r,100))}
 browser=await chromium.launch({executablePath:process.env.TRACE_CHROMIUM,headless:true,args:['--no-sandbox']});
 page=await browser.newPage({viewport:{width:1440,height:1000}});const errors=[];page.on('pageerror',e=>errors.push(e.message));
 await page.goto(base+'/#mcp');await page.locator('[data-action="mcp-new"]').waitFor();
 assert.equal(await page.locator('#page #mcp-editor,#page #mcp-form,#page #integration-form').count(),0);
 await page.locator('[data-action="mcp-new"]').click();await page.locator('#detail-dialog[open] #mcp-name').waitFor({state:'visible'});
 assert.equal(await page.locator('#dialog-title').innerText(),'添加 MCP 服务');
 await page.locator('#mcp-name').fill('discard-me');await page.locator('[data-action="dialog-cancel"]').click();
 await page.locator('[data-action="mcp-new"]').click();assert.equal(await page.locator('#mcp-name').inputValue(),'');
 console.log('PASS one page add tile, direct modal, cancel clears draft and no duplicate editor');
 assert.equal(await page.locator('#integration-form').count(),1);
 await page.locator('#mcp-source').selectOption('github');await page.locator('#integration-url').waitFor({state:'visible'});
 assert.equal(await page.locator('#mcp-form').isVisible(),false);
 await page.locator('#mcp-source').selectOption('connect');assert.equal(await page.locator('#integration-form').isVisible(),false);
 await page.locator('#mcp-name').fill(' fixture ');await page.locator('#mcp-command').fill('python3');await page.locator('#mcp-args').fill(['-u',fixture].join(String.fromCharCode(10)));
 await page.locator('#mcp-env').fill('TOKEN=fixture-secret');await page.locator('#mcp-form button.primary').click();
 await page.waitForFunction(()=>document.querySelectorAll('.mcp-management .mcp-service').length===1);
 await page.waitForFunction(()=>!document.getElementById('detail-dialog').open);
 await page.locator('[data-action="mcp-detail"]').first().click();await page.locator('[data-action="mcp-edit"]').click();
 await page.locator('#close-dialog').click();await page.locator('a[href="#models"]').click();await page.locator('#provider-model').waitFor();await page.locator('a[href="#mcp"]').click();await page.locator('[data-action="mcp-detail"]').first().click();await page.locator('[data-action="mcp-edit"]').click();await page.locator('#mcp-command').waitFor({state:'visible'});
 assert.equal(await page.locator('#mcp-command').inputValue(),'python3');assert.match(await page.locator('#mcp-env').inputValue(),/stored secret/);
 await page.locator('#mcp-description').fill('second save');await page.locator('#mcp-form button.primary').click();
 await page.waitForFunction(()=>document.querySelector('.mcp-management .mcp-service p')?.textContent==='second save');
 console.log('PASS single add entry, exclusive sources, consecutive saves and real stdio discovery');
 await page.locator('[data-action="mcp-new"]').first().click();await page.locator('#mcp-name').fill('fixture');await page.locator('#mcp-command').fill('python3');await page.locator('#mcp-form button.primary').click();
 await page.getByText('同名 MCP 服务已存在，请从服务详情进入编辑连接。',{exact:true}).first().waitFor();
 assert.equal(JSON.parse(fs.readFileSync(configPath)).mcp.length,1);console.log('PASS duplicate name does not overwrite');
 await page.locator('#close-dialog').click();await page.locator('#advanced-tool-config summary').click();
 await page.locator('[data-action="mcp-toggle"]').click();await page.waitForFunction(()=>document.querySelector('.mcp-management .mcp-service .status')?.textContent==='已禁用');
 const json=JSON.parse(await page.locator('#mcp-json').inputValue());assert.equal(json[0].disabled,true);console.log('PASS pristine JSON synchronizes after stop');
 await page.locator('#mcp-json').fill((await page.locator('#mcp-json').inputValue())+' ');
 await page.locator('[data-action="mcp-toggle"]').click();await page.waitForFunction(()=>document.getElementById('mcp-json').dataset.configConflict==='true');
 await page.locator('#mcp-json').fill((await page.locator('#mcp-json').inputValue())+' ');
 await page.locator('a[href="#models"]').click();await page.locator('#provider-model').waitFor();await page.locator('a[href="#mcp"]').click();await page.locator('#mcp-json').waitFor({state:'attached'});
 assert.equal(await page.locator('#mcp-json').getAttribute('data-config-conflict'),'true');
 await page.locator('#config-form button.primary').click();await page.getByText(/配置已被其他操作更新/).first().waitFor();assert.equal(!!JSON.parse(fs.readFileSync(configPath)).mcp[0].disabled,false);
 console.log('PASS stale JSON stays blocked after further input and navigation');
 await page.locator('[data-action="discard-config"]').click();assert.equal(await page.locator('#mcp-json').getAttribute('data-config-conflict'),null);
 await page.locator('[data-action="mcp-detail"]').click();await page.locator('[data-action="mcp-edit"]').click();
 assert.equal(await page.locator('#mcp-source').isDisabled(),true);assert.equal(await page.locator('#mcp-name').getAttribute('readonly'),'');
 assert.match(await page.locator('#mcp-env').inputValue(),/stored secret/);
 await page.locator('#mcp-command').fill('/missing/trace-fixture-command');await page.locator('#mcp-form button.primary').click();
 await page.getByText(/原配置与 Agent 已保留/).first().waitFor();assert.equal(JSON.parse(fs.readFileSync(configPath)).mcp[0].command,'python3');
 assert.equal(JSON.parse(fs.readFileSync(configPath)).mcp[0].env.TOKEN,'fixture-secret');console.log('PASS failed connection preserves enabled config and stored secret');
 await page.locator('#close-dialog').click();await page.locator('[data-action="mcp-new"]').click();await page.locator('#mcp-name').fill('http-fixture');await page.locator('#mcp-transport').selectOption('http');await page.locator('#mcp-url').fill('http://127.0.0.1:1/mcp');await page.locator('#mcp-headers').fill('Authorization=Bearer fixture-http-secret');await page.locator('#mcp-enabled').uncheck();await page.locator('#mcp-form button.primary').click();
 await page.waitForFunction(()=>!document.getElementById('detail-dialog').open);
 await page.locator('a[href="#models"]').click();await page.locator('#provider-model').waitFor();await page.locator('a[href="#mcp"]').click();await page.locator('[data-action="mcp-detail"][data-service="http-fixture"]').click();await page.locator('[data-action="mcp-edit"]').click();await page.locator('#mcp-url').waitFor({state:'visible'});
 assert.equal(await page.locator('#mcp-enabled').isChecked(),false);assert.match(await page.locator('#mcp-headers').inputValue(),/stored secret/);await page.locator('#mcp-description').fill('HTTP persisted');await page.locator('#mcp-form button.primary').click();
 await page.waitForFunction(()=>document.querySelector('.mcp-management')?.textContent.includes('HTTP persisted'));const http=JSON.parse(fs.readFileSync(configPath)).mcp.find(m=>m.name==='http-fixture');assert.equal(http.disabled,true);assert.equal(http.headers.Authorization,'Bearer fixture-http-secret');console.log('PASS HTTP edit survives navigation, disabled state and stored headers retained');
 await page.waitForFunction(()=>!document.getElementById('detail-dialog').open);await page.locator('#workspace').evaluate(el=>el.scrollTop=0);
 assert.equal(await page.locator('#page #mcp-editor,#page #mcp-form').count(),0);
 for(const theme of ['paper','white','black']){await page.locator('#theme-select').selectOption(theme);await page.screenshot({path:path.join(dir,theme+'.png'),fullPage:true})}
 await page.locator('[data-action="mcp-new"]').click();await page.locator('#mcp-name').fill('escape-discard');await page.keyboard.press('Escape');await page.locator('[data-action="mcp-new"]').click();assert.equal(await page.locator('#mcp-name').inputValue(),'');
 for(const theme of ['paper','white','black']){await page.evaluate(value=>{document.documentElement.dataset.theme=value},theme);await page.screenshot({path:path.join(dir,'dialog-'+theme+'.png'),fullPage:true})}
 await page.locator('#close-dialog').click();assert.equal(await page.locator('#detail-dialog').evaluate(el=>el.open),false);console.log('PASS Escape and close button discard modal fields without page editor');
 assert.deepEqual(errors,[]);console.log('PASS paper/white/black render; pageerror delivery is unavailable in the fixed CloakBrowser binary, so empty events do not prove zero exceptions');
}catch(error){if(page){console.log((await page.locator('#page').innerText()).slice(-5000));console.log(await page.locator('#mcp-original').inputValue());await page.screenshot({path:path.join(dir,'failure.png'),fullPage:true})}throw error}finally{if(browser)await browser.close();server.kill('SIGINT');await new Promise(r=>server.once('exit',r));fs.closeSync(out)}
