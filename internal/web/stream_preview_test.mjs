import {measureUI,performanceSample,uiNow} from './static/performance.js';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import {conversation,conversationPreview} from './static/conversation.js';
import {projectEvent,mergePreview,streamPreview,pendingModelCalls,streamedAction} from './static/live.js';
import {patchConversationPreview} from './static/interactions.js';

const base=()=>({run:{id:'preview',status:'running',event_seq:0},messages:[],requests:[{id:'current'}],responses:[],actions:[],events:[]});
const stream=(sequence,text,request='current',kind='text')=>({sequence,type:'model_stream',data:{request_id:request,delta:{kind,text}}});
const patchHTML=patch=>patch.full??patch.tail+patch.groups.flatMap(group=>group.calls.map(call=>call.html)).join('');

// No historical Markdown or event replay after the initial semantic render.
{
  const snap=base();let historicalReads=0,eventReads=0;
  snap.messages=Array.from({length:300},(_,i)=>({id:'old-'+i,role:'assistant',request_id:'old-request-'+i,get content(){historicalReads++;return 'SETTLED_MARKDOWN_'+i}}));
  snap.events=Array.from({length:2000},(_,i)=>({sequence:i+1,type:'model_stream',get data(){eventReads++;return {request_id:'old-request',delta:{kind:'text',text:'OLD_STREAM'}}}}));
  snap.run.event_seq=2000;
  conversation(snap);historicalReads=0;eventReads=0;
  const previousProjection=streamPreview(snap);
  projectEvent(snap,stream(2001,'LIVE_ONE'));
  const first=conversationPreview(snap);
  assert.equal(first.full,undefined);assert.match(first.tail,/LIVE_ONE/);assert.doesNotMatch(patchHTML(first),/SETTLED_MARKDOWN|OLD_STREAM/);
  projectEvent(snap,stream(2002,'_TWO'));
  assert.match(conversationPreview(snap).tail,/LIVE_ONE_TWO/);
  assert.equal(streamPreview(snap),previousProjection,'append-only stream reuses one projection');
  assert.equal(historicalReads,0,'preview does not read historical message content');
  assert.equal(eventReads,0,'preview does not fold historical events again');
  assert.equal(projectEvent(snap,stream(2002,'DUPLICATE')),false);
  assert.equal(streamPreview(snap).text,'LIVE_ONE_TWO');
}

// Long streams keep their folded prefix when the bounded display tail rolls.
// Paging can replace action metadata midway without replaying that trimmed tail.
{
  const snap=base();snap.messages_page={};snap.actions=[{request_id:'current',call_id:'long',status:'started'}];
  let reads=0,sequence=0,text='',thinking='',argumentsText='',stdout='';
  const append=(type,data)=>projectEvent(snap,{sequence:++sequence,type,get data(){reads++;return data}});
  append('model_request',{request_id:'current'});
  append('action_started',{request_id:'current',call_id:'long'});
  append('model_stream',{request_id:'current',delta:{kind:'tool_call',index:0,id:'long',name:'exec',arguments:'{'}});argumentsText='{';
  conversation(snap);reads=0;const events=snap.events;
  for(let i=0;i<2400;i++){
    const chunk=String(i)+',';
    if(i%4===0){text+=chunk;append('model_stream',{request_id:'current',delta:{kind:'text',text:chunk}})}
    if(i%4===1){thinking+=chunk;append('model_stream',{request_id:'current',delta:{kind:'thinking',text:chunk}})}
    if(i%4===2){argumentsText+=chunk;append('model_stream',{request_id:'current',delta:{kind:'tool_call',index:0,arguments:chunk}})}
    if(i%4===3){stdout+=chunk;append('tool_output',{call_id:'long',content:chunk})}
    if(i===1300)snap.actions=snap.actions.map(action=>({...action}));
    if(i%23===0)streamPreview(snap);
    assert.ok(snap.events.length<=700,'display event count stays bounded');
  }
  const preview=streamPreview(snap);
  assert.equal(snap.events,events,'tail pruning keeps stable array ownership');
  assert.equal(preview.text,text);assert.equal(preview.thinking,thinking);
  assert.equal(pendingModelCalls(snap,'current')[0].arguments,argumentsText);
  assert.equal(streamedAction(snap,snap.actions[0]).result.stdout,stdout,'legacy output ownership survives pruning');
  assert.equal(reads,2400,'each new delta folds exactly once across pruning and action refresh');
  snap.actions=[{...snap.actions[0],status:'completed',result:{stdout:'AUTHORITATIVE'}}];
  assert.equal(streamPreview(snap).targets.size,0,'settlement drops cached active output');
  append('model_request',{request_id:'next'});append('model_stream',{request_id:'next',delta:{kind:'text',text:'NEXT_ONLY'}});
  assert.equal(streamPreview(snap).text,'NEXT_ONLY','a new request never inherits the old prefix');
}

