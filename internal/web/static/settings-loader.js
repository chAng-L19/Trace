// Reference: Muteki SettingsContent / AgentExtensionsSettings, 144df7666dc7.
// Adopt per-page loading and stale-response ownership; keep Trace's native modules.
const paths={config:'/config',tools:'/tools',skills:'/skills',mcpStatuses:'/mcp/status'};
export const settingsFields=tab=>tab==='tools'?['config','tools','mcpStatuses']:tab==='local'?['config','tools']:tab==='skills'?['config','skills']:['config'];
export function createSettingsLoader({api,state}){
  const resources=Object.fromEntries(Object.keys(paths).map(field=>[field,{loaded:field==='config'&&!!state.config,available:field==='config'&&!!state.config,loading:false,error:''}]));
  const flights=new Map(),versions=Object.fromEntries(Object.keys(paths).map(field=>[field,0]));
  state.settingsResources=resources;
  const report=()=>{state.settingsError=settingsFields(state.settingTab).map(field=>resources[field].error).filter(Boolean).join('；')};
  function invalidate(fields){
    for(const field of fields){versions[field]++;resources[field].loaded=false;resources[field].error=''}
    report();
  }
  function read(field){
    const resource=resources[field];
    if(resource.loaded)return Promise.resolve();
    if(flights.has(field))return flights.get(field);
    resource.loading=true;resource.error='';
    const request=(async()=>{
      while(!resource.loaded){
        const version=versions[field];
        try{
          const value=await api(paths[field]);
          if(version!==versions[field])continue;
          state[field]=value||(field==='config'?null:[]);resource.loaded=true;resource.available=true;resource.error='';
        }catch(error){
          if(version!==versions[field])continue;
          resource.error=`${field}：${error.message}`;break;
        }
      }
    })().finally(()=>{resource.loading=false;flights.delete(field);report()});
    flights.set(field,request);return request;
  }
  async function load(tab='provider',reload=false){
    const fields=settingsFields(tab);if(reload)invalidate(fields);
    await Promise.all(fields.map(read));report();
  }
  function saved(config,{skills=false,modelOnly=false}={}){
    const previous=state.config;
    skills=skills||['skill_paths','disabled_skills'].some(field=>JSON.stringify(previous?.[field]||[])!==JSON.stringify(config[field]||[]));
    // Model-only saves retain the existing tool connections and catalog.
    invalidate(modelOnly?['config']:['config','tools','mcpStatuses',...(skills?['skills']:[])]);
    // GET /config enriches source links that the save response does not contain.
    state.config=config;resources.config.available=true;report();
  }
  return {load,invalidate,saved};
}
