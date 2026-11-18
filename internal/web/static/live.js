// Muteki useRun.ts @ 144df766 (AGPL-3.0): fold committed SSE events,
// coalesce paint, and reconcile at semantic boundaries. No second event store.
export const isPreviewEvent=event=>['model_stream','tool_output','tool_progress','mcp_progress'].includes(event.type);
export function projectEvent(snap,event){
  if(!snap||event.sequence<=(snap.run.event_seq||0))return false;
  snap.events??=[];snap.events.push(event);snap.run.event_seq=event.sequence;
  if(event.type==='model_request'){
    snap.requests??=[];
    if(!snap.requests.some(r=>r.id===event.data?.request_id))snap.requests.push({id:event.data?.request_id,purpose:event.data?.purpose,created_at:event.created_at});
  }
  // Fold before pruning: a long request can outlive the display event window.
  // Muteki c60c8fee useConversation.ts:3017-3030 keeps the folded stream state
  // separate from its published event window. Keep Trace's one snapshot cache.
  if(snap.messages_page&&snap.events.length>700){
    const p=streamPreview(snap);snap.events.splice(0,snap.events.length-600);p.index=snap.events.length;
  }
  return true;
}
export function mergePreview(snapshot,current){
  if(snapshot.run.id!==current?.run?.id)return snapshot;
  const previous=streamPreview(current);
  if(snapshot.run.event_seq<current.run.event_seq){
    const known=new Set((snapshot.requests||[]).map(request=>request.id));snapshot.requests??=[];
    for(const request of current.requests||[])if(!known.has(request.id))snapshot.requests.push(request);
  }
  for(const event of current.events||[])if(event.sequence>(snapshot.run.event_seq||0))projectEvent(snapshot,event);
  // Workspace responses contain only a bounded event tail. Carry the already
  // observed same-request prefix, then fold newer committed events once.
  streamPreview(snapshot,previous);
  return snapshot;
}
// One mounted snapshot, not a second event store. Fold only appended events;
// retain text for the current request and output for unsettled actions only.
// Muteki useConversation.ts streamRef/flushStream and bounded stream cache.
let preview;
const emptyRecords=[];
const actionKey=(request,call)=>JSON.stringify([request||'',call]);
const outputOmission=bytes=>bytes>0?`\n[已省略 ${bytes} 字节；完整输出见原始产物]\n`:'';
function appendToolPreview(output,data){
  const key=data.stream==='stderr'?'stderr':'stdout',content=data.content||'';
  const bytes=new TextEncoder().encode(content),offset=data.offset,end=data.end_offset??offset+bytes.length,next=data.next_offset;
  if(!Number.isSafeInteger(offset)||!Number.isSafeInteger(end)||!Number.isSafeInteger(next)||offset<0||end<offset||next<end){
    output[key]=(output[key]+content).slice(-65536);if(output.ranges)delete output.ranges[key];return;
  }
  output.ranges??={};
  const range=output.ranges[key]??={text:output[key],end:0,next:0};
  range.next=Math.max(range.next,next);
  if(end>range.end){
    let start=offset,text=content;
    if(start<range.end){
      // Binary replacement text need not have a reversible byte mapping.
      // Leave an overlap omitted rather than display it twice as fresh output.
      if(bytes.length!==end-offset){output[key]=(range.text+outputOmission(range.next-range.end)).slice(-65536);return}
      let skip=range.end-offset;while(skip<bytes.length&&(bytes[skip]&0xc0)===0x80)skip++;
      start+=skip;text=new TextDecoder().decode(bytes.subarray(skip));
    }
    range.text=(range.text+outputOmission(start-range.end)+text).slice(-65536);range.end=end;
  }
  // The observed watermark can exceed the displayed prefix. Replace its
  // temporary omission marker when the final tail supplies more exact bytes.
  output[key]=(range.text+outputOmission(range.next-range.end)).slice(-65536);
}
export function streamPreview(snap,continuation){
  const events=snap.events||emptyRecords,requests=snap.requests||emptyRecords,actions=snap.actions||emptyRecords;
  const continuous=preview?.snap===snap&&preview.events===events&&preview.index<=events.length&&preview.last===events[preview.index-1];
  if(continuation||!preview||preview.snap!==snap||preview.events!==events||preview.requests!==requests||preview.requestCount!==requests.length||preview.actions!==actions||preview.actionCount!==actions.length||preview.index>events.length||preview.last!==events[preview.index-1]){
    const latest=requests.findLast(request=>!request.purpose||request.purpose==='conversation'),targets=new Map(),unique=new Map(),activeIDs=new Set(),settled=new Set();
    for(const action of actions)if(action.result&&action.request_id===latest?.id)settled.add(action.call_id);
    for(const action of actions)if(!action.result){targets.set(actionKey(action.request_id,action.call_id),{stdout:'',stderr:'',progress:undefined});activeIDs.add(action.call_id)}
    for(const action of actions)if(activeIDs.has(action.call_id))unique.set(action.call_id,unique.has(action.call_id)?null:action.request_id||'');
    const candidate=continuation||(continuous?preview:null),previous=candidate?.latest?.id===latest?.id?candidate:null;
    preview={snap,events,requests,requestCount:requests.length,actions,actionCount:actions.length,index:0,last:undefined,latest,current:'',owners:new Map(),unique,targets,activeIDs,settled,text:'',thinking:'',error:'',calls:new Map(),through:Math.max(previous?.through||0,previous?.last?.sequence||0),inheritedTargets:new Set()};
    // Paging replaces action metadata, not the already folded active stream.
    // Replaying a trimmed tail here would lose text/arguments and old owners.
    if(previous){
      for(const key of ['current','text','thinking','error','calls'])preview[key]=previous[key];
      for(const call of previous.calls.values())if(!settled.has(call.call_id)){const key=actionKey(call.request_id,call.call_id);if(!targets.has(key))targets.set(key,{stdout:'',stderr:'',progress:undefined});activeIDs.add(call.call_id)}
      for(const key of targets.keys())if(previous.targets.has(key)){targets.set(key,previous.targets.get(key));preview.inheritedTargets.add(key)}
      for(const [call,owner] of previous.owners)if(activeIDs.has(call))preview.owners.set(call,owner);
      if(!continuation&&[...targets.keys()].every(key=>preview.inheritedTargets.has(key))){preview.index=previous.index;preview.last=previous.last}
    }
  }
  const p=preview,replayOwners=new Map();let replayCurrent='';
  for(;p.index<events.length;p.index++){
    const event=events[p.index],data=event.data||{};
    const inherited=event.sequence<=p.through;
    if(event.type==='model_request'){if(inherited)replayCurrent=data.request_id||'';else p.current=data.request_id||''}
    // Ownership is needed only for calls whose output can still change.
    if(event.type==='action_started'&&p.activeIDs.has(data.call_id)){if(inherited)replayOwners.set(data.call_id,data.request_id||replayCurrent);else p.owners.set(data.call_id,data.request_id||p.current)}
    const owner=data.request_id||(inherited?replayOwners.get(data.call_id)||replayCurrent:p.owners.get(data.call_id)||p.current)||p.unique.get(data.call_id)||'';
    if(!inherited&&event.type==='model_error'&&(data.request_id||p.current)===p.latest?.id)p.error=data.error||event.summary||'模型请求失败';
    if(!inherited&&event.type==='model_stream'&&p.latest&&data.request_id===p.latest.id){
      const delta=data.delta||{};
      if(delta.kind==='text')p.text=(p.text+(delta.text||'')).slice(-65536);
      if(delta.kind==='thinking')p.thinking=(p.thinking+(delta.text||'')).slice(-65536);
      if(delta.kind==='tool_call'){
        const index=delta.index||0,call=p.calls.get(index)||{request_id:p.latest.id,call_id:p.latest.id+':'+index,name:'',arguments:'',status:'running'};
        if(delta.id)call.call_id=delta.id;
        call.name+=delta.name||'';call.arguments+=delta.arguments||'';p.calls.set(index,call);
        const key=actionKey(call.request_id,call.call_id);
        if(!p.settled.has(call.call_id)&&!p.targets.has(key)){p.targets.set(key,{stdout:'',stderr:'',progress:undefined});p.activeIDs.add(call.call_id)}
      }
    }
    if(['tool_output','tool_progress','mcp_progress'].includes(event.type)){
      for(const key of new Set([actionKey(owner,data.call_id),actionKey('',data.call_id)])){
        const output=p.targets.get(key);if(!output||event.sequence<=p.through&&p.inheritedTargets.has(key))continue;
        if(event.type==='tool_output')appendToolPreview(output,data);
        else output.progress=data;
      }
    }
  }
  for(const [call,owner] of replayOwners)if(!p.owners.has(call))p.owners.set(call,owner);
  p.last=events[p.index-1];
  return p;
}
export function streamedAction(snap,action){
  if(action.result)return action;
  const output=streamPreview(snap).targets.get(actionKey(action.request_id,action.call_id));
  return output?{...action,result:output.stdout||output.stderr?{stdout:output.stdout,stderr:output.stderr}:undefined,progress:output.progress}:action;
}
export function pendingModelCalls(snap,requestID){
  const preview=streamPreview(snap);
  if(requestID===preview.latest?.id)return [...preview.calls.values()];
  const calls=new Map();
  for(const event of snap.events||[]){
    if(event.type!=='model_stream'||event.data?.request_id!==requestID)continue;
    const delta=event.data.delta||{};if(delta.kind!=='tool_call')continue;
    const index=delta.index||0,call=calls.get(index)||{request_id:requestID,call_id:requestID+':'+index,name:'',arguments:'',status:'running'};
    if(delta.id)call.call_id=delta.id;
    call.name+=delta.name||'';call.arguments+=delta.arguments||'';calls.set(index,call);
  }
  return [...calls.values()];
}
