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
function points(route){
  assert.match(route.d,/^M[\d.]+ [\d.]+(?: [HV][\d.]+)+$/);
  const path=[];let x,y;
  for(const [,command,a,b] of route.d.matchAll(/([MHV])([\d.]+)(?: ([\d.]+))?/g)){
    if(command==='M'){x=Number(a);y=Number(b)}else if(command==='H')x=Number(a);else y=Number(a);
    path.push([x,y]);
  }
  return path;
}
function avoidsNodes(graph,route,w,h){
  const path=points(route);
  path.forEach(([x,y],i)=>{
    assert.ok(x>=0&&x<=graph.width&&y>=0&&y<=graph.height,'track stays inside SVG');
    if(!i)return;
    const [a,b]=path[i-1];
    for(const [id,p] of graph.positions){
      const inside=Math.max(a,x)>p.x&&Math.min(a,x)<p.x+w&&Math.max(b,y)>p.y&&Math.min(b,y)<p.y+h;
      assert.ok(!inside,`segment ${a},${b} to ${x},${y} crosses ${id}`);
    }
  });
}
for(const route of routed.routes.slice(4))avoidsNodes(routed,route,246,132);
for(const [input,nodeWidth,port,columnGap] of [[layout,246,65,340],[{positions:new Map([...pos].map(([id,p])=>[id,{x:p.x*252/340,y:p.y*92/164}])),width:layout.width*252/340+12,height:layout.height*92/164+12},212,28,252]]){
  const boundary=explorationRoutes(input,[{from:'y',to:'goal'}],{nodeWidth,port,columnGap});
  const [,,out,,into]=boundary.routes[0].d.match(/-?\d+(?:\.\d+)?/g).map(Number);
  assert.ok(out>boundary.positions.get('y').x+nodeWidth&&out<boundary.width);
  assert.ok(into>0&&into<boundary.positions.get('goal').x);
}

// Two branches reconverge. Each also has its own shortcut spanning an
// occupied column. A global top lane crosses the other branch unnecessarily.
const joinNodes=['root','a1','b1','b2','a2','a3','b3','join'].map(id=>({id}));
const joinEdges=[['root','a1'],['root','b1'],['a1','a2'],['b1','b2'],['a2','a3'],['b2','b3'],['a3','join'],['b3','join'],['a1','a3'],['b1','b3']].map(([from,to])=>({from,to}));
const joined=explorationLayout(joinNodes,joinEdges);
for(const [sx,sy,w,port,gap] of [[1,1,246,65,340],[252/340,92/164,212,28,252]]){
  const sizes={nodeWidth:w,port,columnGap:gap},p=new Map([...joined.positions].map(([id,v])=>[id,{x:v.x*sx,y:v.y*sy}]));
  const g=explorationRoutes({positions:p,width:joined.width*sx,height:joined.height*sy},joinEdges,sizes);
  const ranges=g.routes.slice(-2).map(r=>{avoidsNodes(g,r,w,port*2+2);const ys=points(r).map(v=>v[1]);return [Math.min(...ys),Math.max(...ys)]});
  assert.ok(ranges[0][1]<ranges[1][0]||ranges[1][1]<ranges[0][0],'shortcuts stay in disjoint branch corridors');
  // A completely clear middle row should use the straight corridor.
  const spare=new Map([['x',{x:24,y:30}],['other',{x:24+gap,y:200}],['y',{x:24+gap*2,y:30}]]);
  const open=explorationRoutes({positions:spare,width:gap*3,height:400},[{from:'x',to:'y'}],sizes);
  assert.equal(new Set(points(open.routes[0]).map(point=>point[1])).size,1);
}