// New workspace generations carry the same active request across server tail
// limits, including an HTTP response overtaken by more than one event window.
{
  let snap=base();snap.messages_page={};snap.actions=[{request_id:'current',call_id:'call',status:'started'}];
  let sequence=0,text='PREFIX_',output='TOOL_PREFIX_';
  const append=(type,data)=>projectEvent(snap,{sequence:++sequence,type,data});
  append('model_request',{request_id:'current'});append('action_started',{request_id:'current',call_id:'call'});
  append('model_stream',{request_id:'current',delta:{kind:'text',text}});append('tool_output',{call_id:'call',content:output});
  for(let i=0;i<1200;i++){text+='x';append('model_stream',{request_id:'current',delta:{kind:'text',text:'x'}})}
  const stale={...snap,run:{...snap.run},requests:[{id:'current'}],actions:snap.actions.map(action=>({...action})),events:snap.events.slice(-500)};
  for(let i=0;i<900;i++){text+='y';append('model_stream',{request_id:'current',delta:{kind:'text',text:'y'}})}
  output+='_NEW';append('tool_output',{call_id:'call',content:'_NEW'});
  snap=mergePreview(stale,snap);
  assert.equal(streamPreview(snap).text,text,'stale semantic snapshot keeps all observed prefix despite pruned race events');
  assert.equal(streamedAction(snap,snap.actions[0]).result.stdout,output);
  const fresh={...snap,run:{...snap.run,event_seq:++sequence},requests:[{id:'current'}],actions:snap.actions.map(action=>({...action})),events:[...snap.events.slice(-499),{sequence,type:'model_stream',data:{request_id:'current',delta:{kind:'text',text:'HTTP_AHEAD'}}}]};
  snap=mergePreview(fresh,snap);text+='HTTP_AHEAD';
  assert.equal(streamPreview(snap).text,text,'snapshot-only committed frame folds once after inherited prefix');
  const final={...snap,run:{...snap.run,status:'completed'},messages:[{id:'settled',role:'assistant',request_id:'current',content:'CANONICAL_FINAL'}],responses:[{request_id:'current',response:{}}],actions:[{...snap.actions[0],status:'completed',result:{stdout:'CANONICAL_OUTPUT'}}],events:[]};
  snap=mergePreview(final,snap);assert.equal(streamPreview(snap).targets.size,0);
  const html=conversation(snap);assert.match(html,/CANONICAL_FINAL/);assert.doesNotMatch(html,/PREFIX_|HTTP_AHEAD|live-message/);
  const other=mergePreview({...base(),run:{id:'other',event_seq:0}},snap);assert.equal(streamPreview(other).text,'','cross-run snapshots never inherit');
  const switched=mergePreview({...base(),run:{...snap.run,event_seq:sequence+1},requests:[{id:'next'}]},snap);assert.equal(streamPreview(switched).text,'','request change never inherits');
}

// A late workspace response may predate a new request whose request/owner
// events have already fallen out of the browser tail. Preserve that identity.
{
  const snap=base();snap.messages_page={};snap.requests=[{id:'old'}];
  snap.actions=[{request_id:'old',call_id:'same',status:'started'}];
  let sequence=0;const append=(type,data)=>projectEvent(snap,{sequence:++sequence,type,data});
  append('model_request',{request_id:'old'});append('action_started',{request_id:'old',call_id:'same'});
  const stale={...snap,run:{...snap.run},requests:[{id:'old'}],actions:snap.actions.map(action=>({...action})),events:[...snap.events]};
  append('model_request',{request_id:'new'});
  append('model_stream',{request_id:'new',delta:{kind:'tool_call',index:0,id:'same',name:'exec',arguments:'{NEW}'}});
  append('action_started',{request_id:'new',call_id:'same'});
  append('tool_output',{call_id:'same',content:'NEW_PREFIX'});
  for(let i=0;i<900;i++)append('model_stream',{request_id:'new',delta:{kind:'text',text:'n'}});
  const merged=mergePreview(stale,snap);
  assert.equal(streamPreview(merged).latest.id,'new');assert.equal(streamPreview(merged).text,'n'.repeat(900));
  projectEvent(merged,{sequence:++sequence,type:'tool_output',data:{call_id:'same',content:'_TAIL'}});
  const call=pendingModelCalls(merged,'new')[0];
  assert.equal(call.arguments,'{NEW}');assert.equal(streamedAction(merged,call).result.stdout,'NEW_PREFIX_TAIL');
  assert.equal(streamedAction(merged,merged.actions[0]).result,undefined,'reused old call cannot receive the new output');
}

