import assert from 'node:assert/strict';
import path from 'node:path';
import {enlargeExplorationForOuterScroll} from './exploration_browser_fixture.mjs';

const edgeSelector='[data-action=exploration-chain][data-from="task:todo-ready"][data-to="task:todo-dependent"]';
const shot=(page,output,name)=>page.screenshot({path:path.join(output,`workspace-${name}.png`)});

async function graphs({page,base,id,output,frame,stage,result,change}){
 for(const [name,selector] of [['preview','.exploration-preview'],['full','.exploration-view']]){
  stage(name+' graph resize and selection');
  if(name==='full')await page.locator('#task-tab-assets').click();
  const root=page.locator(selector),canvas=root.locator('.graph-shell'),edge=root.locator(edgeSelector);
  await canvas.scrollIntoViewIfNeeded();
  const before=await canvas.boundingBox(),x=before.x+before.width-5,y=before.y+before.height-5;
  await page.mouse.move(x,y);await page.mouse.down();await page.mouse.move(x,y+110,{steps:12});await page.mouse.up();await frame(page);
  const after=await canvas.boundingBox();
  assert.ok(after.height>=before.height+70,`native resize: ${before.height} to ${after.height}`);
  await edge.locator('.edge-hit').evaluate(el=>{
   const canvas=el.closest('.graph-shell'),r=canvas.getBoundingClientRect();
   const p=el.getPointAtLength(el.getTotalLength()/2).matrixTransform(el.getScreenCTM());
   canvas.scrollLeft+=p.x-r.left-canvas.clientWidth/2;
   canvas.scrollTop+=p.y-r.top-canvas.clientHeight/2;
  });
  await frame(page);
  const point=await edge.locator('.edge-hit').evaluate(el=>{
   const p=el.getPointAtLength(el.getTotalLength()/2).matrixTransform(el.getScreenCTM());
   return {x:p.x,y:p.y,hit:document.elementFromPoint(p.x,p.y)?.closest('[data-action="exploration-chain"]')===el.parentElement};
  });
  assert.ok(point.hit,`edge pointer hit: ${JSON.stringify(point)}`);
  await page.mouse.click(point.x,point.y);assert.equal(await edge.getAttribute('aria-pressed'),'true');
  const muted=await root.locator('.exploration-edge.chain-muted').count();assert.ok(muted>0);
  await shot(page,output,name+'-highlight');
  await root.locator('[data-command=clear]').click();await frame(page);
  assert.equal(await edge.getAttribute('aria-pressed'),'false');
  const selected=pressed=>page.waitForFunction(({selector,pressed})=>document.querySelector(selector)?.getAttribute('aria-pressed')===pressed,{selector:selector+' '+edgeSelector,pressed});
  await edge.focus();
  const focused=()=>edge.evaluate(el=>el===document.activeElement);
  assert.ok(await focused());
  await page.keyboard.press('Enter');await selected('true');assert.ok(await focused());
  await page.keyboard.press('Escape');await selected('false');assert.ok(await focused());
  await page.keyboard.press('Space');await selected('true');assert.ok(await focused());
  const report=await fetch(base+'/_p0/report?id='+id,{method:'POST'});assert.equal(report.status,204);
  const reportNodes=selector+(name==='preview'?' a.child_report':' .exploration-node.child_report');
  await page.waitForFunction(({reportNodes,count})=>document.querySelectorAll(reportNodes).length===count,{reportNodes,count:name==='preview'?2:3});
  assert.equal(await edge.getAttribute('aria-pressed'),'true','same edge lost selection on refresh');
  const refreshed=(await canvas.boundingBox()).height;
  assert.ok(Math.abs(refreshed-after.height)<=2,'graph lost resized height on refresh');
  result[name]={before:before.height,after:after.height,refreshed,muted,pointerClear:true,keyboardClear:true,sameRelationPreserved:true};
 }
 stage('removed relation clears selection');
 await change('remove-dependency');
 async function cleared(root){
  await root.locator(edgeSelector).waitFor({state:'detached'});
  await root.locator('.exploration-edge[data-from="goal"][data-to="task:todo-dependent"]').waitFor();
  assert.equal(await root.locator('.chain-muted').count(),0);
  assert.ok(await root.locator('[data-command=clear]').isDisabled());
 }
 await cleared(page.locator('.exploration-view'));
 await page.locator('#task-tab-activity').click();
 await cleared(page.locator('.exploration-preview'));
 result.removal={fullCleared:true,previewCleared:true};
}

