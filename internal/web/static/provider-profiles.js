import {esc} from './ui.js';

export function activeProvider(config){
 const name=config?.active_provider||'default';
 return {name,provider:name==='default'?config?.provider:config?.providers?.[name]};
}

export function providerEditorSelection(config,selection){
 if(selection!==undefined&&selection!==null)return String(selection);
 const active=config?.active_provider||'default';
 return active==='default'?'default':String(Object.keys(config.providers||{}).indexOf(active));
}

export function providerEditorName(config,selection,get=id=>document.getElementById(id)){
 const selected=providerEditorSelection(config,selection);
 return selected==='default'?'default':get('model-provider-'+selected+'-name')?.value.trim()||'';
}

export function providerProfiles(config,selection,drafts=0){
 const {name:active}=activeProvider(config),selected=providerEditorSelection(config,selection),profiles=[['default',config.provider||{}],...Object.entries(config.providers||{}),...Array.from({length:drafts},()=>['',{}])];
 return `<aside class="provider-profile-list" aria-label="模型配置"><div class="provider-list-heading"><h2>模型配置 · ${Object.keys(config.providers||{}).length+1}</h2><button type="button" class="link-button" data-action="reload-settings">重新读取</button></div><div class="provider-profile-items">${profiles.map(([name,p],i)=>{const key=i===0?'default':String(i-1),editing=selected===key;return `<article class="provider-profile-card" data-editing="${editing}" data-active="${name===active}"><button type="button" class="provider-profile-select" data-action="provider-profile-edit" data-editor="${key}" aria-pressed="${editing}"><span class="provider-profile-name">${esc(name||'新增配置')}${name===active?'<span class="provider-active-label">当前默认</span>':''}${!name?'<span class="provider-draft-label">未保存</span>':''}</span><span class="provider-profile-model">${esc(p.model||(name==='default'?'Mock · 本地验证':'填写模型名称'))}</span></button>${name?`<div class="provider-profile-actions"><span>${editing?'正在编辑':'已保存'}</span><button type="button" class="small" data-action="provider-profile-switch" data-provider="${esc(name)}" aria-pressed="${name===active}" ${name===active?'disabled':''}>${name===active?'使用中':'设为默认'}</button></div>`:''}</article>`}).join('')}</div><button type="button" class="provider-add" data-action="provider-profile-new">＋ 新增配置</button><p class="note">编辑不会切换默认模型。保存后可单独启用。</p></aside>`;
}

export function switchProviderProfile(config,name,form){
 if(name!=='default'&&!Object.hasOwn(config.providers||{},name))throw new Error('模型配置已变化，请重新读取后再选择。');
 if(!form)throw new Error('设置页面已变化，请重新打开模型配置。');
 const dirty=[...form.querySelectorAll('[data-config-base],[data-config-checked]')].some(el=>el.dataset.configChecked!==undefined?String(el.checked)!==el.dataset.configChecked:el.value!==el.dataset.configBase);
 if(dirty)throw new Error('请先保存或丢弃本页草稿，再切换默认模型配置。');
 return {...structuredClone(config),active_provider:name==='default'?'':name};
}
