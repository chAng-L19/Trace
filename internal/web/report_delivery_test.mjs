import assert from 'node:assert/strict';
import {test} from 'node:test';
import {createHash} from 'node:crypto';
import {createDelivery,reportDownloads} from './static/delivery.js';
import {renderExplorationDetail} from './static/exploration.js';
import {artifactsPanel} from './static/workspace.js';

const report={id:'final-v1',kind:'final',subject_id:'result',version:1,source_status:'current',translation_status:'complete',files:[{name:'report.zh-CN.md'},{name:'report.en.md'}]};
const snap=(reports=[report],id='run-A')=>({run:{id},reports,result:{summary:'Recorded result'}});
const digest=value=>createHash('sha256').update(value).digest('hex');
function button(html,language){
  const tag=html.match(/<button\b[^>]*>/g)?.find(t=>t.includes('data-language="'+language+'"'));
  if(!tag)return;
  const attr=name=>tag.match(new RegExp('data-'+name+'="([^"]*)"'))?.[1];
  return {dataset:{format:attr('format'),run:attr('run'),reportId:attr('report-id'),language:attr('language')}};
}
function fixture(t){
  const state={selected:'run-A',snap:snap()},urls=[],saved=[],blobs=[],notes=[],graphRoots=[];
  const bytes={'final-v1':'version one archived bytes','final-v2':'version two different bytes'};
  const fetchCall=t.mock.method(globalThis,'fetch',async url=>{
    urls.push(url);const id=new URL(url,'http://fixture').searchParams.get('report_id');
    return new Response(bytes[id]||'other export');
  });
  t.mock.method(URL,'createObjectURL',blob=>{blobs.push(blob);return 'blob:fixture'});
  t.mock.method(URL,'revokeObjectURL',()=>{});
  globalThis.document={body:{append(){}},createElement(){return {click(){saved.push(this.download)},remove(){}}},querySelectorAll(selector){assert.equal(selector,'.exploration-view');return graphRoots}};
  t.after(()=>{delete globalThis.document});
  const {dispatch}=createDelivery({state,token:()=>({nav:0}),notice:text=>notes.push(text)});
  return {state,urls,saved,blobs,notes,bytes,fetchCall,graphRoots,click:el=>dispatch('export',el)};
}
test('rendered report buttons fetch the selected Run, immutable version and language',async t=>{
  const f=fixture(t),other={...report,id:'stage-new',kind:'stage'};
  f.state.snap=snap([report,{...report,id:'final-v2',version:2},other]);
  for(const lang of ['zh-CN','en']){
    const html=lang==='en'?renderExplorationDetail(f.state.snap,'report'):reportDownloads(f.state.snap);
    await f.click(button(html,lang));
    assert.equal(f.urls.at(-1),'/api/runs/run-A/export?format=report&report_id=final-v2&language='+lang);
    assert.equal(digest(await f.blobs.at(-1).text()),digest(f.bytes['final-v2']));
  }
  assert.equal(f.saved.length,2);
  for(const format of ['json','evidence'])await f.click({dataset:{format}});
  assert.deepEqual(f.urls.slice(2),['/api/runs/run-A/export?format=json','/api/runs/run-A/export?format=evidence']);
});
test('report detail offers association expansion only for a mounted graph of the same Run',t=>{
  const f=fixture(t);
  for(const mountedRuns of [[],['run-B'],['run-B','run-A']]){
    f.graphRoots.splice(0,f.graphRoots.length,...mountedRuns.map(run=>({dataset:{explorationRun:run}})));
    const html=renderExplorationDetail(f.state.snap,'report');
    if(mountedRuns.includes('run-A'))assert.match(html,/data-command="expand" data-run="run-A" data-id="report"/);
    else assert.doesNotMatch(html,/data-command="expand"/);
    assert.deepEqual(button(html,'en').dataset,{format:'report',run:'run-A',reportId:'final-v1',language:'en'});
  }
  assert.deepEqual(f.urls,[]);assert.deepEqual(f.saved,[]);
});
test('a displayed v1 stays downloadable after v2 replaces the current projection',async t=>{
  const f=fixture(t),old=button(reportDownloads(f.state.snap),'en');
  f.state.snap=snap([{...report,id:'final-v2',version:2}]);
  await f.click(old);
  assert.equal(f.urls[0],'/api/runs/run-A/export?format=report&report_id=final-v1&language=en');
  assert.equal(digest(await f.blobs[0].text()),digest(f.bytes['final-v1']));
  assert.notEqual(digest(await f.blobs[0].text()),digest(f.bytes['final-v2']));
  assert.equal(f.saved.length,1);
});
test('missing archive or language renders no download; cross-Run and unbound clicks send nothing',async t=>{
  const f=fixture(t),old=button(reportDownloads(f.state.snap),'en');
  for(const [reports,status] of [[[],'报告未归档'],[[{...report,files:[]}],'报告未归档'],[[{...report,source_status:'changed'}],'仅有历史归档'],[[{...report,source_status:'not_checked'}],'归档状态待核对']]){
    const html=reportDownloads(snap(reports));
    assert.ok(html.includes(status));assert.equal(button(html,'en'),undefined);
  }
  f.state.snap=snap([report],'run-B');await f.click(old);
  f.state.selected='run-B';await f.click(old);
  await f.click({dataset:{format:'report'}});
  assert.deepEqual(f.urls,[]);assert.deepEqual(f.saved,[]);
  const graph=renderExplorationDetail(snap([{...report,kind:'stage'}]),'report');
  assert.match(graph,/最终报告未归档/);assert.doesNotMatch(graph,/data-format="report"/);
  for(const missing of ['en','zh-CN']){
    const partial=snap([{...report,files:report.files.filter(f=>!f.name.endsWith('.'+missing+'.md'))}]);
    assert.equal(button(reportDownloads(partial),missing),undefined);
    const card=artifactsPanel(partial);
    assert.match(card,/生成时：中英文均已生成/);
    assert.ok(card.includes((missing==='en'?'English':'中文')+' 文件当前不可下载'));
    assert.ok(!card.includes('&amp;language='+missing+'"'));
  }
});
test('Run changes cancel a pending download while a newer report leaves fixed bytes intact',async t=>{
  const f=fixture(t);
  for(const crossRun of [true,false]){
    f.state.selected='run-A';f.state.snap=snap();
    const started=Promise.withResolvers(),body=Promise.withResolvers();
    f.fetchCall.mock.mockImplementation(async url=>{f.urls.push(url);return {ok:true,headers:new Headers(),blob:()=>{started.resolve();return body.promise}}});
    const sending=f.click(button(reportDownloads(f.state.snap),'en'));
    await started.promise;
    if(crossRun){f.state.selected='run-B';f.state.snap=snap([report],'run-B')}
    else f.state.snap=snap([{...report,id:'final-v2',version:2}]);
    body.resolve(new Blob([f.bytes['final-v1']]));await sending;
  }
  assert.deepEqual(f.urls,Array(2).fill('/api/runs/run-A/export?format=report&report_id=final-v1&language=en'));
  assert.equal(f.saved.length,1);
  assert.equal(digest(await f.blobs[0].text()),digest(f.bytes['final-v1']));
});
