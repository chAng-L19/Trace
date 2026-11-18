// Opt-in, bounded, credential-free diagnostics. Never changes event delivery,
// visibility policy, render cadence or the source-to-DOM acceptance threshold.
let enabled=false,observer;
const samples=[],limit=1000;
const now=()=>globalThis.performance?.now?.()??Date.now();
export function performanceSample(phase,started,extra={}){
 if(!enabled)return;
 const item={phase,at:now(),duration_ms:Math.max(0,now()-started),visibility:globalThis.document?.visibilityState||'unknown'};
 for(const key of ['bytes','sequence','count','first_semantic','gap_ms','supported','duration_ms','value','had_recent_input'])if(typeof extra[key]==='number'||typeof extra[key]==='boolean')item[key]=extra[key];
 samples.push(item);if(samples.length>limit)samples.splice(0,samples.length-limit);
}
export function measureUI(phase,fn){if(!enabled)return fn();const start=now();try{return fn()}finally{performanceSample(phase,start)}}
export function performanceDiagnostics(value){
 if(typeof value==='boolean'){enabled=value;observer?.disconnect();observer=null;if(enabled&&typeof PerformanceObserver!=='undefined'){observer=new PerformanceObserver(list=>{for(const entry of list.getEntries())performanceSample(entry.entryType,entry.startTime,{supported:true,duration_ms:entry.duration,value:entry.value,had_recent_input:entry.hadRecentInput})});for(const type of ['longtask','layout-shift'])try{observer.observe({type,buffered:false})}catch{}}}
 return {enabled,limit,samples:samples.map(row=>({...row})),longtask_supported:typeof PerformanceObserver!=='undefined'&&PerformanceObserver.supportedEntryTypes?.includes('longtask')===true,layout_shift_supported:typeof PerformanceObserver!=='undefined'&&PerformanceObserver.supportedEntryTypes?.includes('layout-shift')===true};
}
export const uiNow=now;
