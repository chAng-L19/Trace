import {captureScroll,restoreScroll} from './interactions.js';
export function createConversationHistory({state,api,render}){
  let epoch=0,controller,pending;
  const anchor=()=>{const pane=document.querySelector('.conversation');return pane?captureScroll(pane):{top:0,left:0,follow:false}};
  const restore=saved=>{const pane=document.querySelector('.conversation');if(pane)restoreScroll(pane,saved,{follow:false})};
  function cancel(){epoch++;controller?.abort();controller=null;pending=null;state.messagesLoading=false}
  function refresh(next,previous){
    const pane=document.querySelector('.conversation');
    if(next.run.id===previous?.run?.id&&previous.messages_page&&pane&&pane.scrollHeight-pane.scrollTop-pane.clientHeight>45){previous.historyWindow=true;state.messageCursor={around:previous.messages[Math.floor(previous.messages.length/2)]?.id}}
    if(next.run.id!==previous?.run?.id||!previous.historyWindow)return next;
    next.messages=previous.messages;next.historyWindow=true;
    next.messages_page={...previous.messages_page,total:next.messages_page?.total??previous.messages_page.total,has_more_after:previous.messages_page.end<(next.messages_page?.total??previous.messages_page.total)};
    // The page's original call output stays intact while new status/metadata
    // comes from the latest authoritative generation.
    const old=new Map((previous.actions||[]).map(a=>[JSON.stringify([a.request_id,a.call_id]),a]));
    next.actions=(next.actions||[]).map(a=>{const prior=old.get(JSON.stringify([a.request_id,a.call_id]));return prior?.result&&a.result?{...a,result:{...a.result,stdout:prior.result.stdout,stderr:prior.result.stderr}}:a});
    // The activity projection contains only its latest window. Keep the bounded
    // reader window's associated calls/requests when that window is frozen.
    const requests=new Set(previous.messages.map(m=>m.request_id).filter(Boolean));
    const keys=new Set(next.actions.map(a=>JSON.stringify([a.request_id,a.call_id])));
    for(const [key,action] of old)if(!keys.has(key)&&requests.has(action.request_id))next.actions.push(action);
    for(const field of ['requests','responses']){
      const key=row=>row.request_id||row.id,seen=new Set((next[field]||[]).map(key));
      next[field]=[...(next[field]||[]),...(previous[field]||[]).filter(row=>requests.has(key(row))&&!seen.has(key(row)))];
    }
    return next;
  }
  function load(query={},position='preserve'){
    const id=state.selected,key=id+':'+JSON.stringify(query);
    if(pending?.key===key)return pending.promise;
    cancel();const token=epoch,abort=controller=new AbortController(),saved=anchor();
    state.messagesLoading=true;state.messagesError='';render();
    const params=new URLSearchParams({limit:'40',...query});
    const promise=(async()=>{try{
      const page=await api(`/runs/${encodeURIComponent(id)}/messages?${params}`,'GET',undefined,{signal:abort.signal});
      if(abort.signal.aborted||token!==epoch||id!==state.selected||!state.historyDetail)return false;
      // Do not roll the SSE waterline backwards when a page request overlaps
      // a committed stream frame or a semantic workspace refresh.
      const current=state.snap;
      if(current?.run.id!==id)return false;
      const liveActions=new Map((current.actions||[]).map(a=>[JSON.stringify([a.request_id,a.call_id]),a]));
      current.messages=page.messages;current.messages_page=page.messages_page;
      current.historyWindow=Object.keys(query).length>0&&!!page.messages_page.has_more_after;
      state.messageCursor=current.historyWindow?{around:page.messages[Math.floor(page.messages.length/2)]?.id}:null;
      const pageKeys=new Set();current.actions=(page.actions||[]).map(a=>{const key=JSON.stringify([a.request_id,a.call_id]),live=liveActions.get(key);pageKeys.add(key);if(!live||current.run.event_seq<=page.run.event_seq)return a;if(!a.result&&live.result)return live;return {...a,...live,result:a.result&&live.result?{...live.result,stdout:a.result.stdout,stderr:a.result.stderr}:live.result}});
      for(const [key,action] of liveActions)if(!pageKeys.has(key)&&['started','running','waiting_tool'].includes(action.status))current.actions.push(action);
      const activeRequests=new Set(current.actions.map(a=>a.request_id).filter(Boolean));
      for(const field of ['requests','responses']){
        const key=row=>row.request_id||row.id,rows=new Map((page[field]||[]).map(row=>[key(row),row]));
        for(const row of current[field]||[])if((rows.has(key(row))||activeRequests.has(key(row)))&&(!rows.has(key(row))||current.run.event_seq>page.run.event_seq))rows.set(key(row),row);
        current[field]=[...rows.values()];
      }
      state.messagesLoading=false;render();
      const pane=document.querySelector('.conversation');if(pane){if(position==='start')pane.scrollTop=0;else if(position==='end')pane.scrollTop=pane.scrollHeight;else restore(saved)}
      return true;
    }catch(error){if(!abort.signal.aborted&&token===epoch&&id===state.selected){state.messagesError=error.message;state.messagesLoading=false;render();throw error}return false}
    finally{if(token===epoch){pending=null;controller=null}}})();
    pending={key,promise};return promise;
  }
  return {cancel,refresh,load};
}
