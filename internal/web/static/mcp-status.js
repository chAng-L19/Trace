import {esc,btn} from './ui.js';

export const mcpStateLabel=state=>({connected:'协议连接已响应',failed:'连接失败',cancelled:'已取消',closed:'连接已关闭',disabled:'已禁用',not_connected:'尚无连接记录',probing:'检测中…'})[state]||'尚无连接记录';
export function mcpStatusBody(row={},busy=false){
  const item=row.connection||{},attempt=row.last_attempt;
  const details=s=>`<p>${esc(mcpStateLabel(s.state))}${s.stage?' · '+esc(s.stage):''}${s.code?' · '+esc(s.code):''}</p>${s.detail?`<p class="note">${esc(s.detail)}</p>`:''}${(s.cleanup||[]).map(c=>`<p class="inline-error">${c.stage==='candidate_cleanup'?'候选连接清理失败':'临时连接清理失败'}：${esc(c.detail)}${c.code?' · '+esc(c.code):''}</p>`).join('')}<dl class="service-info"><dt>协议版本</dt><dd>${esc(s.protocol_version||'尚未协商')}</dd><dt>协商能力</dt><dd>${esc((s.capabilities||[]).join(' / ')||'未声明可展示能力')}</dd><dt>观察时间</dt><dd>${esc(s.checked_at&&!s.checked_at.startsWith('0001')?s.checked_at:'尚无记录')}</dd></dl>`;
  return `<h3>当前 Agent 连接</h3>${details(item)}${attempt?`<h3>最近检测 / 配置尝试</h3>${details(attempt)}<p class="note">${attempt.stage==='cleanup'?'临时连接清理失败，请检查服务端会话。':'临时检测结果仅记录本次协议观察。'}此结果不替换当前 Agent 或工具目录。</p>`:''}<p class="note">检测不会启用或停用服务。启用配置不等于接口可用；连接状态是最近一次协议观察，不是持续健康保证。</p><div class="buttons">${btn('mcp-status-refresh','刷新状态',busy?'disabled':'')}${btn('mcp-probe',busy?'检测中…':'检测已保存连接',busy||row.enabled===false?'disabled':'')}${busy?btn('mcp-probe-cancel','取消检测'):''}</div>`;
}

// Only this panel is repainted. Configuration drafts, search and page scroll stay intact.
export function createMCPStatus({api,getState,getElement}){
  let current='',revision=0,controller=null,busy=false,config=null;
  const element=()=>getElement('mcp-connection-status');
  const valid=()=>element()?.dataset.name===current&&getState().config===config;
  const row=()=>getState().mcpStatuses?.find(item=>item.name===current)||{enabled:!getState().config?.mcp?.find(item=>item.name===current)?.disabled};
  function paint(){if(valid())element().innerHTML=mcpStatusBody(row(),busy)}
  function cancel(){revision++;controller?.abort();controller=null;busy=false;paint()}
  async function refresh(){const rev=++revision,name=current,owner=config;try{const rows=await api('/mcp/status');if(rev!==revision||name!==current||owner!==getState().config)return;getState().mcpStatuses=rows;paint()}catch{if(rev===revision&&valid())element().innerHTML='<p class="inline-error">状态读取失败，请重试。</p>'+btn('mcp-status-refresh','刷新状态')}}
  async function open(name){cancel();current=name;config=getState().config;paint();await refresh()}
  async function start(){
    if(busy||!valid()||row().enabled===false)return;
    const rev=++revision,name=current,owner=config;controller=new AbortController();busy=true;paint();
    try{const result=await api('/mcp/probe','POST',{name},{signal:controller.signal});if(rev!==revision||owner!==getState().config||!valid())return;const rows=getState().mcpStatuses||[],old=rows.find(item=>item.name===name)||row();getState().mcpStatuses=[...rows.filter(item=>item.name!==name),{...old,name,last_attempt:result}]}catch{if(rev===revision&&valid()){const rows=getState().mcpStatuses||[],old=rows.find(item=>item.name===name)||row();getState().mcpStatuses=[...rows.filter(item=>item.name!==name),{...old,name,last_attempt:{state:'failed',detail:'连接检测请求失败，请重试'}}]}}finally{if(rev===revision){busy=false;controller=null;paint()}}
  }
  function cancelProbe(){const name=current;cancel();if(valid()){const rows=getState().mcpStatuses||[],old=rows.find(item=>item.name===name)||row();getState().mcpStatuses=[...rows.filter(item=>item.name!==name),{...old,name,last_attempt:{state:'cancelled',detail:'连接检测已取消'}}];paint()}}
  return {open,refresh,start,cancel:cancelProbe,close:cancel};
}