// Action metadata may arrive after its output. A new target must fold available
// history even while existing targets keep their nonduplicated output prefix.
{
  const snap=base();snap.actions=[{request_id:'current',call_id:'known',status:'started'}];
  for(const [index,call_id] of ['known','late'].entries())projectEvent(snap,{sequence:index+1,type:'tool_output',data:{request_id:'current',call_id,content:call_id+'_OUTPUT'}});
  streamPreview(snap);snap.actions=[...snap.actions,{request_id:'current',call_id:'late',status:'started'}];
  assert.equal(streamedAction(snap,snap.actions[0]).result.stdout,'known_OUTPUT');
  assert.equal(streamedAction(snap,snap.actions[1]).result.stdout,'late_OUTPUT');
}
{
  const snap=base();snap.events=[{sequence:1,type:'model_request',data:{request_id:'older'}},{sequence:2,type:'action_started',data:{request_id:'older',call_id:'late'}},{sequence:3,type:'tool_output',data:{call_id:'late',content:'EARLY'}},{sequence:4,type:'model_request',data:{request_id:'current'}}];snap.run.event_seq=4;
  streamPreview(snap);snap.actions=[{request_id:'older',call_id:'late',status:'started'}];streamPreview(snap);
  projectEvent(snap,{sequence:5,type:'tool_output',data:{call_id:'late',content:'_LATE'}});
  assert.equal(streamedAction(snap,snap.actions[0]).result.stdout,'EARLY_LATE','late target retains replayed owner for future unannotated output');
}

// Reused tool IDs are owned by request, including old events without request_id.
{
  const snap=base();
  snap.actions=[{call_id:'same',request_id:'old',name:'exec',arguments:'{}',status:'completed',result:{stdout:'OLD_SETTLED'}},{call_id:'same',request_id:'current',name:'exec',arguments:'{}',status:'started'}];
  const events=[
    ['model_request',{request_id:'old'}],['action_started',{request_id:'old',call_id:'same'}],['tool_output',{call_id:'same',content:'OLD_OUTPUT'}],
    ['model_request',{request_id:'current'}],['action_started',{request_id:'current',call_id:'same'}],['tool_output',{call_id:'same',content:'NEW_OUTPUT'}],
  ];
  events.forEach(([type,data],i)=>projectEvent(snap,{sequence:i+1,type,data}));
  conversation(snap);
  projectEvent(snap,{sequence:7,type:'tool_progress',data:{call_id:'same',message:'CURRENT_PROGRESS'}});
  const patch=conversationPreview(snap),html=patchHTML(patch);
  assert.equal(patch.groups.length,1);assert.equal(patch.groups[0].calls.length,1);
  assert.equal(patch.groups[0].calls[0].id,'chat-item-call-current:same');
  assert.match(html,/NEW_OUTPUT/);assert.match(html,/CURRENT_PROGRESS/);assert.doesNotMatch(html,/OLD_OUTPUT|OLD_SETTLED/);
  assert.equal(streamPreview(snap).targets.size,1,'settled tool output is not retained in preview cache');
}

// Tool arguments/output can lead the semantic action ledger by one refresh.
{
  const snap=base();conversation(snap);
  projectEvent(snap,{sequence:1,type:'model_stream',data:{request_id:'current',delta:{kind:'tool_call',index:0,id:'streamed',name:'exec',arguments:'{"cmd":'}}});
  projectEvent(snap,{sequence:2,type:'model_stream',data:{request_id:'current',delta:{kind:'tool_call',index:0,arguments:'"echo ok"}'}}});
  projectEvent(snap,{sequence:3,type:'action_started',data:{request_id:'current',call_id:'streamed'}});
  projectEvent(snap,{sequence:4,type:'tool_output',data:{call_id:'streamed',content:'EARLY_OUTPUT'}});
  assert.equal(pendingModelCalls(snap,'current')[0].arguments,'{"cmd":"echo ok"}');
  assert.match(conversationPreview(snap).tail,/EARLY_OUTPUT/);
  assert.equal(streamedAction(snap,pendingModelCalls(snap,'current')[0]).result.stdout,'EARLY_OUTPUT');
}