const parallel=[{from:'a',to:'y',relation:'first'},{from:'a',to:'y',relation:'second'}];
// R2 preview failure: 40px gutter, 184px rise. Merely translating a cubic
// by 20px leaves its midpoint inside the other relation's 16px hit stroke.
// Dagre/Cairn's independent 20px edge spacing is adapted to Trace's SVG routes.
function arcMidpoint(path){
  const lengths=path.slice(1).map(([x,y],i)=>Math.hypot(x-path[i][0],y-path[i][1]));
  let left=lengths.reduce((a,b)=>a+b,0)/2;
  for(const [i,length] of lengths.entries()){
    if(left<=length){const t=length?left/length:0;return path[i].map((v,j)=>v+(path[i+1][j]-v)*t)}
    left-=length;
  }
}
function segmentGap(a,b,c,d){
  return Math.hypot(...[0,1].map(i=>Math.max(0,Math.min(a[i],b[i])-Math.max(c[i],d[i]),Math.min(c[i],d[i])-Math.max(a[i],b[i]))));
}
for(const sizes of [{nodeWidth:212,port:28,columnGap:252},{nodeWidth:246,port:65,columnGap:340}]){
  const r2={positions:new Map([['a',{x:269.7882385253906,y:13.463414192199707}],['y',{x:269.7882385253906+sizes.columnGap-.0000305175781,y:197.46340942382812}]]),width:1000,height:600};
  for(const rise of [184-.0000047683716,-184,368,-368,0]){
    r2.positions.get('a').y=rise<0?400:13.463414192199707;
    r2.positions.get('y').y=r2.positions.get('a').y+rise;
    for(const pair of [parallel,[...parallel].reverse()]){
      const before=JSON.stringify([...r2.positions]),r=explorationRoutes(r2,pair,sizes),paths=r.routes.map(points);
      assert.equal(JSON.stringify([...r2.positions]),before,'routing leaves node positions untouched');
      assert.deepEqual(explorationRoutes(r2,pair,sizes),r,'parallel routing is deterministic');
      r.routes.forEach(route=>avoidsNodes(r,route,sizes.nodeWidth,sizes.port*2+2));
      for(const [i,path] of paths.entries()){
        const middle=arcMidpoint(path),other=paths[1-i];
        const distance=Math.min(...other.slice(1).map((end,j)=>segmentGap(middle,middle,other[j],end)));
        assert.ok(distance>8,'each arc midpoint is outside the other 16px hit stroke');
        const gap=Math.min(...path.slice(1).flatMap((end,j)=>other.slice(1).map((otherEnd,k)=>segmentGap(path[j],end,other[k],otherEnd))));
        assert.ok(gap>16,'parallel hit strokes are disjoint, including port approaches');
      }
    }
  }
  const single=explorationRoutes(r2,[parallel[0]],sizes);
  assert.match(single.routes[0].d,/ C/,'ordinary adjacent single edges keep their existing curve');
}
assert.deepEqual([...explorationChain(nodes,parallel,parallel[1]).linked],[1]);
const loop=[...parallel,{from:'y',to:'a',relation:'return'},{from:'goal',to:'a'},{from:'y',to:'x'}];
assert.deepEqual([...explorationChain(nodes,loop,parallel[1]).linked].sort(),[1,3,4],'a cycle does not reselect a parallel relation or the return edge');
assert.equal(explorationChain(nodes,loop,{...parallel[0],relation:'retired'}).active.size,0,'same endpoints do not resurrect a missing relation');

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
const selection={innerHTML:'',firstElementChild:{prepend(text){this.prefix=text}}};
const root={dataset:{explorationRun:'chain-fixture'},querySelectorAll:selector=>selector==='[data-action="exploration-chain"]'?edgeElements:nodeElements,querySelector:()=>selection};
selectExplorationChain({closest:()=>root,dataset:{action:'exploration-chain',from:'a',to:'y'}});
assert.equal(edgeElements[2]['aria-pressed'],'true');
assert.equal(nodeElements[2].classList.values.has('chain-muted'),true);
assert.match(selection.innerHTML,/data-action="exploration-detail" data-id="a"/);
assert.match(selection.innerHTML,/data-action="exploration-detail" data-id="y"/,'edge highlighting preserves source-detail navigation');
edgeElements[2].dataset={...edgeElements[2].dataset,relation:'replacement'};
selectExplorationChain({closest:()=>root,dataset:{action:'exploration-chain',...edgeElements[2].dataset}});
assert.equal(edgeElements[2]['aria-label'],'a → y：replacement');
assert.match(selection.firstElementChild.prefix,/replacement/,'visible selection labels the specific relationship');
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
const replacement={...canvas,style:{}};
sizedRoot.querySelector=selector=>selector==='.graph-shell'?replacement:null;
mountExploration();assert.deepEqual(replacement.style,{width:'640px',height:'520px'},'restore dimensions without an inline style attribute');
assert.doesNotMatch(explorationGraph({snap}),/width:640px;height:520px/,'preview and full stay independent for the same Run');
assert.doesNotMatch(explorationPreview({...snap,run:{...snap.run,id:'another-run'}}),/width:640px;height:520px/,'identical node IDs do not transfer dimensions between Runs');
sizedRoot.dataset={explorationRun:'full-size-fixture'};
const zoom={};sizedRoot.querySelector=selector=>selector==='.graph-shell'||selector==='.exploration-canvas'?canvas:selector==='.exploration-zoom'?zoom:null;
mountExploration();
assert.match(explorationGraph({snap:{...snap,run:{...snap.run,id:'full-size-fixture'}}}),/style="width:640px;height:520px"/);
delete globalThis.document;delete globalThis.ResizeObserver;
console.log('Exploration interactions: stable crossing reduction, cycles, branch selection, source links, clear, resize/pan and retained dimensions passed.');