async function geometry(page){
 return page.evaluate(()=>{
  const h=document.querySelector('.workspace-dialogue-head'),pane=h.nextElementSibling,owner=document.querySelector('#workspace');
  const title=h.querySelector('.workspace-heading').getBoundingClientRect(),header=h.getBoundingClientRect(),body=pane.getBoundingClientRect(),tabs=document.querySelector('.task-tabs')?.getBoundingClientRect();
  const graph=document.querySelector('.exploration-preview .graph-shell')?.getBoundingClientRect(),input=document.querySelector('.task-inspector #message-content');
  const hit=(el,r,top)=>{const bottom=Math.min(r.bottom,innerHeight);return bottom>top&&el.contains(document.elementFromPoint(r.left+r.width/2,(top+bottom)/2))};
  return {title:title.toJSON(),headHit:hit(h,title,title.top),headerTop:header.top,tabsBottom:tabs?.bottom,headHeight:header.height,
   layout:h.firstElementChild.id==='workspace-notice'&&pane.matches('.conversation')&&!h.querySelector('form,input,textarea,select,#run-mailbox'),
   scroll:pane.scrollTop,outer:owner.scrollTop,outerMax:owner.scrollHeight-owner.clientHeight,paneHeight:body.height,
   bodyHit:hit(pane,body,Math.max(header.bottom,body.top)),graphHit:graph&&hit(document.querySelector('.exploration-preview .graph-shell'),graph,Math.max(header.bottom,graph.top)),
   paging:!!h.querySelector('.conversation-pages'),input:input?.getBoundingClientRect().toJSON()};
 });
}

function sameHeading(before,after,stage){
 assert.ok(after.layout&&after.headHit,stage+': reader heading is covered or contains editor controls');
 for(const k of ['x','y','width','height'])assert.ok(Math.abs(before.title[k]-after.title[k])<=2,stage+': heading '+k+' moved');
 assert.ok(Math.abs(before.paneHeight-after.paneHeight)<=1,stage+': transcript shrank');
}

