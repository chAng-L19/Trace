import assert from 'node:assert/strict';
import {explorationLayout,explorationRoutes,explorationChain,explorationGraph,selectExplorationChain,explorationControl,mountExploration} from './static/exploration.js';
import {explorationPreview} from './static/graphs.js';
import {mountGraphPan} from './static/history-projection.js';

// Execute on the designated Ubuntu host. ARTEX e6ec569 barycenter adaptation:
// preserve graph identities/edges while reducing a known crossing, not an
// assertion that every arbitrary cyclic graph is crossing-free.
const nodes=['goal','a','b','x','y'].map(id=>({id}));
const edges=[{from:'goal',to:'a'},{from:'goal',to:'b'},{from:'a',to:'y'},{from:'b',to:'x'}];
const original=JSON.stringify({nodes,edges}),layout=explorationLayout(nodes,edges),pos=layout.positions;
assert.ok((pos.get('a').y-pos.get('b').y)*(pos.get('y').y-pos.get('x').y)>0,'crossed input order becomes non-crossing parallel branches');
assert.deepEqual(explorationLayout(nodes,edges),layout,'stable layout for the same snapshot');
assert.equal(JSON.stringify({nodes,edges}),original,'layout never rewrites ledger edges');
const cyclic=explorationLayout(nodes,[...edges,{from:'y',to:'a'}]);
assert.equal(cyclic.positions.size,nodes.length);
assert.equal(new Set([...cyclic.positions.values()].map(p=>`${p.x},${p.y}`)).size,nodes.length,'cycle break retains distinct nodes');
for(const p of cyclic.positions.values())assert.ok(Number.isFinite(p.x)&&Number.isFinite(p.y));
assert.equal(explorationLayout([],[]).positions.size,0);
const detours=[...edges,{from:'goal',to:'y',relation:'shortcut'},{from:'y',to:'a',relation:'back'}];
const routed=explorationRoutes(layout,detours,{nodeWidth:246,port:65,columnGap:340});
assert.equal(routed.routes.length,detours.length);
assert.ok(routed.routes.slice(0,4).every(route=>!route.detour));
for(const route of routed.routes.slice(4)){
  assert.ok(route.detour&&route.y<Math.min(...[...routed.positions.values()].map(p=>p.y)));
  assert.match(route.d,/ H[0-9.]+ V[0-9.]+ H[0-9.]+ V[0-9.]+ H[0-9.]+$/);
}
for(const [input,nodeWidth,port,columnGap] of [[layout,246,65,340],[{positions:new Map([...pos].map(([id,p])=>[id,{x:p.x*252/340,y:p.y*92/164}])),width:layout.width*252/340+12,height:layout.height*92/164+12},212,28,252]]){
  const boundary=explorationRoutes(input,[{from:'y',to:'goal'}],{nodeWidth,port,columnGap});
  const [,,out,,into]=boundary.routes[0].d.match(/-?\d+(?:\.\d+)?/g).map(Number);
  assert.ok(out>boundary.positions.get('y').x+nodeWidth&&out<boundary.width);
  assert.ok(into>0&&into<boundary.positions.get('goal').x);
}


const parallel=[{from:'a',to:'y',relation:'first'},{from:'a',to:'y',relation:'second'}];
assert.notEqual(...explorationRoutes(layout,parallel,{nodeWidth:246,port:65,columnGap:340}).routes.map(route=>route.d));
assert.deepEqual([...explorationChain(nodes,parallel,parallel[1]).linked],[1]);

const branch=explorationChain(nodes,edges,{from:'a',to:'y'});
assert.deepEqual([...branch.active].sort(),['a','goal','y']);
assert.deepEqual([...branch.linked].sort(),[0,2],'shared goal does not pull in sibling branch');
assert.deepEqual([...explorationChain(nodes,edges,{from:'a',to:'a'}).active].sort(),['a','goal','y']);
assert.equal(explorationChain(nodes,edges,null).active.size,0);
assert.equal(explorationChain(nodes,edges,{from:'removed',to:'y'}).active.size,0);
assert.equal(explorationChain(nodes,[...edges,{from:'y',to:'a'}],{from:'a',to:'a'}).active.size,3);

