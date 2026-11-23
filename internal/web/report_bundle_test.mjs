import assert from 'node:assert/strict';
import {test} from 'node:test';
import {createDelivery,reportDownloads} from './static/delivery.js';

const report={id:'version-1',kind:'final',version:1,source_status:'current',files:[{name:'summary.zh-CN.md'},{name:'summary.en.md'}]};
const snapshot=(reports=[report],id='run-A')=>({run:{id},reports});
function button(html,language,format='report_bundle'){
  const tag=html.match(/<button\b[^>]*>/g)?.find(t=>t.includes('data-format="'+format+'"')&&t.includes('data-language="'+language+'"'));
  if(!tag)return;
  const attr=name=>tag.match(new RegExp('data-'+name+'="([^"]*)"'))?.[1];
  return {dataset:{format:attr('format'),run:attr('run'),reportId:attr('report-id'),language:attr('language')}};
}
function fixture(t){
  const state={selected:'run-A',snap:snapshot()},urls=[],saved=[],blobs=[],notes=[];
  let nav=0;
  const fetchCall=t.mock.method(globalThis,'fetch',async url=>{
    urls.push(url);
    const params=new URL(url,'http://fixture').searchParams;
    return new Response('fixed '+params.get('report_id')+' '+params.get('language'),{headers:{'Content-Type':'application/zip'}});
  });
  t.mock.method(URL,'createObjectURL',blob=>{blobs.push(blob);return 'blob:bundle'});
  t.mock.method(URL,'revokeObjectURL',()=>{});
  const previous=Object.getOwnPropertyDescriptor(globalThis,'document');
  globalThis.document={body:{append(){}},createElement(){return {click(){saved.push(this.download)},remove(){}}}};
  t.after(()=>{if(previous)Object.defineProperty(globalThis,'document',previous);else delete globalThis.document});
  const {dispatch}=createDelivery({state,token:()=>({nav}),notice:(...args)=>notes.push(args)});
  return {state,urls,saved,blobs,notes,fetchCall,navigate:()=>nav++,click:el=>dispatch('export',el)};
}

test('each available language renders both Markdown and pinned attachment ZIP buttons',()=>{
  const html=reportDownloads(snapshot());
  for(const language of ['zh-CN','en'])for(const format of ['report','report_bundle']){
    assert.deepEqual(button(html,language,format)?.dataset,{format,run:'run-A',reportId:'version-1',language});
  }
  assert.match(html,/中文 含附件 ZIP/);assert.match(html,/English 含附件 ZIP/);
  for(const missing of ['en','zh-CN']){
    const partial={...report,files:report.files.filter(f=>!f.name.endsWith('.'+missing+'.md'))};
    const rendered=reportDownloads(snapshot([partial]));
    assert.equal(button(rendered,missing),undefined);
    assert.equal(button(rendered,missing,'report'),undefined);
  }
  for(const reports of [[],[{...report,files:[]}],[{...report,source_status:'changed'}],[{...report,source_status:'not_checked'}]])assert.doesNotMatch(reportDownloads(snapshot(reports)),/data-format="report_bundle"/);
});

test('complete translations still show missing screenshot attachments in the report status',()=>{
  for(const translation_status of ['complete','translation_missing']){
    const html=reportDownloads(snapshot([{...report,translation_status,missing_attachments:['capture-A','capture-B']}]));
    assert.ok(html.includes('<span class="note">最终报告 v1'+(translation_status==='translation_missing'?' · 待补译':'')+' · 截图附件缺失 2 项</span>'));
    assert.equal(html.includes('待补译'),translation_status==='translation_missing');
    for(const language of ['zh-CN','en'])for(const format of ['report','report_bundle']){
      assert.deepEqual(button(html,language,format)?.dataset,{format,run:'run-A',reportId:'version-1',language});
    }
    assert.match(reportDownloads(snapshot([{...report,translation_status,missing_attachments:['capture-A']}]),true),/截图附件缺失 1 项/);
  }
  for(const missing_attachments of [undefined,[]]){
    assert.doesNotMatch(reportDownloads(snapshot([{...report,translation_status:'complete',missing_attachments}])),/截图附件缺失/);
  }
});