// Bounded tool previews retain source-byte gaps and do not replay a final tail.
{
  const snap=base();snap.actions=[{request_id:'current',call_id:'bounded',status:'started'}];
  let sequence=0;
  const append=(stream,content,offset,end_offset,next_offset)=>projectEvent(snap,{sequence:++sequence,type:'tool_output',data:{request_id:'current',call_id:'bounded',stream,content,offset,end_offset,next_offset}});
  const output=()=>streamedAction(snap,snap.actions[0]).result;
  append('stdout','HEAD',0,4,100);assert.match(output().stdout,/已省略 96 字节/);
  append('stderr','err',0,3,3);assert.equal(output().stderr,'err');
  append('stdout','TAIL',96,100,100);
  const withTail=output().stdout;
  assert.equal(withTail,'HEAD\n[已省略 92 字节；完整输出见原始产物]\nTAIL');
  append('stdout','TAIL',96,100,100);append('stdout','OLD',4,7,7);
  assert.equal(output().stdout,withTail,'duplicate and out-of-order ranges are not new output');
  append('stdout','TAIL结束',96,106,106);
  assert.equal(output().stdout,withTail+'结束','overlap is trimmed by UTF-8 bytes');
  snap.actions=snap.actions.map(action=>({...action}));
  assert.equal(output().stdout,withTail+'结束','action refresh preserves byte watermarks');
  const replacement={...snap,run:{...snap.run},requests:[...snap.requests],actions:snap.actions.map(action=>({...action})),events:snap.events.slice(-2)};
  const merged=mergePreview(replacement,snap);
  projectEvent(merged,{sequence:++sequence,type:'tool_output',data:{request_id:'current',call_id:'bounded',stream:'stdout',content:'结束',offset:100,end_offset:106,next_offset:106}});
  assert.equal(streamedAction(merged,merged.actions[0]).result.stdout,withTail+'结束','workspace replay does not duplicate a previously folded tail');
}

// Older offset previews have no explicit end; legacy no-offset events still fold.
{
  const snap=base();snap.actions=[{request_id:'current',call_id:'old-offset',status:'started'},{request_id:'current',call_id:'legacy',status:'started'}];
  const events=[
    {call_id:'old-offset',content:'汉字',offset:0,next_offset:20},
    {call_id:'old-offset',content:'尾',offset:17,next_offset:20},
    {call_id:'legacy',content:'legacy '},{call_id:'legacy',content:'output'},
  ];
  events.forEach((data,index)=>projectEvent(snap,{sequence:index+1,type:'tool_output',data:{request_id:'current',...data}}));
  assert.equal(streamedAction(snap,snap.actions[0]).result.stdout,'汉字\n[已省略 11 字节；完整输出见原始产物]\n尾');
  assert.equal(streamedAction(snap,snap.actions[1]).result.stdout,'legacy output');
}

// Binary display replacements retain raw byte ranges, including an empty tail.
{
  const snap=base();snap.actions=[{request_id:'current',call_id:'binary',status:'started'}];
  for(const [index,data] of [
    {content:'?',offset:0,end_offset:4096,next_offset:8192},
    {content:'',offset:8192,end_offset:8192,next_offset:8192},
    {content:'',offset:8192,end_offset:8192,next_offset:8192},
  ].entries())projectEvent(snap,{sequence:index+1,type:'tool_output',data:{request_id:'current',call_id:'binary',...data}});
  assert.equal(streamedAction(snap,snap.actions[0]).result.stdout,'?\n[已省略 4096 字节；完整输出见原始产物]\n');
}

