import {chainDetail,chainComposeForm,chainReferenceForm,chainMethodFields} from './evidence-chains.js';

export function createChainEvents({state,api,showDetail,render,refresh,notice,beginDetail,isCurrent,captureNotice=()=>notice}){
  let revision=0,loading=null;
  const scope=()=>state.page==='history'?state.selected||'':'';
  async function load(invalidate=false,before=''){
    const run=scope();
    if(loading?.run===run){if(invalidate){loading.dirty=true;loading.before=before}return loading.work}
    const request=++revision,flight={run,dirty:true,before};loading=flight;
    flight.work=(async()=>{
      while(flight.dirty){
        flight.dirty=false;
        try{const params=new URLSearchParams({limit:'40'});if(run)params.set('run',run);if(state.chainQuery)params.set('query',state.chainQuery);if(flight.before)params.set('before',flight.before);const page=await api('/evidence-chains?'+params);if(flight.dirty)continue;if(request===revision&&run===scope()){state.chains=Array.isArray(page)?page:page.items||[];state.chainsPage=Array.isArray(page)?null:page;state.chainsScope=run;state.chainsError=''}}
        catch(error){if(flight.dirty)continue;if(request===revision&&run===scope())state.chainsError=error.message}
      }
    })().finally(()=>{if(loading===flight)loading=null});return flight.work;
  }
  async function records(){
    const run=scope();return api('/evidence'+(run?'?run='+encodeURIComponent(run):''));
  }
  async function refreshAfterChange(){await refresh()}
  async function action(el){const notice=captureNotice();
    const key=el.dataset.action;
    if(!['chain-refresh','chain-page','catalog-mode','chain-compose','chain-detail','chain-reference','chain-export','chain-delete','chain-unreference','chain-evolve'].includes(key))return false;
    if(key==='catalog-mode'){state.catalogMode=el.dataset.mode;if(state.catalogMode==='records'&&state.page==='history'){render();return true}if(state.catalogMode==='records'){const run=scope(),entries=await records();if(run!==scope())return true;state.catalog=entries;state.catalogScope=run;state.catalogError=''}else await load();render();return true}
    if(key==='chain-refresh'||key==='chain-page'){await load(true,key==='chain-page'?el.dataset.before||'':'');render();return true}
    if(key==='chain-unreference'){
      await api('/runs/'+encodeURIComponent(el.dataset.target)+'/evidence-chain-references','DELETE',{source_run_id:el.dataset.run,chain_id:el.dataset.id,chain_hash:el.dataset.hash});
      await refreshAfterChange();notice('整链引用已取消，来源操作保留');return true;
    }
    const token=beginDetail();
    if(key==='chain-compose'){
      const [entries,methods]=await Promise.all([records(),api('/evidence-chains?limit=40')]);
      if(!isCurrent(token))return true;
      const source=state.page==='history'?entries.filter(e=>e.run_id===state.selected):entries;
      showDetail('整理操作证据链',chainComposeForm(source,state.runs,state.catalogSelected));document.querySelector('#chain-compose-form').insertAdjacentHTML('afterbegin',chainMethodFields(null,Array.isArray(methods)?methods:methods.items||[]));return true;
    }
    const entry=await api('/evidence-chains/'+encodeURIComponent(el.dataset.run)+'/'+encodeURIComponent(el.dataset.id)+(el.dataset.hash?'?hash='+encodeURIComponent(el.dataset.hash):''));
    if(!isCurrent(token))return true;
    if(el.dataset.hash&&el.dataset.hash!==entry.chain.content_hash)throw new Error('链路与固定引用不一致，请核对来源；原引用仍保留');
    if(key==='chain-evolve'){
      const [entries,methods]=await Promise.all([api('/evidence'),api('/evidence-chains?limit=40')]);if(!isCurrent(token))return true;
      showDetail('吸收经验 / 新建方法变体',chainComposeForm(entries,state.runs,entry.chain.steps.map(step=>step.run_id+'/'+step.evidence_id),entry.chain.steps));
      const form=document.querySelector('#chain-compose-form');form.insertAdjacentHTML('afterbegin',chainMethodFields(entry,Array.isArray(methods)?methods:methods.items||[]));form.querySelector('#chain-owner').value=entry.run_id;form.querySelector('#chain-name').value=entry.chain.name;form.querySelector('#chain-summary').value=entry.chain.summary;return true;
    }
    if(key==='chain-delete'){showDetail('移除证据链',`<form id="chain-delete-form" data-run="${entry.run_id}" data-chain="${entry.chain.id}" class="field-stack"><p>移除此链路组织记录？原始操作和产物保留，其他会话引用中的链路会阻止移除。</p><button type="submit" class="danger">确认移除证据链</button></form>`);return true}
    if(key==='chain-detail')showDetail('操作证据链',chainDetail(entry));
    if(key==='chain-reference'){showDetail('引用整条证据链',chainReferenceForm(entry,state.runs));document.querySelector('#chain-reference-form').insertAdjacentHTML('afterbegin','<label>支撑的任务或结论<textarea id="chain-purpose" placeholder="说明这条链路在目标会话中的用途"></textarea></label>')}
    if(key==='chain-export'){
      const url=URL.createObjectURL(new Blob([JSON.stringify(entry,null,2)],{type:'application/json'})),a=document.createElement('a');
      a.href=url;a.download=entry.chain.id+'.json';a.click();setTimeout(()=>URL.revokeObjectURL(url),1000);
    }
    return true;
  }
  async function submit(form){const notice=captureNotice();
    if(form.id==='chain-delete-form'){
      const token=beginDetail();await api('/evidence-chains/'+encodeURIComponent(form.dataset.run)+'/'+encodeURIComponent(form.dataset.chain),'DELETE',{});await refreshAfterChange();if(isCurrent(token))showDetail('链路已移除','原始操作记录与产物保留。');return true;
    }
    if(form.id==='chain-compose-form'){
      const token=beginDetail();
      const entries=[...form.querySelectorAll('input[name="chain-step"]:checked')].map(input=>{
        const row=input.closest('li'),[run_id,evidence_id]=input.value.split('/');
        return {run_id,evidence_id,content_hash:input.dataset.hash,label:row.querySelector('[name="chain-label"]').value,order:Number(row.querySelector('[name="chain-order"]').value)};
      });
      if(!entries.length)throw new Error('请选择组成链路的实际操作');
      const steps=entries.sort((a,b)=>a.order-b.order).map(({order,...step})=>step);
      const run=form.querySelector('#chain-owner').value;
      const parents=[...JSON.parse(form.querySelector('#chain-parents').value),...Array.from(form.querySelectorAll('#chain-other-parents option:checked'),option=>JSON.parse(option.value))];
      const entry=await api('/runs/'+encodeURIComponent(run)+'/evidence-chains','POST',{name:form.querySelector('#chain-name').value,summary:form.querySelector('#chain-summary').value,steps,method_id:form.querySelector('#chain-method').value,variant:form.querySelector('#chain-variant').value,applicability:form.querySelector('#chain-applicability').value,lesson:form.querySelector('#chain-lesson').value,derived_from:parents});
      state.catalogMode='chains';await refreshAfterChange();if(isCurrent(token)){showDetail('操作证据链',chainDetail(entry));notice('整条证据链已保存')}return true;
    }
    if(form.id==='chain-reference-form'){
      const token=beginDetail();
      await api('/runs/'+encodeURIComponent(form.querySelector('#chain-target').value)+'/evidence-chain-references','POST',{source_run_id:form.dataset.run,chain_id:form.dataset.chain,chain_hash:form.dataset.hash,purpose:form.querySelector('#chain-purpose').value});
      await refreshAfterChange();const entry=await api('/evidence-chains/'+encodeURIComponent(form.dataset.run)+'/'+encodeURIComponent(form.dataset.chain)+'?hash='+encodeURIComponent(form.dataset.hash));if(isCurrent(token)){showDetail('操作证据链',chainDetail(entry));notice('整条链路已引用，操作未重新执行')}return true;
    }
    return false;
  }
  return {load,action,submit};
}
