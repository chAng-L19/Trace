import assert from 'node:assert/strict';

// Test-only DOM relations: the production preview projects plans/child reports,
// which currently have no natural parallel relation. Do not call this a saved
// Store projection test. Both real renderers and explorationRoutes stay intact.
// ARTEX e6ec569: src + relation + dst identifies an edge; pi 1f6f319:
// tree-selector.test.ts checks exact identities and sibling exclusion;
// Muteki a73535a: EvidenceChain.tsx keeps display focus separate from facts.
export const parallelRelations=['依赖','专项夹具关联'];
export const parallelEndpoints={from:'task:todo-ready',to:'task:todo-dependent'};

export async function installExplorationParallelFixture(page,selector){
  return page.locator(selector).evaluate(async(root,{relations,endpoints})=>{
    const {explorationRoutes,mountExploration}=await import('/exploration.js');
    const edges=[...root.querySelectorAll('[data-action="exploration-chain"]')];
    const source=edges.find(e=>e.dataset.from===endpoints.from&&e.dataset.to===endpoints.to&&e.dataset.relation===relations[0]);
    if(!source)throw new Error('Parallel fixture dependency missing');
    const copy=source.cloneNode(true);
    copy.dataset.relation=relations[1];copy.dataset.explorationFixture='parallel';
    copy.querySelector('title').textContent=relations[1];
    source.after(copy);
    const preview=!!root.dataset.explorationPreview,svg=root.querySelector('.graph-shell svg');
    const positions=new Map([...root.querySelectorAll('[data-exploration-search],[data-chain-node]')].map(n=>{
      const p=preview?n.querySelector('g').transform.baseVal.consolidate().matrix:n.closest('foreignObject');
      return [n.dataset.id,preview?{x:p.e,y:p.f}:{x:p.x.baseVal.value,y:p.y.baseVal.value}];
    }));
    const pair=relations.map(relation=>({...endpoints,relation}));
    const {routes}=explorationRoutes({positions,width:svg.viewBox.baseVal.width,height:svg.viewBox.baseVal.height},pair,
      preview?{nodeWidth:212,port:28,columnGap:252}:{nodeWidth:246,port:65,columnGap:340});
    [source,copy].forEach((edge,i)=>{
      edge.querySelectorAll('path').forEach(path=>path.setAttribute('d',routes[i].d));
      const text=edge.querySelector('text');
      if(text){text.textContent=relations[i];text.setAttribute('x',routes[i].x);text.setAttribute('y',routes[i].y)}
    });
    mountExploration();
    return {kind:'test-only DOM parallel relations',endpoints,relations,paths:routes.map(route=>route.d),
      hitWidths:[source,copy].map(edge=>Number.parseFloat(getComputedStyle(edge.querySelector('.edge-hit')).strokeWidth)),
      positions:[...positions],sizes:preview?{nodeWidth:212,port:28,columnGap:252}:{nodeWidth:246,port:65,columnGap:340}};
  },{relations:parallelRelations,endpoints:parallelEndpoints});
}

// Reusable by workspace acceptance. Only a native drag grows the graph.
export async function enlargeExplorationForOuterScroll({page,root,frame,result={}}){
  const canvas=root.locator('.graph-shell');
  await canvas.evaluate(el=>el.scrollIntoView({block:'end',inline:'nearest'}));await frame();
  const before=result.before=await canvas.boundingBox(),x=before.x+before.width-5,y=before.y+before.height-5;
  await page.mouse.move(x,y);await page.mouse.down();
  await page.mouse.move(x,y+260,{steps:16});await page.mouse.up();await frame();
  const after=result.after=await canvas.boundingBox();
  assert.ok(after.height>=before.height+160,`native enlargement for outer scroll: ${before.height} -> ${after.height}`);
  const geometry=result.geometry=await page.locator('#workspace').evaluate(el=>({top:el.scrollTop,height:el.scrollHeight,client:el.clientHeight,max:el.scrollHeight-el.clientHeight}));
  assert.ok(geometry.max>560,'enlarged graph must supply actual outer scrolling beyond 560px');
  await page.locator('#workspace').evaluate(el=>{el.scrollTop=0});await frame();
  // Wheel on the outer owner, outside the inner graph and transcript.
  const point=await page.locator('#workspace').evaluate(el=>{const r=el.getBoundingClientRect();return {x:r.left+3,y:r.top+r.height/2}});
  result.wheel={...point,deltaY:620};
  await page.mouse.move(point.x,point.y);await page.mouse.wheel(0,620);
  await page.waitForFunction(()=>document.querySelector('#workspace').scrollTop>560);await frame();
  const scrolled=result.scrolled=await page.locator('#workspace').evaluate(el=>({top:el.scrollTop,max:el.scrollHeight-el.clientHeight}));
  assert.ok(scrolled.top>560,'wheel moves #workspace itself, not an inner viewport');
  return result;
}