export async function workspaceInteractions({page,base,id,output,waitWorkspace,frame,stage,result}){
 const api=base+'/api/runs/'+id,textarea=page.locator('#message-content');
 const json=async url=>{const r=await fetch(url);assert.equal(r.status,200);return r.json()};
 const change=async name=>{const r=await fetch(base+'/_p0/workspace/'+name,{method:'POST'});assert.equal(r.status,204,await r.text())};
 let release;
 const gate=new Promise(resolve=>release=resolve),workspace='**/api/runs/'+id+'?view=workspace*';
 await page.route(workspace,async route=>{await gate;await route.continue()});
 const nav=page.goto(base+'/#history/'+id);
 stage('reader loading, history and independent scrolling');
 try{
  await page.waitForSelector('.workspace-loading .workspace-dialogue-head [data-action=latest]:disabled');
  result.loading=await geometry(page);assert.ok(result.loading.layout&&result.loading.headHit&&!result.loading.input);
  await shot(page,output,'loading');
 }finally{release();await nav;await page.unroute(workspace)}
 await waitWorkspace(page,id);
 const before=await geometry(page);sameHeading(result.loading,before,'loading to ready');assert.ok(before.paging&&before.input&&before.bodyHit&&before.graphHit);
 await page.locator('.conversation').evaluate(el=>{el.scrollTop=el.scrollHeight/2});await frame(page);
 const scrolled=await geometry(page);assert.ok(scrolled.scroll>100&&scrolled.outer===before.outer);sameHeading(before,scrolled,'inner scroll');
 const root=page.locator('.exploration-preview'),preview=root.locator('.graph-shell');
 const originalHeight=await preview.evaluate(el=>el.style.height);
 const enlargement=await enlargeExplorationForOuterScroll({page,root,frame:()=>frame(page)});
 const deep=await geometry(page);
 assert.ok(deep.outer>560&&deep.layout&&deep.headHit&&deep.graphHit&&Math.abs(deep.headerTop-deep.tabsBottom)<=2,'deep wheel must dock the reader without hiding the graph');
 assert.ok(Math.abs(deep.paneHeight-before.paneHeight)<=1,'outer scroll must not shrink the transcript');
 for(const max of [enlargement.scrolled.max,deep.outerMax])assert.ok(Math.abs(max-enlargement.geometry.max)<=2,'wheel changed scroll range');
 await shot(page,output,'deep-scroll');
 await page.locator('#workspace').evaluate(el=>{el.scrollTop+=el.querySelector('.workspace-column').getBoundingClientRect().top-el.querySelector('.task-tabs').getBoundingClientRect().bottom+35});await frame(page);
 const docked=await geometry(page);assert.ok(docked.bodyHit&&Math.abs(docked.headerTop-docked.tabsBottom)<=2);
 await page.locator('#workspace').evaluate(el=>{el.scrollTop+=90});await frame(page);
 const outer=await geometry(page);assert.ok(outer.outer-docked.outer>60&&outer.bodyHit);sameHeading(docked,outer,'outer scroll');
 await page.locator('.workspace-dialogue-head [data-action=messages-page][data-before]').click();
 await page.waitForSelector('.workspace-dialogue-head [data-action=messages-latest]');
 await frame(page);const history=await geometry(page);sameHeading(outer,history,'older page');assert.ok(history.paging);
 await page.locator('.workspace-dialogue-head [data-action=messages-latest]').click();
 await page.waitForSelector('[data-message-id=p0-history-119]');
 await frame(page);const latest=await geometry(page);sameHeading(history,latest,'return to live');
 result.reader={before,scrolled,docked,outer,history,latest,enlargement,deep};

 stage('real HTTP acknowledgement');
 const content='Workspace browser acceptance fixture '+id;
 await textarea.fill(content);await frame(page);
 const sending=await geometry(page);assert.ok(sending.input&&sending.layout);
 const receipt=page.waitForResponse(r=>r.url()===api+'/steer'&&r.request().method()==='POST');
 await page.locator('#message-form button.primary').click();
 const response=await receipt;assert.equal(response.status(),200);
 const ack=await response.json();
 assert.equal(response.request().postDataJSON().content,content);
 assert.ok(ack.accepted&&ack.operation_id&&ack.message_id&&ack.status==='queued');
 assert.deepEqual(await json(api+'/operations/'+ack.operation_id),ack);
 await page.getByRole('status').filter({hasText:'已入队 · 等待运行边界实际投递'}).waitFor();
 await page.locator('#run-mailbox .queue-message').filter({hasText:content}).waitFor({state:'attached'});
 assert.equal(await textarea.inputValue(),'');await frame(page);
 const received=await geometry(page);
 for(const k of ['x','y','width','height'])assert.ok(Math.abs(sending.input[k]-received.input[k])<=2,'ACK moved the right-hand input: '+k);
 sameHeading(sending,received,'queued ACK');
 assert.ok(await textarea.evaluate(el=>{const r=el.getBoundingClientRect();return document.elementFromPoint(r.left+r.width/2,r.top+r.height/2)===el}));
 result.ack={http:response.status(),status:ack.status,operation:ack.operation_id,message:ack.message_id,sending,received};
 await shot(page,output,'ack');
 await preview.evaluate((el,h)=>{el.style.height=h},originalHeight);await frame(page);

 stage('uncertain transition and retained source');
 const pending=page.locator('.conversation [data-tool-key$=":fixture-pending"]');
 const normal=page.locator('.conversation [data-tool-key$=":fixture-normal"]');
 assert.equal(await pending.count(),1);assert.equal(await normal.count(),1);
 assert.equal(await page.locator('.conversation .tool-group [data-tool-key]').count(),2);
 await change('uncertain');await pending.waitFor({state:'detached'});
 assert.equal(await normal.count(),1);
 const raw=await json(api+'/actions/fixture-pending?request=p0-request-0');
 const events=await json(api+'/events/history');
 assert.equal(raw.status,'uncertain');assert.equal(raw.call_id,'fixture-pending');
 assert.ok(events.events.some(e=>e.type==='action_uncertain'&&e.data?.call_id===raw.call_id&&e.data?.request_id===raw.request_id));
 result.uncertain={hidden:true,normalVisible:true,action:raw.status,event:'action_uncertain'};
 await graphs({page,base,id,output,frame,stage,result,change});
}
