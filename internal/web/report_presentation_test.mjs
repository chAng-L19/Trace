import assert from 'node:assert/strict';
import {markdown,messageBody,streamingBody} from './static/presentation.js';
import {esc} from './static/ui.js';

const hash='a1'.repeat(32),preview=`/api/artifacts/${hash}/download?preview=1`;
const report={trace_report:{zh_cn:'# 中文正文\n\n- 已验证\n\n![检查截图]('+preview+')',en:'ENGLISH_REPORT_ONLY',source_digest:'REPORT_DIGEST_ONLY',documents:['REPORT_DOCUMENT_ONLY']},source_digest:'SOURCE_DIGEST_ONLY',documents:['DOCUMENT_ONLY']};
const raw=JSON.stringify(report);
const fence=(text,marker='```',language='json')=>`${marker}${language}\n${text}\n${marker}`;
const render=(content,role='assistant')=>messageBody({id:'report',role,content});
// All generated disclosures default closed. Remove nested disclosures to inspect
// what the user sees before opening raw/result details, without a DOM dependency.
function visible(html){
  assert.doesNotMatch(html,/<details\b[^>]*\sopen(?:\s|=|>)/);
  let depth=0;
  const text=html.split(/(<\/?details\b[^>]*>)/).filter(part=>{
    if(part.startsWith('<details')){depth++;return false}
    if(part.startsWith('</details')){depth--;assert.ok(depth>=0);return false}
    return depth===0;
  }).join('');
  assert.equal(depth,0,'balanced disclosures');
  return text;
}
function assertRaw(html,content){
  assert.ok(html.includes(`<pre>${esc(content)}</pre>`),'the complete original message is retained verbatim in escaped raw text');
  assert.match(html,/class="request-details raw-record"/);
}
function assertReport(html,content){
  const body=visible(html);
  assert.match(body,/<h2>中文正文<\/h2>/);
  assert.match(body,/<li>已验证<\/li>/);
  assert.ok(body.includes(`<img class="artifact-media" src="${preview}" alt="检查截图">`));
  assert.doesNotMatch(body,/ENGLISH_REPORT_ONLY|DIGEST_ONLY|DOCUMENT_ONLY|trace_report|zh_cn|class="code-block"/);
  assertRaw(html,content);
}

// Bare and closed report fences share the report view, including mixed prose.
for(const content of [raw,fence(raw),fence(raw,'~~~'),fence(raw,'````'),fence(raw).replace(/\n/g,'\r\n'),'# 执行概览\n\n'+fence(raw)+'\n\n后续说明。']){
  assertReport(render(content),content);
  assertReport(streamingBody(content,'closed-report'),content);
}
const mixed='# 执行概览\n\n| 项目 | 状态 |\n| --- | --- |\n| 探针 | 完成 |\n\n'+fence(raw)+'\n\n后续说明。';
const mixedHTML=render(mixed);
assertReport(mixedHTML,mixed);
assert.match(visible(mixedHTML),/<h2>执行概览<\/h2>/);
assert.match(visible(mixedHTML),/<table>/);
assert.match(visible(mixedHTML),/<p>后续说明。<\/p>/);
assertReport(render(raw,'report'),raw);
assertReport(messageBody({id:'result-panel',content:raw}),raw);
for(const zh_cn of [undefined,null,'',' \n\t ',false]){
  const content=JSON.stringify({trace_report:{zh_cn,en:'# English fallback'},source_digest:'HIDDEN_FALLBACK_DIGEST'});
  assert.match(visible(render(content)),/<h2>English fallback<\/h2>/);
  assert.doesNotMatch(visible(render(content)),/HIDDEN_FALLBACK_DIGEST/);
  assertRaw(render(content),content);
}
for(const trace_report of [null,{},false,{zh_cn:42,en:{text:'WRONG_SHAPE'}},{documents:[null,{en:'  '},{subject_id:'WRONG_SHAPE'}]}]){
  const content=JSON.stringify({trace_report,source_digest:'HIDDEN_EMPTY_DIGEST'});
  assert.match(visible(render(content)),/报告正文为空/);
  assert.doesNotMatch(visible(render(content)),/HIDDEN_EMPTY_DIGEST|WRONG_SHAPE/);
  assertRaw(render(content),content);
}