const classList=()=>({values:new Set(),toggle(k,on){if(on)this.values.add(k);else this.values.delete(k)}});
const nodeElements=nodes.map(n=>({dataset:{id:n.id,chainTitle:n.id},classList:classList()}));
const edgeElements=edges.map(e=>({dataset:e,classList:classList(),setAttribute(k,v){this[k]=v}}));
const selection={innerHTML:''};
const root={dataset:{explorationRun:'chain-fixture'},querySelectorAll:selector=>selector==='[data-action="exploration-chain"]'?edgeElements:nodeElements,querySelector:()=>selection};
selectExplorationChain({closest:()=>root,dataset:{action:'exploration-chain',from:'a',to:'y'}});
assert.equal(edgeElements[2]['aria-pressed'],'true');
assert.equal(nodeElements[2].classList.values.has('chain-muted'),true);
assert.match(selection.innerHTML,/data-action="exploration-detail" data-id="a"/);
assert.match(selection.innerHTML,/data-action="exploration-detail" data-id="y"/,'edge highlighting preserves source-detail navigation');
edgeElements[2].dataset={...edgeElements[2].dataset,relation:'replacement'};
selectExplorationChain({closest:()=>root,dataset:{action:'exploration-chain',from:'a',to:'y',relation:'removed'}});
assert.doesNotMatch(selection.innerHTML,/已高亮/,'a replaced or deleted edge cannot remain selected');
selectExplorationChain({closest:()=>root,dataset:{id:'a'}});
assert.match(selection.innerHTML,/已高亮/,'node selection does not require a self-edge');
explorationControl({closest:()=>root,dataset:{command:'clear'}});
assert.ok(nodeElements.every(n=>!n.classList.values.has('chain-muted')));
assert.ok(edgeElements.every(e=>e['aria-pressed']==='false'));
assert.match(selection.innerHTML,/data-command="clear" disabled/);

const snap={run:{id:'preview-fixture',goal:'Goal',status:'paused'},plan:{tasks:[{id:'one',goal:'First',status:'completed'},{id:'two',goal:'Second',dependencies:['one'],status:'pending'}]}};
for(const html of [explorationPreview(snap),explorationGraph({snap})]){
  assert.match(html,/data-action="exploration-chain"/);
  assert.match(html,/data-command="clear"/);
  assert.match(html,/data-action="exploration-detail"/);
}

const handlers={},canvas={dataset:{},style:{width:'640px',height:'520px'},clientWidth:640,clientHeight:520,scrollLeft:100,scrollTop:100,getBoundingClientRect:()=>({left:0,top:0}),setPointerCapture(){},classList:{add(){},remove(){}},addEventListener:(name,fn)=>handlers[name]=fn};
mountGraphPan(canvas);
const pointer=(x,y,target=null)=>({button:0,pointerId:1,clientX:x,clientY:y,target:{closest:()=>target},preventDefault(){this.prevented=true}});
const resize=pointer(635,515);handlers.pointerdown(resize);handlers.pointermove(pointer(580,470));
assert.equal(resize.prevented,undefined,'native resize grip is not captured as pan');
assert.equal(canvas.scrollLeft,100);
const edge=pointer(100,100,{});handlers.pointerdown(edge);assert.equal(edge.prevented,undefined,'interactive edges remain clickable');
const pan=pointer(100,100);handlers.pointerdown(pan);handlers.pointermove(pointer(80,70));
assert.equal(pan.prevented,true);assert.equal(canvas.scrollLeft,120);assert.equal(canvas.scrollTop,130);handlers.pointerup();

// Native dimensions survive a subsequent renderer call for both graph modes.
let observed=[];globalThis.ResizeObserver=class{constructor(callback){this.callback=callback}observe(el){observed.push(el)}disconnect(){}};
const svg={dataset:{naturalWidth:'1000',naturalHeight:'700'},style:{},setAttribute(){}};
canvas.querySelector=()=>svg;
const sizedRoot={dataset:{explorationRun:'preview-fixture',explorationPreview:'true'},querySelectorAll:()=>[],querySelector:selector=>selector==='.graph-shell'?canvas:null};
globalThis.document={querySelectorAll:()=>[sizedRoot]};
mountExploration();assert.equal(observed[0],canvas);
assert.match(explorationPreview(snap),/style="width:640px;height:520px"/);
sizedRoot.dataset={explorationRun:'full-size-fixture'};
const zoom={};sizedRoot.querySelector=selector=>selector==='.graph-shell'||selector==='.exploration-canvas'?canvas:selector==='.exploration-zoom'?zoom:null;
mountExploration();
assert.match(explorationGraph({snap:{...snap,run:{...snap.run,id:'full-size-fixture'}}}),/style="width:640px;height:520px"/);
delete globalThis.document;delete globalThis.ResizeObserver;
console.log('Exploration interactions: stable crossing reduction, cycles, branch selection, source links, clear, resize/pan and retained dimensions passed.');