// Errors, empty terminal responses, final overlap and manual replies stay honest.
{
  const snap=base();conversation(snap);projectEvent(snap,stream(1,'PARTIAL'));
  projectEvent(snap,{sequence:2,type:'model_error',data:{request_id:'current',error:'STREAM_FAILED'}});
  assert.match(conversationPreview(snap).tail,/PARTIAL/);assert.match(conversationPreview(snap).tail,/STREAM_FAILED/);
  const final={...base(),run:{...snap.run,status:'completed'},messages:[{id:'final',role:'assistant',request_id:'current',content:'FINAL'}],responses:[{request_id:'current',response:{}}]};
  const merged=mergePreview({...final,run:{...final.run,event_seq:0}},snap);
  const patch=conversationPreview(merged);
  assert.ok(patch.full);assert.equal(patch.full.split('id="live-current"').length-1,1);assert.doesNotMatch(patch.full,/live-message|PARTIAL|STREAM_FAILED/);
  const manual={...base(),messages:[{id:'assistant',role:'assistant',request_id:'current',tool_calls:[{id:'manual',name:'exec',arguments:'{}'}]},{id:'reply',role:'tool',tool_call_id:'manual',content:'{"stdout":"MANUAL_RESULT"}'}]};
  assert.match(conversation(manual),/MANUAL_RESULT/);assert.equal(conversationPreview(manual).groups.length,0);
  assert.doesNotMatch(conversation({...base(),responses:[{request_id:'current',response:{}}]}),/live-message/);
  assert.match(conversation({...base(),responses:[{request_id:'current',error:'EMPTY_RESPONSE_ERROR'}]}),/EMPTY_RESPONSE_ERROR/);
}

