// A separate read-only projection: never block initial workspace/SSE reads and
// never scan a tree for token preview events. A stale navigation cannot paint.
export function createWorkerCapacity({api,getRun,getElement,schedule=setTimeout,cancel=clearTimeout}){
 let selected='',flight=null,controller=null,dirty=false,value=null,timer,epoch=0,foregrounds=0;
 const paint=()=>{
  const el=getElement();if(!el)return;
  el.textContent=value?`整树 Worker ${value.active} / ${value.capacity}`:'整树 Worker · 待读取';
  el.title=value?`根会话 ${value.root_run_id}；未确认清理仍占用名额`:'整树并发状态独立读取';
 };
 function reset(){epoch++;foregrounds=0;cancel(timer);controller?.abort();controller=null;selected=getRun();flight=null;dirty=false;value=null;paint()}
 async function foreground(read){
  const run=getRun();if(run!==selected)reset();if(!run)return read();
  const token=epoch;foregrounds++;cancel(timer);controller?.abort();controller=null;flight=null;dirty=true;
  // Preserve the last read-only value while interactive workspace/history
  // reads release the server's cold-display gate from a background tree scan.
  try{return await read()}
  finally{if(token===epoch&&--foregrounds===0&&dirty)timer=schedule(refresh,0)}
 }
 function refresh(){
  const run=getRun();if(run!==selected)reset();if(!run)return Promise.resolve();
  cancel(timer);dirty=true;if(foregrounds)return Promise.resolve();if(flight)return flight;
  const owner=selected,abort=controller=new AbortController();
  const work=(async()=>{
   while(dirty&&selected===owner&&!abort.signal.aborted){
    dirty=false;
    try{
     const next=await api('/runs/'+encodeURIComponent(owner)+'/worker-capacity','GET',undefined,{signal:abort.signal});
     if(selected!==owner||getRun()!==owner||abort.signal.aborted)continue;
     value=next;paint();
    }catch(error){if(!abort.signal.aborted&&getRun()===owner){value=null;paint();const el=getElement();if(el)el.title='整树状态读取失败，请刷新重试'}}
   }
  })().finally(()=>{if(flight===work){flight=null;controller=null;if(value?.active>0&&getRun()===owner)timer=schedule(refresh,5000)}});
  flight=work;return work;
 }
 return {refresh,reset,paint,foreground};
}
