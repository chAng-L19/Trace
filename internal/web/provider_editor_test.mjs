import assert from 'node:assert/strict';
import {settings} from './static/settings.js';
import {addModelRow,resetModelRows} from './static/model-settings.js';
import {providerEditorSelection,providerEditorName} from './static/provider-profiles.js';
import {createProviderProbe} from './static/provider-probe.js';

const config={revision:'r1',active_provider:'fast',provider:{url:'https://default.invalid',model:'base',api_key:'[stored secret]'},providers:{fast:{url:'https://fast.invalid',model:'fast-model',api_key:'[stored secret]'},other:{url:'https://other.invalid',model:'other-model',api_key:'[stored secret]'}},context:{}};
const state={page:'settings',settingTab:'provider',config};
resetModelRows();
const html=settings(state);
assert.equal(providerEditorSelection(config),'0');
assert.match(html,/data-profile-panel="default" hidden/);
assert.match(html,/data-profile-panel="0" >/);
assert.match(html,/data-profile-panel="1" hidden/);
assert.equal((html.match(/id="provider-probe-status"/g)||[]).length,1);
assert.match(html,/id="provider-save-apply"[^>]*data-config-activate="true"/);
assert.match(html,/class="provider-save-bar"><p data-form-feedback="true" role="status"/);
assert.doesNotMatch(html,/<details[^>]+(?:provider-additional-config|model-role-forms|model-price-forms)[^>]*\bopen\b/);
assert.equal((html.match(/data-model-provider=/g)||[]).length,2,'no empty provider form until Add');
assert.equal(addModelRow('providers'),0);
assert.equal(addModelRow('providers'),1);
assert.equal((settings({...state,providerEditor:'3'}).match(/data-model-provider=/g)||[]).length,4);
resetModelRows();

const elements={};
const field=(id,value,base=value)=>elements[id]={value,dataset:{configBase:base}};
field('provider-editor-selection','0');
for(const [index,name,p] of [['default','default',config.provider],...Object.entries(config.providers).map(([name,p],i)=>[String(i),name,p])]){
 const prefix=index==='default'?'provider':'model-provider-'+index;
 if(index!=='default')field(prefix+'-name',name);
 for(const [id,key] of [['url','url'],['model','model'],['key','api_key']])field(prefix+'-'+id,p[key]);
}
for(const id of ['provider-probe-status','provider-probe-button','provider-probe-cancel','provider-probe-panel','provider-probe-time'])elements[id]={dataset:{}};
const calls=[],events=[];
const probe=createProviderProbe({getState:()=>state,getElement:id=>elements[id],onChange:value=>events.push(value),api:(...args)=>new Promise(resolve=>calls.push({args,resolve}))});
const done=async(work,index,status='success')=>{calls[index].resolve({status,detail:'fixture response',elapsed_ms:7,checked_at:'2026-11-17T00:00:00Z'});await work};
let work=probe.start();
assert.equal(calls[0].args[2].provider,'fast');
assert.equal(calls[0].args[2].api_key,'[stored secret]');
await done(work,0);
assert.equal(elements['provider-probe-panel'].dataset.status,'success');
assert.match(elements['provider-probe-status'].textContent,/fast.*检测通过.*7 ms/);
assert.equal(events.at(-1).activeResult.status,'success');

// Editing a different profile keeps the active connection's result separate.
elements['provider-editor-selection'].value='1';probe.sync();
assert.equal(events.at(-1).provider,'other');assert.equal(events.at(-1).status,'unprobed');assert.equal(events.at(-1).activeResult.status,'success');
work=probe.start();elements['provider-editor-selection'].value='0';probe.sync();
assert.equal(calls[1].args[3].signal.aborted,true);await done(work,1);
assert.match(elements['provider-probe-status'].textContent,/fast.*检测通过/);
elements['model-provider-0-key'].value='';work=probe.start();
assert.equal(calls[2].args[2].api_key,'','cleared key does not become stored-secret sentinel');await done(work,2);
assert.equal(events.at(-1).matchesSavedConfig,false);
const submitted=structuredClone(config);submitted.providers.fast.api_key='';
const stored={...submitted,revision:'r2'};probe.saved(submitted,stored);state.config=stored;probe.sync();
assert.equal(events.at(-1).status,'success');assert.equal(events.at(-1).matchesSavedConfig,true);
state.config={...stored,revision:'r3'};probe.sync();assert.equal(events.at(-1).status,'unprobed');

// Rename never resolves a placeholder against default or another named secret.
elements['model-provider-0-name'].value='default';elements['model-provider-0-key'].value='[stored secret]';work=probe.start();
assert.equal(calls[3].args[2].provider,'fast');await done(work,3);
assert.equal(events.at(-1).matchesSavedConfig,false);
elements['model-provider-0-name'].dataset.configBase='';elements['model-provider-0-name'].value='new';
await probe.start();assert.equal(calls.length,4);assert.equal(events.at(-1).status,'failed');
assert.equal(providerEditorName(config,'0',id=>elements[id]),'new');
elements['model-provider-0-key'].value='new-key';work=probe.start();await done(work,4);
const newSubmitted=structuredClone(state.config);newSubmitted.providers.new={url:elements['model-provider-0-url'].value,model:elements['model-provider-0-model'].value,api_key:'new-key'};
const newStored=structuredClone(newSubmitted);newStored.revision='r4';newStored.providers.new.api_key='[stored secret]';
probe.saved(newSubmitted,newStored);state.config=newStored;elements['model-provider-0-name'].dataset.configBase='new';elements['model-provider-0-key'].value='[stored secret]';probe.sync();
assert.equal(events.at(-1).status,'success','new-profile result follows exact submitted credentials through redaction');
assert.equal(events.at(-1).matchesSavedConfig,true);
assert.doesNotMatch(JSON.stringify(events),/stored secret|api_key|https:/);
console.log('Provider editor: contextual layout, explicit additions, named/active separation, cancellation, empty key, save revision and rename credential ownership passed.');
