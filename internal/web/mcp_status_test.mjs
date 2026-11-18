import assert from 'node:assert/strict';
import {createMCPStatus,mcpStatusBody} from './static/mcp-status.js';
import {mcpDetail,mcpForm} from './static/workforms.js';

function deferred(){let resolve,reject;const promise=new Promise((a,b)=>{resolve=a;reject=b});return {promise,resolve,reject}}
function harness(){
  const state={config:{mcp:[{name:'one',command:'fixture'}]},mcpStatuses:[]},element={dataset:{name:'one'},innerHTML:''},calls=[];
  const controller=createMCPStatus({getState:()=>state,getElement:()=>element,api:(...args)=>{const reply=deferred();calls.push({args,reply});return reply.promise}});
  return {state,element,calls,controller};
}
const connected={name:'one',enabled:true,connection:{state:'connected',protocol_version:'2025-06-18',capabilities:['resources','prompts']}};
{
  const h=harness(),opening=h.controller.open('one');assert.deepEqual(h.calls[0].args,['/mcp/status']);h.calls[0].reply.resolve([connected]);await opening;
  assert.match(h.element.innerHTML,/协议连接已响应/);assert.match(h.element.innerHTML,/resources \/ prompts/);
  const draft=JSON.stringify(h.state.config),pending=h.controller.start();assert.equal(h.calls[1].args[1],'POST');assert.deepEqual(h.calls[1].args[2],{name:'one'});
  h.controller.cancel();assert.equal(h.calls[1].args[3].signal.aborted,true);h.calls[1].reply.resolve({state:'connected',detail:'late success'});await pending;
  assert.match(h.element.innerHTML,/已取消/);assert.doesNotMatch(h.element.innerHTML,/late success/);assert.equal(JSON.stringify(h.state.config),draft);
  const again=h.controller.start();h.calls[2].reply.reject(Error('header-secret'));await again;assert.doesNotMatch(h.element.innerHTML,/header-secret/);
  const refresh=h.controller.refresh();h.calls[3].reply.resolve([{...connected,connection:{state:'closed'}}]);await refresh;assert.match(h.element.innerHTML,/连接已关闭/);assert.equal(JSON.stringify(h.state.config),draft);
}
for(const change of ['close','config','service']){
  const h=harness(),opening=h.controller.open('one');h.calls[0].reply.resolve([connected]);await opening;
  const pending=h.controller.start();if(change==='close')h.controller.close();if(change==='config')h.state.config=structuredClone(h.state.config);if(change==='service')h.element.dataset.name='two';
  h.calls[1].reply.resolve({state:'connected',detail:'stale reply'});await pending;assert.doesNotMatch(h.element.innerHTML,/stale reply/);
}
{
  const h=harness();h.state.config.mcp[0].disabled=true;const opening=h.controller.open('one');h.calls[0].reply.resolve([{name:'one',enabled:false,connection:{state:'disabled'}}]);await opening;await h.controller.start();assert.equal(h.calls.length,1);
}
const config={mcp:[{name:'one',command:'fixture'}]};
assert.match(mcpDetail(config,[],0,[connected]),/id="mcp-connection-status"/);
assert.match(mcpForm(config,[],[connected]),/1 个服务/);
assert.match(mcpStatusBody(connected),/启用配置不等于接口可用/);
{
  const html=mcpStatusBody({...connected,last_attempt:{state:'failed',stage:'initialize',code:'rpc',detail:'主连接失败',cleanup:[{stage:'cleanup',code:'http',detail:'清理响应异常 <fixture>'},{stage:'candidate_cleanup',code:'network',detail:'候选连接清理异常'}]}});
  assert.match(html,/initialize · rpc/);assert.match(html,/主连接失败/);assert.match(html,/临时连接清理失败：清理响应异常 &lt;fixture&gt;/);assert.match(html,/候选连接清理失败：候选连接清理异常/);assert.doesNotMatch(html,/<fixture>/);
}
console.log('MCP status: one service per row, negotiated capability display, cancel/stale protection, status-only refresh and draft retention.');
