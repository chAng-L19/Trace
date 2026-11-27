// Only via TestExplorationGraphInteractions on the designated server.
import assert from 'node:assert/strict';
import {existsSync,readFileSync,writeFileSync} from 'node:fs';
import {dirname,join} from 'node:path';
import {createRequire} from 'node:module';
import {pathToFileURL} from 'node:url';
import {parallelRelations,parallelEndpoints,installExplorationParallelFixture,enlargeExplorationForOuterScroll,clickExplorationRelation,explorationSelectionState} from './exploration_browser_fixture.mjs';

const {base,ids:[id],output}=JSON.parse(process.env.TRACE_P0_FIXTURE);
assert.ok(process.env.DISPLAY&&existsSync(process.env.CLOAKBROWSER_BINARY_PATH));
const entry=createRequire(import.meta.url).resolve(process.env.TRACE_CLOAKBROWSER_NODE_MODULE);
let dir=dirname(entry),sdk;
while(dir!==dirname(dir)){
  if(existsSync(join(dir,'package.json'))){const pkg=JSON.parse(readFileSync(join(dir,'package.json')));if(pkg.name==='cloakbrowser'){sdk=pkg.version;break}}
  dir=dirname(dir);
}
assert.equal(sdk,'0.6.4');
const {launch}=await import(pathToFileURL(entry));
const browser=await launch({headless:false,args:['--no-sandbox','--window-size=1440,1000']});
const writes=[],measurements={},report={sdk,run:id,measurements,writes,
  scope:'real application DOM and pointer input; parallel relation is a test-only DOM fixture, not persisted graph data',
  refs:{pi:'1f6f319bb28992a40b99c147fffb23b65a23129d',ARTEX:'e6ec56999175509551a1e43a3ee3b053d0941bd9',Muteki:'a73535a19fd2c1cddd97dfbeea65f62f4ce136ee',Cairn:'8e7e0ea67552383851dfcabfba0c4e9c8d007878'}};