// Documents use a language fallback per subject, not once for the envelope.
const documents=[
  {kind:'finding',subject_id:'SUBJECT_ONLY',source_digest:'DIGEST_ONLY',zh_cn:'# 发现正文',en:'TRANSLATION_ONLY'},
  null,{kind:'stage',subject_id:'STAGE_ONLY',zh_cn:'  ',en:'**Stage body** <script>bad</script>'},
];
for(const top of [{},{zh_cn:'# 总报告'}]){
  const payload=JSON.stringify({trace_report:{...top,documents}});
  for(const content of [payload,fence(payload)]){
    const html=render(content),body=visible(html);
    assert.match(body,/<h2>发现正文<\/h2>[\s\S]*<strong>Stage body<\/strong> &lt;script&gt;bad&lt;\/script&gt;/);
    if(top.zh_cn)assert.match(body,/<h2>总报告<\/h2>[\s\S]*发现正文/);
    assert.doesNotMatch(body,/报告正文为空|TRANSLATION_ONLY|SUBJECT_ONLY|DIGEST_ONLY|STAGE_ONLY|subject_id|trace_report|<script>/);
    assertRaw(html,content);
  }
}

// Coexisting result fields are available only in a closed details element.
for(const key of ['trace_result','final_result']){
  const result={summary:'RESULT_SUMMARY_ONLY',answers:[{value:'RESULT_ANSWER_ONLY',evidence_ids:['SOURCE/EVIDENCE']}],cleanup_status:'RESULT_CLEANUP_ONLY'};
  for(const envelope of [{...report,[key]:result},{[key]:result,...report}]){
    for(const content of [JSON.stringify(envelope),'说明。\n\n'+fence(JSON.stringify(envelope))]){
      const html=render(content);
      assertReport(html,content);
      assert.doesNotMatch(visible(html),/RESULT_SUMMARY_ONLY|RESULT_ANSWER_ONLY|RESULT_CLEANUP_ONLY/);
      assert.match(html,/<details[^>]*class="request-details"><summary>结果详情<\/summary>/);
      assert.match(html,/RESULT_SUMMARY_ONLY/);
      assert.match(html,/RESULT_ANSWER_ONLY/);
    }
  }
}
const twice=fence(raw)+'\n\n第二部分\n\n'+fence(raw);
assert.equal((render(twice).match(/class="request-details raw-record"/g)||[]).length,1,'report fences retain a single whole-message raw disclosure');
assertRaw(render(twice),twice);

// Formal reports embedded in result envelopes take precedence over the summary.
for(const key of ['trace_result','final_result']){
  for(const report of [{zh_cn:'# 内嵌中文报告\n\n![内嵌截图]('+preview+')',en:'EMBEDDED_EN_ONLY'},{en:'# Embedded fallback\n\n![embedded]('+preview+')'},{zh_cn:' \n ',en:'# Embedded fallback\n\n![embedded]('+preview+')'}]){
    const raw=JSON.stringify({[key]:{summary:'EMBEDDED_SUMMARY_ONLY',report,answers:[{value:'EMBEDDED_ANSWER_ONLY',evidence_ids:['child/ev']}],cleanup_status:'EMBEDDED_CLEANUP_ONLY'}});
    for(const content of [raw,fence(raw),'先前检查。\n\n'+fence(raw)+'\n\n后续说明。']){
      for(const html of [render(content),streamingBody(content,'embedded-complete')]){
        const body=visible(html);
        assert.match(body,report.zh_cn?.trim()?/<h2>内嵌中文报告<\/h2>/:/<h2>Embedded fallback<\/h2>/);
        assert.match(body,/<img class="artifact-media"/);
        assert.doesNotMatch(body,/EMBEDDED_EN_ONLY|EMBEDDED_SUMMARY_ONLY|EMBEDDED_ANSWER_ONLY|EMBEDDED_CLEANUP_ONLY|class="code-block"/);
        assert.match(html,/<summary>结果详情<\/summary>/);
        assert.match(html,/EMBEDDED_SUMMARY_ONLY/);
        assert.match(html,/EMBEDDED_ANSWER_ONLY/);
        assertRaw(html,content);
      }
    }
    const unclosed='```json\n'+raw;
    for(const html of [render(unclosed),streamingBody(unclosed,'embedded-live')]){
      assert.doesNotMatch(visible(html),/内嵌中文报告|Embedded fallback|EMBEDDED_SUMMARY_ONLY|<img\b|class="code-block"/);
      assertRaw(html,unclosed);
    }
    assert.equal(render(fence(raw),'user'),markdown(fence(raw)));
  }
  for(const report of [undefined,{},null,{zh_cn:' ',en:' '},{zh_cn:42,en:false}]){
    const content=JSON.stringify({[key]:{summary:'无有效报告仍展示摘要',report,answers:[{value:'原答案',evidence_ids:['ev']}]}});
    assert.match(visible(render(content)),/无有效报告仍展示摘要/);
    assert.match(visible(render(content)),/原答案/);
  }
}

