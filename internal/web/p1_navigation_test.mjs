import assert from 'node:assert/strict';
import fs from 'node:fs';
import {resolveRoute,renderPage} from './static/views.js';
import {occupiedWorkerCount} from './static/ui.js';
for(const tab of ['planner','assets','events']){
  assert.deepEqual(resolveRoute('#'+tab,'current'),{page:'history',historyDetail:true,runID:'current',deepLink:{tab}});
  assert.deepEqual(resolveRoute('#'+tab),{page:'history',historyDetail:false});
}
const html=fs.readFileSync(new URL('./static/index.html',import.meta.url),'utf8');
assert.doesNotMatch(html,/href="#(?:planner|assets|events)"/);
const snap={run:{id:'one',goal:'fixture goal',status:'paused',budget:{}},workers:['starting','running','cancelling','completed','paused','failed'].map((status,i)=>({id:String(i),status,task_id:String(i)})),messages:[],plan:{tasks:[],capacity:5}};
assert.equal(occupiedWorkerCount(snap),6,'unknown cleanup still occupies capacity');
const page=renderPage({page:'history',historyDetail:true,taskTab:'activity',selected:'one',snap,runs:[],tools:[],skills:[]});
assert.match(page,/6 占槽/);assert.match(page,/本计划占槽 \/ 容量/);assert.match(page,/data-worker-tree-capacity/,'authoritative tree status has an independent asynchronous slot');
console.log('P1 navigation: old scoped hashes, unique sidebar entries and consistent occupied Worker count passed.');