try{
  const page=await browser.newPage({viewport:{width:1440,height:1000}});
  page.on('request',req=>{if(req.url().startsWith(base+'/api/runs/')&&!['GET','HEAD'].includes(req.method()))writes.push([req.method(),req.url()])});
  await page.goto(`${base}/#history/${id}`);
  const frame=()=>page.evaluate(()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve))));
  for(const [mode,selector] of [['preview','.exploration-preview'],['full','.exploration-view']]){
    if(mode==='full')await page.locator('#task-tab-assets').click();
    const root=page.locator(selector),canvas=root.locator('.graph-shell');
    const edge=root.locator('[data-action=exploration-chain][data-from="task:todo-ready"][data-to="task:todo-dependent"][data-relation="依赖"]');
    await edge.waitFor({state:'attached'});
    if(mode==='preview'){
      measurements.previewRefresh=await root.evaluate(async root=>{
        const {patchLive}=await import('/interactions.js'),{mountExploration}=await import('/exploration.js');
        const nodes=[...root.querySelectorAll('[data-chain-node]')],edges=[...root.querySelectorAll('[data-action=exploration-chain]')];
        const retained=[];
        for(const target of [nodes[0],edges[0]]){
          target.focus();
          patchLive(root,root.innerHTML);mountExploration();
          const nextNodes=[...root.querySelectorAll('[data-chain-node]')],nextEdges=[...root.querySelectorAll('[data-action=exploration-chain]')];
          retained.push({focused:document.activeElement===target,nodes:nextNodes.length===nodes.length&&nodes.every((node,i)=>node===nextNodes[i]),edges:nextEdges.length===edges.length&&edges.every((edge,i)=>edge===nextEdges[i])});
        }
        return {nodeCount:nodes.length,edgeCount:edges.length,retained};
      });
      assert.ok(measurements.previewRefresh.nodeCount>0&&measurements.previewRefresh.edgeCount>0);
      assert.deepEqual(measurements.previewRefresh.retained,Array.from({length:2},()=>({focused:true,nodes:true,edges:true})),'same-topology preview reconciliation retains actual nodes, edges and keyboard focus');
    }
    await canvas.evaluate(el=>el.scrollIntoView({block:'center',inline:'nearest'}));await frame();
    const before=await canvas.boundingBox(),x=before.x+before.width-5,y=before.y+before.height-5;
    await page.mouse.move(x,y);await page.mouse.down();await page.mouse.move(x+80,y+100,{steps:12});await page.mouse.up();await frame();
    const after=await canvas.boundingBox();
    assert.ok(after.width>=before.width+60&&after.height>=before.height+70,`${mode} diagonal resize: ${JSON.stringify({before,after})}`);
    assert.equal(await root.getAttribute('data-exploration-run'),id);
    // Use a real pointer hit at the rendered path, not locator.click/DOM click.
    const path=edge.locator('.edge-hit');
    await path.evaluate(el=>{const c=el.closest('.graph-shell'),r=c.getBoundingClientRect(),p=el.getPointAtLength(el.getTotalLength()/2).matrixTransform(el.getScreenCTM());c.scrollLeft+=p.x-r.left-c.clientWidth/2;c.scrollTop+=p.y-r.top-c.clientHeight/2});
    await frame();
    const point=await path.evaluate(el=>{const p=el.getPointAtLength(el.getTotalLength()/2).matrixTransform(el.getScreenCTM());return {x:p.x,y:p.y,hit:document.elementFromPoint(p.x,p.y)?.closest('.exploration-edge')===el.parentElement}});
    assert.ok(point.hit,`${mode}: hittable path`);await page.mouse.click(point.x,point.y);
    assert.equal(await edge.getAttribute('aria-pressed'),'true');
    assert.equal(await root.locator('.exploration-edge.chain-selected').count(),1);
    assert.ok(await root.locator('.exploration-edge.chain-muted').count()>0);
    await page.screenshot({path:join(output,`exploration-${mode}-selected.png`)});
    // A drag starting on whitespace pans; the same point clicked clears.
    const blankPoint=()=>canvas.evaluate(c=>{
      const r=c.getBoundingClientRect();
      for(let y=Math.max(1,r.top)+10;y<Math.min(innerHeight,r.bottom)-36;y+=12)for(let x=Math.max(1,r.left)+10;x<Math.min(innerWidth,r.right)-36;x+=12){
        const el=document.elementFromPoint(x,y);if(el&&(el===c||el===c.querySelector('svg')))return {x,y};
      }
    });
    const blank=await blankPoint();assert.ok(blank,`${mode}: visible canvas background`);
    await page.mouse.move(blank.x,blank.y);await page.mouse.down();await page.mouse.move(blank.x+16,blank.y+16,{steps:4});await page.mouse.up();
    assert.equal(await edge.getAttribute('aria-pressed'),'true','pan must not clear');
    const clearPoint=await blankPoint();assert.ok(clearPoint);
    await page.mouse.click(clearPoint.x,clearPoint.y);assert.equal(await root.locator('.chain-muted').count(),0,'background click clears');
    await edge.focus();await page.keyboard.press('Enter');assert.equal(await edge.getAttribute('aria-pressed'),'true');
    await page.keyboard.press('Escape');assert.equal(await edge.getAttribute('aria-pressed'),'false');
    assert.ok(await edge.evaluate(el=>el===document.activeElement),'Escape preserves keyboard focus');
    await page.keyboard.press('Space');assert.equal(await edge.getAttribute('aria-pressed'),'true');
    await root.locator('[data-command=clear]').click();assert.equal(await root.locator('.chain-muted').count(),0);
    await edge.focus();await page.keyboard.press('Enter');assert.equal(await edge.getAttribute('aria-pressed'),'true');
    const other=mode==='preview'?'assets':'activity',back=mode==='preview'?'activity':'assets';
    await page.locator('#task-tab-'+other).click();await root.waitFor({state:'detached'});
    await page.locator('#task-tab-'+back).click();await edge.waitFor({state:'attached'});await frame();
    const restored=await canvas.boundingBox();
    assert.ok(Math.abs(restored.width-after.width)<=2&&Math.abs(restored.height-after.height)<=2,'remount preserves both dimensions');
    assert.equal(await edge.getAttribute('aria-pressed'),'true');
    const measurement=measurements[mode]={before,after,restored,parallel:[]};
    measurement.fixture=await installExplorationParallelFixture(page,selector);
    assert.notEqual(...measurement.fixture.paths,'parallel relations have separate real hit paths');
    await root.locator('[data-command=clear]').click();
    assert.equal(await root.locator('.chain-muted,.chain-selected,.chain-active').count(),0,'first mouse selection starts from a cleared state');
    const identity=e=>JSON.stringify([e.from,e.to,e.relation]);
    for(const [index,relation] of parallelRelations.entries()){
      // Persist the attempted midpoint and actual stroke ownership even on failure.
      const attempt={requested:{...parallelEndpoints,relation}};measurement.parallel.push(attempt);
      await clickExplorationRelation({page,root,relation,frame,result:attempt});
      const selection=attempt.selection=await explorationSelectionState(root);
      await page.screenshot({path:join(output,`exploration-${mode}-relation-${index+1}.png`)});
      const expected={...parallelEndpoints,relation};
      assert.equal(selection.run,id);
      assert.deepEqual(selection.edges.filter(e=>e.selected).map(identity),[identity(expected)],'exactly the requested relation is selected');
      assert.deepEqual(selection.edges.filter(e=>e.pressed==='true').map(identity),[identity(expected)],'aria selection agrees with exact relation');
      assert.deepEqual(selection.edges.filter(e=>e.active).map(identity).sort(),[identity({from:'goal',to:parallelEndpoints.from,relation:'计划方向'}),identity(expected)].sort(),'only the chosen relation and its upstream path are active');
      assert.deepEqual(selection.nodes.filter(n=>n.active).map(n=>n.id).sort(),['goal',parallelEndpoints.from,parallelEndpoints.to].sort(),'siblings are not added to the selected branch');
      for(const item of [...selection.edges,...selection.nodes]){
        assert.equal(item.muted,!item.active,'every unrelated item is muted');
        assert.ok(item.active?item.opacity===1:item.opacity<1,'computed appearance agrees with highlight state');
      }
      const sibling=selection.edges.find(e=>e.from===expected.from&&e.to===expected.to&&e.relation!==relation);
      assert.ok(sibling&&!sibling.active&&sibling.muted&&!sibling.selected&&sibling.pressed==='false','other relation on the same endpoints stays muted');
    }
    const second=root.locator('[data-exploration-fixture=parallel]');
    await second.focus();await page.keyboard.press('Escape');
    assert.equal(await root.locator('.chain-muted,.chain-selected,.chain-active').count(),0,'Escape clears both relation and ancestor highlight');
    await page.keyboard.press('Space');assert.equal(await second.getAttribute('aria-pressed'),'true');
    assert.equal(await root.locator('.exploration-edge.chain-selected').count(),1);
    await root.locator('[data-command=clear]').click();
    assert.equal(await root.locator('.chain-muted,.chain-selected,.chain-active').count(),0,'mouse clear leaves no parallel selection');
    measurement.parallelClear={escape:true,space:true,mouse:true};
    // Remove only the test relation before returning to live application data.
    await second.evaluate(el=>el.remove());
    await page.evaluate(async()=>{const {mountExploration}=await import('/exploration.js');mountExploration()});
    measurement.outer={};
    await enlargeExplorationForOuterScroll({page,root,frame,result:measurement.outer});
    await page.screenshot({path:join(output,`exploration-${mode}-outer-scroll.png`)});
  }
  const full=page.locator('.exploration-view'),sources=full.locator('.exploration-node.source');
  await full.locator('[data-command=mainline]').click();
  assert.equal(await sources.count(),0,'sources fold behind the default task line');
  const zoom=await full.locator('.exploration-zoom').textContent();
  await full.locator('[data-command=kind][data-kind=""]').click();
  assert.ok(await sources.count()>0,'the full view retains fixed source nodes');
  await full.locator('[data-command=mainline]').click();
  const search=full.locator('#exploration-search');
  await search.fill('child-source:');
  assert.ok(await sources.count()>0,'global search discloses folded source nodes');
  assert.ok(await full.locator('.exploration-node.source.search-match').count()>0);
  assert.equal(await search.evaluate(el=>el===document.activeElement),true,'search keeps keyboard focus when it unfolds the graph');
  await search.fill('');
  assert.equal(await sources.count(),0,'clearing search restores the task line');
  assert.equal(await full.locator('.exploration-zoom').textContent(),zoom,'projection switches preserve manual zoom');
  await full.locator('.exploration-node.child_report').first().click();
  await page.locator('#dialog-content [data-command=expand]').click();
  await page.locator('#close-dialog').click();
  assert.ok(await sources.count()>0,'receipt disclosure can expand its source in the graph');
  await full.locator('[data-command=collapse]').click();
  assert.equal(await sources.count(),0,'collapse restores folded details');
  measurements.projection={defaultTaskLine:true,fullView:true,globalSearch:true,focusRetained:true,zoomRetained:true,receiptDisclosure:true};
  assert.deepEqual(writes,[],'graph interaction must not issue a Run mutation');
}catch(error){report.error=String(error.stack||error);throw error}
finally{
  try{writeFileSync(join(output,'exploration-geometry.json'),JSON.stringify(report,null,2))}
  finally{await browser.close()}
}