// User examples and non-report roles are never promoted into report content.
const example=JSON.stringify({trace_report:{zh_cn:'# 示例报告',en:'EXAMPLE_EN'}});
for(const content of [example,fence(example),'使用示例：\n\n'+fence(example)]){
  assert.equal(render(content,'user'),markdown(content));
  assert.doesNotMatch(render(content,'user'),/<h2>示例报告<\/h2>|raw-record/);
}
for(const role of ['tool','system']){
  assert.equal(render(fence(example),role),markdown(fence(example)));
  assert.equal(render('示例\n\n'+fence(example),role),markdown('示例\n\n'+fence(example)));
}
for(const content of [fence(example,'```','js'),fence(example,'```','text'),fence(example,'```',''),fence('{"example":1}'),fence(JSON.stringify({example:JSON.parse(example)})),fence(JSON.stringify({text:'{"trace_report":{"zh_cn":"example"}}'}))]){
  assert.equal(render(content),markdown(content),'ordinary fenced code stays Markdown code');
}

// Incomplete bare JSON and unclosed JSON fences do not expose partial reports.
for(const content of ['{','{"trace_rep','{"trace_report":{"zh_cn":"PARTIAL_REPORT_ONLY','{"source_digest":"PARTIAL_DIGEST_ONLY","trace_report":{"zh_cn":"PARTIAL_REPORT_ONLY']){
  const html=streamingBody(content,'bare-live');
  assert.match(visible(html),/正在整理结构化/);
  assert.doesNotMatch(visible(html),/PARTIAL_REPORT_ONLY|PARTIAL_DIGEST_ONLY|trace_rep/);
  assertRaw(html,content);
}
for(const tail of ['{','{"trace_rep','{"trace_report":{"zh_cn":"PARTIAL_REPORT_ONLY',raw,'{"source_digest":"PARTIAL_DIGEST_ONLY","trace_report":{"zh_cn":"PARTIAL_REPORT_ONLY',JSON.stringify({trace_result:{summary:'RESULT_FIRST_ONLY'},...report})]){
  const content='# 已完成的检查\n\n```json\n'+tail;
  const html=streamingBody(content,'fenced-live'),body=visible(html);
  assert.match(body,/<h2>已完成的检查<\/h2>/);
  assert.match(body,/正在整理结构化/);
  assert.doesNotMatch(body,/PARTIAL_REPORT_ONLY|PARTIAL_DIGEST_ONLY|RESULT_FIRST_ONLY|中文正文|trace_rep|class="code-block"/);
  if(tail.includes('"trace_report"'))assertRaw(html,content);
}
for(const content of ['```json\n'+raw,'````json\n'+raw+'\n```']){
  const html=render(content);
  assert.doesNotMatch(visible(html),/中文正文|trace_report|DIGEST_ONLY|class="code-block"/);
  assert.match(visible(html),/未完整解析/);
  assertRaw(html,content);
}
for(const content of ['示例\n\n```json\n{"example":','示例\n\n```json\n{"example":{"trace_report":','示例\n\n```json\n{"text":"escaped \\"trace_report\\":']){
  assert.match(streamingBody(content,'ordinary-live'),/class="code-block"/,'incomplete ordinary examples are still code');
}
const completed=streamingBody('# 完成\n\n'+fence(raw)+'\n\n结束。','complete-live');
assert.match(visible(completed),/<h2>中文正文<\/h2>/);
assert.match(visible(completed),/<p>结束。<\/p>/);

