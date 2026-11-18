import assert from 'node:assert/strict';
import {attachmentRecords,attachmentPicker,taskInputPicker,chooseAttachments,uploadAttachments,clearAttachments} from './static/attachments.js';

const input={id:'input-id',name:'<sample>.png',hash:'a'.repeat(64),mime_type:'image/png',bytes:64,source_run_id:'parent',source_message_id:'m1',delivery:'native_image'};
const rendered=attachmentRecords([input]);
for(const text of ['&lt;sample&gt;.png','用户输入','原生图像','parent','m1',input.hash,'?preview=1'])assert.ok(rendered.includes(text),text);
assert.ok(!rendered.includes('<sample>'));
assert.match(attachmentPicker(true),/type="file"[^>]+disabled/);
assert.match(taskInputPicker([input,input]),/data-task-input="input-id"/);
assert.equal(taskInputPicker([input,input]).match(/data-task-input=/g).length,1);

globalThis.document={querySelector:()=>null};
const state={selected:'run1'},form={id:'message-form'};
const file=new Blob(['original'],{type:'text/plain'});Object.defineProperty(file,'name',{value:'original.txt'});
chooseAttachments(state,{form,files:[file],value:''});
let uploads=0;
globalThis.fetch=async(url,options)=>{uploads++;assert.equal(url,'/api/inputs');assert.ok(options.body instanceof FormData);return{ok:true,text:async()=>JSON.stringify({id:'uploaded'})}};
assert.deepEqual(await uploadAttachments(state,form),['uploaded']);
// Failed message delivery may retry without uploading the same bytes again.
assert.deepEqual(await uploadAttachments(state,form),['uploaded']);assert.equal(uploads,1);
assert.deepEqual(await uploadAttachments({selected:'another-run'},form),[]);
clearAttachments(state,form);assert.deepEqual(await uploadAttachments(state,form),[]);
chooseAttachments(state,{form,files:[file],value:''});
const submitted=await uploadAttachments(state,form);
const next=new Blob(['new']);Object.defineProperty(next,'name',{value:'new.txt'});
chooseAttachments(state,{form,files:[next],value:''});
clearAttachments(state,form,submitted);assert.equal((await uploadAttachments(state,form)).length,1,'late ACK preserves newly selected file');
console.log('Input attachments: escaped preview, scoped draft, upload identity and retry checks passed.');