test('missing attachment warning keeps both language Markdown and ZIP download paths pinned',async t=>{
  const f=fixture(t);
  f.state.snap=snapshot([{...report,translation_status:'complete',missing_attachments:['capture-A']}]);
  const html=reportDownloads(f.state.snap);
  assert.match(html,/截图附件缺失 1 项/);
  for(const language of ['zh-CN','en'])for(const format of ['report','report_bundle']){
    await f.click(button(html,language,format));
  }
  assert.deepEqual(f.urls,[
    '/api/runs/run-A/export?format=report&report_id=version-1&language=zh-CN',
    '/api/runs/run-A/export?format=report_bundle&report_id=version-1&language=zh-CN',
    '/api/runs/run-A/export?format=report&report_id=version-1&language=en',
    '/api/runs/run-A/export?format=report_bundle&report_id=version-1&language=en',
  ]);
  assert.deepEqual(f.saved,[
    'trace-run-A-report.zh-CN.md','trace-run-A-report_bundle.zh-CN.zip',
    'trace-run-A-report.en.md','trace-run-A-report_bundle.en.zip',
  ]);
  assert.equal(f.blobs.length,4);
  for(const [i,blob] of f.blobs.entries())assert.equal(await blob.text(),'fixed version-1 '+(i<2?'zh-CN':'en'));
});

test('ZIP download carries immutable Run, report ID, language and zip fallback extension',async t=>{
  const f=fixture(t),old=button(reportDownloads(f.state.snap),'en');
  f.state.snap=snapshot([{...report,id:'version-2',version:2}]);
  await f.click(old);
  assert.deepEqual(f.urls,['/api/runs/run-A/export?format=report_bundle&report_id=version-1&language=en']);
  assert.equal(await f.blobs[0].text(),'fixed version-1 en');
  assert.equal(f.saved[0],'trace-run-A-report_bundle.en.zip');
  f.fetchCall.mock.mockImplementation(async url=>{f.urls.push(url);return new Response('zip archive',{headers:{'Content-Disposition':'attachment; filename="saved.zh-CN.zip"'}})});
  await f.click(button(reportDownloads(f.state.snap),'zh-CN'));
  assert.equal(f.saved[1],'saved.zh-CN.zip');
  assert.equal(f.urls[1],'/api/runs/run-A/export?format=report_bundle&report_id=version-2&language=zh-CN');
});

test('ZIP rejects cross-Run, missing version and unsupported language before any request',async t=>{
  const f=fixture(t),old=button(reportDownloads(f.state.snap),'en');
  f.state.snap=snapshot([report],'run-B');await f.click(old);
  f.state.selected='run-B';await f.click(old);
  f.state.selected='run-A';f.state.snap=snapshot();
  for(const dataset of [
    {format:'report_bundle'},
    {format:'report_bundle',run:'run-A',language:'en'},
    {format:'report_bundle',run:'run-A',reportId:'version-1',language:'fr'},
  ])await f.click({dataset});
  assert.deepEqual(f.urls,[]);assert.deepEqual(f.blobs,[]);assert.deepEqual(f.saved,[]);
});

test('Run or navigation changes suppress late ZIP downloads; a newer same-Run report keeps pinned bytes',async t=>{
  const f=fixture(t);
  for(const change of ['run','navigation','new-report']){
    f.state.selected='run-A';f.state.snap=snapshot();
    const started=Promise.withResolvers(),body=Promise.withResolvers();
    f.fetchCall.mock.mockImplementation(async url=>{f.urls.push(url);return {ok:true,headers:new Headers(),blob:()=>{started.resolve();return body.promise}}});
    const sending=f.click(button(reportDownloads(f.state.snap),'en'));
    await started.promise;
    if(change==='run'){f.state.selected='run-B';f.state.snap=snapshot([report],'run-B')}
    else if(change==='navigation')f.navigate();
    else f.state.snap=snapshot([{...report,id:'version-2',version:2}]);
    body.resolve(new Blob(['fixed version-1 en']));await sending;
  }
  assert.deepEqual(f.urls,Array(3).fill('/api/runs/run-A/export?format=report_bundle&report_id=version-1&language=en'));
  assert.equal(f.saved.length,1);assert.equal(f.blobs.length,1);
  assert.equal(await f.blobs[0].text(),'fixed version-1 en');
});

test('ZIP HTTP errors do not allocate a blob or trigger a browser download',async t=>{
  const f=fixture(t);
  f.fetchCall.mock.mockImplementation(async()=>new Response(JSON.stringify({error:'report attachment missing'}),{status:404,headers:{'Content-Type':'application/json'}}));
  await assert.rejects(f.click(button(reportDownloads(f.state.snap),'en')),/report attachment missing/);
  assert.deepEqual(f.blobs,[]);assert.deepEqual(f.saved,[]);
});

test('adding ZIP retains Markdown and JSON/evidence export selection',async t=>{
  const f=fixture(t);
  await f.click(button(reportDownloads(f.state.snap),'en','report'));
  assert.equal(f.saved[0],'trace-run-A-report.en.md');
  for(const format of ['json','evidence'])await f.click({dataset:{format}});
  assert.deepEqual(f.urls,[
    '/api/runs/run-A/export?format=report&report_id=version-1&language=en',
    '/api/runs/run-A/export?format=json',
    '/api/runs/run-A/export?format=evidence',
  ]);
});