export async function clickExplorationRelation({page,root,relation,frame,result={}}){
  const edge=root.locator(`[data-action="exploration-chain"][data-from="${parallelEndpoints.from}"][data-to="${parallelEndpoints.to}"][data-relation="${relation}"]`);
  const path=edge.locator('.edge-hit');
  await path.evaluate(el=>{
    const c=el.closest('.graph-shell'),r=c.getBoundingClientRect();
    const p=el.getPointAtLength(el.getTotalLength()/2).matrixTransform(el.getScreenCTM());
    c.scrollLeft+=p.x-r.left-c.clientWidth/2;c.scrollTop+=p.y-r.top-c.clientHeight/2;
  });
  await root.locator('.graph-shell').evaluate(el=>el.scrollIntoView({block:'center',inline:'nearest'}));await frame();
  const point=result.point=await path.evaluate(el=>{
    const p=el.getPointAtLength(el.getTotalLength()/2).matrixTransform(el.getScreenCTM());
    const hit=document.elementFromPoint(p.x,p.y)?.closest('[data-action="exploration-chain"]');
    const owner=el.closest('[data-action="exploration-chain"]'),root=el.closest('[data-exploration-run]');
    const hitDomains=[...root.querySelectorAll('.edge-hit')].filter(other=>{
      const edge=other.closest('[data-action="exploration-chain"]');
      return edge.dataset.from===owner.dataset.from&&edge.dataset.to===owner.dataset.to;
    }).map(other=>{
      const edge=other.closest('[data-action="exploration-chain"]');
      return {from:edge.dataset.from,to:edge.dataset.to,relation:edge.dataset.relation,
        contains:other.isPointInStroke(p.matrixTransform(other.getScreenCTM().inverse())),
        strokeWidth:Number.parseFloat(getComputedStyle(other).strokeWidth),d:other.getAttribute('d')};
    });
    return {x:p.x,y:p.y,relation:hit?.dataset.relation,from:hit?.dataset.from,to:hit?.dataset.to,
      totalLength:el.getTotalLength(),hitDomains};
  });
  assert.ok(point.hitDomains.every(domain=>domain.strokeWidth===16),'retain the full 16px pointer target');
  assert.deepEqual(point.hitDomains.filter(domain=>domain.contains).map(({from,to,relation})=>({from,to,relation})),
    [{...parallelEndpoints,relation}],'arc midpoint belongs exclusively to the requested hit stroke');
  assert.deepEqual({from:point.from,to:point.to,relation:point.relation},{...parallelEndpoints,relation},'real pointer must hit the intended parallel relation');
  await page.mouse.click(point.x,point.y);await frame();result.clicked=true;
  return {edge,point};
}

export async function explorationSelectionState(root){
  return root.evaluate(root=>({
    run:root.dataset.explorationRun,
    edges:[...root.querySelectorAll('[data-action="exploration-chain"]')].map(e=>({
      from:e.dataset.from,to:e.dataset.to,relation:e.dataset.relation,
      selected:e.classList.contains('chain-selected'),active:e.classList.contains('chain-active'),
      muted:e.classList.contains('chain-muted'),pressed:e.getAttribute('aria-pressed'),opacity:Number(getComputedStyle(e).opacity)
    })),
    nodes:[...root.querySelectorAll('[data-exploration-search],[data-chain-node]')].map(n=>({
      id:n.dataset.id,active:n.classList.contains('chain-active'),muted:n.classList.contains('chain-muted'),opacity:Number(getComputedStyle(n).opacity)
    }))
  }));
}