// Minimal DOM adapter runs the real range reconciler without a new dependency.
class Node {
  constructor(tag='#fragment',attrs={}){this.nodeName=tag.toUpperCase();this.tagName=this.nodeName;this.nodeType=tag==='#text'?3:tag==='#fragment'?11:1;this.attrs={...attrs};this.childNodes=[];this.parentNode=null;this.data='';this.scrollTop=0;this.scrollLeft=0;this.scrollHeight=600;this.clientHeight=100;this.open='open' in attrs}
  get id(){return this.attrs.id}get attributes(){return Object.entries(this.attrs).map(([name,value])=>({name,value}))}get dataset(){return {scroll:this.attrs['data-scroll']}}
  get firstChild(){return this.childNodes[0]||null}get nextSibling(){return this.parentNode?.childNodes[this.parentNode.childNodes.indexOf(this)+1]||null}get previousSibling(){return this.parentNode?.childNodes[this.parentNode.childNodes.indexOf(this)-1]||null}
  hasAttribute(name){return name in this.attrs}getAttribute(name){return this.attrs[name]??null}setAttribute(name,value){this.attrs[name]=value}removeAttribute(name){delete this.attrs[name]}
  insertBefore(node,before){node.remove();const index=before?this.childNodes.indexOf(before):this.childNodes.length;assert.ok(index>=0);this.childNodes.splice(index,0,node);node.parentNode=this}
  remove(){if(this.parentNode){const siblings=this.parentNode.childNodes;siblings.splice(siblings.indexOf(this),1);this.parentNode=null}}
  cloneNode(deep){const node=new Node(this.nodeName,this.attrs);node.nodeType=this.nodeType;node.data=this.data;node.open=this.open;if(deep)for(const child of this.childNodes)node.insertBefore(child.cloneNode(true),null);return node}
  contains(node){return node===this||this.childNodes.some(child=>child.contains(node))}
  querySelectorAll(selector){if(this.forbidScan)throw Error('historical subtree scanned');const matches=node=>selector==='[data-scroll]'?node.hasAttribute('data-scroll'):node.tagName===selector.toUpperCase();return this.childNodes.flatMap(node=>[...(node.nodeType===1&&matches(node)?[node]:[]),...node.querySelectorAll(selector)])}
  querySelector(selector){return this.querySelectorAll(selector)[0]||null}
}
function parse(html){
  const root=new Node(),stack=[root];
  for(const token of html.match(/<[^>]+>|[^<]+/g)||[]){
    if(token.startsWith('</')){stack.pop();continue}
    let node;if(token.startsWith('<')){const tag=token.match(/^<([a-z0-9-]+)/i)[1],attrs=Object.fromEntries([...token.matchAll(/([a-z0-9_-]+)="([^"]*)"/gi)].map(match=>[match[1],match[2]]));if(/ open(?: |>)/.test(token))attrs.open='';node=new Node(tag,attrs);stack.at(-1).insertBefore(node,null);stack.push(node)}
    else{node=new Node('#text');node.data=token;stack.at(-1).insertBefore(node,null)}
  }return root;
}
{
  const previous=globalThis.document,root=parse('<article id="settled"><details id="settled-open" open><summary>历史</summary></details></article><template id="conversation-live-start"></template><article id="live-current"><details id="live-thinking-current" data-reasoning-phase="active" open><summary>思考</summary><div data-scroll="reasoning">one</div></details></article><template id="conversation-live-end"></template>');
  const byID=id=>{const visit=node=>node.id===id?node:node.childNodes.map(visit).find(Boolean);return visit(root)};
  const settled=byID('settled'),thinking=byID('live-thinking-current'),live=byID('live-current');settled.forbidScan=true;thinking.open=false;root.scrollTop=120;root.scrollLeft=7;
  const input={id:'draft',tagName:'TEXTAREA',value:'KEEP_DRAFT',selectionStart:2,selectionEnd:5,focus(){this.focused=true},setSelectionRange(a,b){this.selectionStart=a;this.selectionEnd=b}};
  globalThis.document={activeElement:input,getElementById:id=>id==='draft'?input:byID(id),createElement:()=>({set innerHTML(html){this.content=parse(html)}})};
  try{
    assert.equal(patchConversationPreview(root,{groups:[],tail:'<article id="live-current"><details id="live-thinking-current" data-reasoning-phase="active" open><summary>思考</summary><div data-scroll="reasoning">one two</div></details></article>'}),true);
    assert.equal(byID('settled'),settled);assert.equal(byID('live-current'),live);assert.equal(byID('live-thinking-current'),thinking);assert.equal(thinking.open,false);
    assert.equal(root.scrollTop,120);assert.equal(root.scrollLeft,7);assert.equal(input.value,'KEEP_DRAFT');assert.equal(globalThis.document.activeElement,input);assert.equal(input.focused,undefined,'unchanged focused input is not refocused during IME composition');assert.equal(input.selectionStart,2);assert.equal(input.selectionEnd,5);
    patchConversationPreview(root,{groups:[],tail:'<article id="live-current"><details id="live-thinking-current" data-reasoning-phase="done"><summary>思考</summary><div data-scroll="reasoning">one two</div></details></article>'});
    assert.equal(thinking.open,false,'settled thinking closes at phase transition');
    root.scrollTop=500;patchConversationPreview(root,{groups:[],tail:''});assert.equal(root.scrollTop,root.scrollHeight,'tail readers remain pinned to bottom');assert.equal(byID('settled'),settled);
  }finally{if(previous===undefined)delete globalThis.document;else globalThis.document=previous}
}
// Exercise the production scheduler, including the background-tab final frame.
{
  const app=fs.readFileSync(new URL('./static/app.js',import.meta.url),'utf8'),timers=[],patches=[],fallback=[],seq={},element={};
  const snap=base();conversation(snap);
  const state={page:'history',historyDetail:true,taskTab:'activity',snap};
  const context=vm.createContext({measureUI,performanceSample,uiNow,state,document:{hidden:true,querySelector:()=>element},$:()=>seq,setTimeout(fn,ms){timers.push({fn,ms});return timers.length},conversationPreview,conversation,patchConversationPreview(node,patch){assert.equal(node,element);patches.push(patch);return true},patchLive:(_,html)=>fallback.push(html)});
  vm.runInContext('let paintFrame;'+app.slice(app.indexOf('function paintSoon(){'),app.indexOf('async function refreshList(')),context);
  for(let i=1;i<=20;i++){projectEvent(snap,stream(i,'x'));vm.runInContext('paintSoon()',context)}
  assert.equal(timers.length,1);assert.equal(timers[0].ms,50);timers[0].fn();
  assert.equal(patches.length,1);assert.match(patches[0].tail,/xxxxxxxxxxxxxxxxxxxx/);assert.equal(seq.textContent,20);assert.equal(fallback.length,0);
  projectEvent(snap,stream(21,'FINAL_FLUSH'));vm.runInContext('paintSoon()',context);timers[1].fn();assert.match(patches[1].tail,/FINAL_FLUSH/);
  state.page='settings';vm.runInContext('paintSoon()',context);timers[2].fn();assert.equal(patches.length,2,'navigation prevents stale painting');
  state.page='history';context.patchConversationPreview=()=>false;vm.runInContext('paintSoon()',context);timers[3].fn();assert.equal(fallback.length,1,'a missing mounted range is reconciled once');
}
console.log('Stream preview: incremental folding, bounded snapshot cache, request-scoped tools, final flush, historical isolation, disclosure/draft/scroll preservation passed.');