// Existing result-only rendering, mixed result fences and normal JSON stay usable.
const resultOnly=JSON.stringify({trace_result:{summary:'既有结果摘要',answers:[{value:'既有答案',evidence_ids:['ev']}],reproduction_steps:['既有步骤']}});
assert.match(visible(render(resultOnly)),/既有结果摘要/);
assert.match(visible(render(resultOnly)),/既有答案/);
const mixedResult='前置正文\n\n'+fence(resultOnly)+'\n\n后续正文';
assert.match(render(mixedResult),/结构化结果详情/);
assert.match(render(mixedResult),/既有答案/);
assert.match(visible(render(mixedResult)),/后续正文/);
assert.doesNotMatch(render(mixedResult),/class="code-block"/);
assert.equal(render(mixedResult,'user'),markdown(mixedResult));
assert.match(streamingBody('普通示例\n\n```json\n{"example":','example-live'),/class="code-block"/);
assert.match(visible(streamingBody('# 结果\n\n```json\n{"trace_result":{"summary":"未完成','old-live')),/正在整理结构化结果/);

// Only exact same-site CAS preview paths produce images, with escaped attributes.
for(const path of [preview,preview.replace(hash,hash.toUpperCase())]){
  const html=markdown(`![截图](${path})`);
  assert.ok(html.includes(`<img class="artifact-media" src="${path}" alt="截图">`));
}
assert.ok(markdown(`![](${preview})`).includes('alt=""'));
const hostileAlt='"><img src=x onerror="alert(1)">&\'';
const altHTML=markdown(`![${hostileAlt}](${preview})`);
assert.ok(altHTML.includes(`alt="${esc(hostileAlt)}"`));
assert.equal((altHTML.match(/<img\b/g)||[]).length,1,'hostile alt stays inside a single escaped attribute');
assert.doesNotMatch(altHTML,/\sonerror="/);
for(const url of [
  'https://outside.test/image.png','//outside.test/image.png','https://trace.test'+preview,
  `/api/artifacts/${hash}/download`,preview+'&extra=1',preview+'#fragment',preview.replace('preview=1','preview=2'),
  preview.replace(hash,'a'.repeat(63)),preview.replace(hash,'a'.repeat(65)),preview.replace(hash,'g'.repeat(64)),
  '/api/artifacts/../'+hash+'/download?preview=1',preview.replace('/download','%2fdownload'),
  '/\\outside.test/image.png',preview+'"onload="bad',
  'data:image/svg+xml,<svg/onload=alert(1)>','javascript:alert(1)',
]){
  const html=markdown(`![可读图片](${url})`);
  assert.doesNotMatch(html,/<img\b|<svg\b|<script\b|href="(?:javascript|data):|\sonload="/,'no automatic image fetch for '+url);
  assert.match(html,/可读图片/);
}
assert.match(markdown('![外部截图](https://outside.test/image.png)'),/<a href="https:\/\/outside.test\/image.png" target="_blank" rel="noopener noreferrer">外部截图<\/a>/);
assert.match(markdown('[正常链接](https://example.test/)'),/<a href="https:\/\/example.test\/" target="_blank" rel="noopener noreferrer">正常链接<\/a>/);
assert.match(markdown('[下载](/api/artifacts/'+hash+'/download)'),/href="\/api\/artifacts\//);
assert.doesNotMatch(markdown('`![截图]('+preview+')`'),/<img\b/);
assert.doesNotMatch(markdown(fence('![截图]('+preview+')','```','md')),/<img\b/);
assert.doesNotMatch(markdown('<img src="'+preview+'" onerror="bad">\n<script>bad</script>'),/<img\b|<script\b/);

console.log('Report presentation: Chinese/fallback body, closed/raw result disclosures, role/code boundaries, streaming envelopes, exact CAS previews and escaped image URLs/alt.');

// Attachment selection uses these same inline and table rendering boundaries.
const tick=String.fromCharCode(96),download=preview.replace('?preview=1','');
assert.doesNotMatch(markdown('[]('+download+')'),/<a\b/);
assert.match(markdown('[download '+tick+'capture.png'+tick+']('+download+')'),/<a href=/);
assert.doesNotMatch(markdown('| literal | example |\n| --- | --- |\n| '+tick+' | '+tick+'![sample]('+preview+')'+tick+' |'),/<img\b/);

assert.doesNotMatch(markdown('## example: '+tick+'command | [download]('+download+')'+tick+'\n---'),/<a\b/);
assert.doesNotMatch(markdown('[![thumbnail]('+preview+')](/detail)'),/<img\b/);
