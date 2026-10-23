/* Static demo transport. This file is never imported by production builds. */
(() => {
  'use strict';
  const seed = window.FOLIO_DEMO_SEED;
  if (!seed) throw new Error('Folio demo seed is missing');
  const nativeFetch = window.fetch.bind(window);
  const storageKey = 'folio.demo.data.v1';
  const authKey = 'folio.demo.signed-in';
  const clone = value => JSON.parse(JSON.stringify(value));
  const now = () => new Date().toISOString();
  const id = prefix => prefix + '-' + crypto.randomUUID().slice(0, 8);
  const en = () => localStorage.getItem('folio.language') === 'en';
  const text = (zh, english) => en() ? english : zh;
  const streams = new Set();
  const exp = () => new Date(Date.now() + 3600000).toISOString();
  let authenticated = localStorage.getItem(authKey) === 'yes';
  let store;
  const fresh = () => ({ version: 1, sites: clone(seed.sites), instances: clone(seed.instances), apps: clone(seed.apps), dbs: clone(seed.dbs), caches: clone(seed.caches), files: clone(seed.file_rows), contents: clone(seed.contents), notes: clone(seed.notes), fixtures: clone(seed.fixture_actions), tasks: [], audits: [], histories: {}, tokens: [], rules: [], policies: {}, plans: {}, uploads: {}, terminals: {}, sessions: [], settings: {panel_name:'Folio · 折纸面板',footer:'Folio · 折纸面板',login_entry:'',captcha_enabled:false,api_enabled:true,maintenance:false,revision:1} });
  try { store = JSON.parse(localStorage.getItem(storageKey) || 'null'); } catch {}
  if (!store || store.version !== 1) store = fresh();
  function save() { try { localStorage.setItem(storageKey, JSON.stringify(store)); } catch { toast(text('演示数据只保存在本次页面中；浏览器存储已满或被禁用。','Demo changes are in memory only; browser storage is unavailable.')); } }
  function emit() { save(); for (const s of streams) s.onmessage?.(new MessageEvent('message', {data:'{"demo":true}'})); window.dispatchEvent(new Event('powerlinux:invalidate')); }
  function fail(message, status = 400, code = 'demo_error') { const error = new Error(message); error.status = status; error.code = code; throw error; }
  const unknown = name => fail(text('此高级操作尚未模拟。可以继续查看表单或体验其他模块。','This advanced action is not simulated. You can inspect its form or try another module.'), 501, 'demo_not_supported');
  function pick(rows, key) { return rows.find(row => row.id === key || row.database === key) ?? fail(text('演示资源不存在，请返回列表刷新。','Demo resource not found. Return to the list and refresh.'), 404, 'not_found'); }
  function check(row, p) { if (p.expected_revision !== undefined && row.revision !== undefined && Number(p.expected_revision) !== row.revision) fail(text('演示数据已变化，请刷新后重试。','Demo data changed. Refresh and retry.'), 409, 'revision_conflict'); }
  function safe(p) { return Object.fromEntries(Object.entries(p).filter(([k]) => !/password|token|secret|credential|private_key|sql|query/i.test(k))); }
  function audit(action, target) { store.audits.unshift({id:id('audit'), action, actor:seed.actor, created_at:now(), success:true, status:'succeeded', target, summary:text('演示操作：','Demo action: ') + action}); store.audits = store.audits.slice(0,100); }
  function task(action, target, apply = () => {}, result = {}) {
    const t = {id:id('task'),action,operation:action,name:({sites_create:'创建网站',software_apply:'应用软件计划',fs_upload_commit:'保存上传文件',browser_app_change:'修改浏览器组件',software_service_control:'管理软件服务'})[action]||action,target,resource:target,actor:seed.actor,status:'queued',state:'queued',progress:0,created_at:now(),updated_at:now(),result};
    store.tasks.unshift(t); audit(action,target); emit();
    setTimeout(() => {if(t.status !== 'queued')return;t.status=t.state='running';t.progress=45;t.updated_at=now();emit();},450);
    setTimeout(() => {if(t.status !== 'running')return;try{apply();t.status=t.state='succeeded';t.progress=100;}catch(e){t.status=t.state='failed';t.error=e.message;}t.updated_at=now();emit();},1400);
    return {task_id:t.id,id:t.id,status:'queued'};
  }
  function page(rows, p = {}) {
    let items = rows.filter(row => !p.query || JSON.stringify([row.name,row.id,row.domains,row.path]).toLowerCase().includes(String(p.query).toLowerCase()));
    if (p.category_id) items = items.filter(row => row.category_id === p.category_id);
    const counts = {running:items.filter(r=>r.status==='running').length,stopped:items.filter(r=>r.status==='stopped').length,unknown:items.filter(r=>!['running','stopped'].includes(r.status)).length};
    if(p.status) items=items.filter(r=>(['running','stopped'].includes(r.status)?r.status:'unknown')===p.status);
    const key = p.sort || 'name'; items=[...items].sort((a,b)=>{const x=a[key]??a.id??'',y=b[key]??b.id??'';return (typeof x==='number'?x-y:String(x).localeCompare(String(y)))*(p.order==='desc'?-1:1)});
    const total=items.length, offset=Number(p.offset)||0, limit=Number(p.limit)||100;
    return {items:items.slice(offset,offset+limit),total,offset,limit,status_counts:counts};
  }
  async function hash(value) { return [...new Uint8Array(await crypto.subtle.digest('SHA-256',new TextEncoder().encode(value)))].map(n=>n.toString(16).padStart(2,'0')).join(''); }
  const session = () => ({initialized:true,authenticated,csrf_token:'demo-local-only',user:authenticated?{id:'demo-admin',name:'Demo Admin',kind:'human',scopes:['*'],resources:['*']}:undefined,panel:store.settings});
  const status = () => ({hostname:'folio-demo',os:'Ubuntu 24.04 LTS · Demo',kernel:'6.8.0-demo',architecture:'amd64',version:'1.0-beta7-ui-demo',cpu_percent:23.4,cpu_cores:8,uptime_seconds:186324,memory:{total_bytes:17179869184,used_bytes:7730941132},disk:{total_bytes:536870912000,used_bytes:204010946560},available:true,sampled_at:now(),updated_at:now()});
  function browser(p) { return pick(store.sessions,p.id); }
  function currentTab(s,p) { return s.tabs.find(t=>t.id===(p.tab_id||s.active_tab_id))||s.tabs[0]; }
  function termOutput(t,value) {t.sequence=(t.sequence||0)+1;t.frames.push({sequence:t.sequence,data:btoa(unescape(encodeURIComponent(value)))});t.frames=t.frames.slice(-100);}
  async function run(name,p={}) {
    if(name==='system_status'||name==='system_info')return status();
    if(name==='plugins_catalog')return clone(seed.catalog);
    if(name==='plugins_list')return {...page(store.instances,p),availability:'ready'};
    if(name==='plugins_get')return pick(store.instances,p.id);
    if(name==='sites_list')return page(store.sites,p);
    if(name==='sites_get')return pick(store.sites,p.id);
    if(name==='sites_categories_list')return store.fixtures[name];
    if(name==='sites_create') {
      if(store.sites.some(s=>s.id===p.id))fail(text('网站标识已存在。','Website ID already exists.'),409);
      const site={...safe(p),id:p.id||id('site'),name:p.name||p.domains?.[0]||'Demo website',domains:p.domains||[],status:'running',document_root:p.document_root||'/srv/www/demo',revision:1,updated_at:now(),tls:{enabled:false},configuration_mode:'managed'};
      return task(name,'site:'+site.id,()=>store.sites.push(site),{site_id:site.id});
    }
    if(name==='sites_update'||name==='sites_domains_update'||name==='sites_php_update'||name==='sites_routing_update'||name==='sites_tls_update'){
      const site=pick(store.sites,p.id);check(site,p);return task(name,'site:'+site.id,()=>Object.assign(site,safe(p),{revision:site.revision+1,updated_at:now()}));
    }
    if(name==='sites_default_get')return {id:p.id,default:store.sites.some(s=>s.id===p.id&&s.default),site_id:store.sites.find(s=>s.default)?.id};
    if(name==='sites_default_set'){const site=pick(store.sites,p.id||p.site_id);store.sites.forEach(s=>s.default=s.id===site.id);emit();return site;}
    if(/sites_(enable|disable|delete|purge|retain)/.test(name)){const site=pick(store.sites,p.id);return task(name,'site:'+site.id,()=>{if(/delete|purge/.test(name))store.sites=store.sites.filter(s=>s.id!==site.id);else site.status=/disable|retain/.test(name)?'stopped':'running';site.revision++;});}
    if(name==='sites_backups_list')return {items:[{id:'backup-demo',filename:'northstar-demo.tar.gz',created_at:seed.now,size:204800,sha256:'d'.repeat(64),state:'ready'}],total:1};
    if(name==='sites_retained_list')return {items:[],total:0};
    if(/sites_(logs|access_log|error_log)/.test(name))return {content:'[demo] GET / 200\n[demo] GET /assets/app.js 200\n',lines:['[demo] GET / 200','[demo] GET /assets/app.js 200']};
    if(name==='sites_config_get')return {...pick(store.sites,p.id),content:'server {\n  listen 80;\n  root /srv/www/northstar;\n}\n',revision:pick(store.sites,p.id).revision};
    if(name==='hosted_apps_list')return page(store.apps,p);
    if(name==='hosted_apps_get')return pick(store.apps,p.id);
    if(name==='hosted_apps_create'){const app={...safe(p),id:p.id||id('app'),name:p.name||p.id,runtime:p.runtime||'nodejs',status:'created',running:false,revision:1,updated_at:now()};return task(name,'app:'+app.id,()=>store.apps.push(app));}
    if(name==='hosted_apps_logs')return {content:'[demo] Listening on 0.0.0.0:3010\n[demo] GET /health 200\n',lines:['[demo] Listening on :3010','[demo] GET /health 200']};
    if(/hosted_apps_(control|update|build|delete|backup)/.test(name)){const app=pick(store.apps,p.id);return task(name,'app:'+app.id,()=>{if(/delete/.test(name))store.apps=store.apps.filter(a=>a.id!==app.id);else{Object.assign(app,safe(p));app.running=p.operation!=='stop'&&p.action!=='stop';app.status=app.running?'running':'stopped';app.revision++;}});}
    if(name==='software_plan') {const plan={...safe(p),plan_id:id('plan'),expected_revision:0,instance_id:p.instance_id||id(p.plugin_id||'service'),state:'ready',expires_at:exp(),changes:[text('仅修改当前浏览器中的演示实例。','Only the browser-local demo instance will change.')]};store.plans[plan.plan_id]=plan;save();return plan;}
    if(name==='software_apply'){const plan=store.plans[p.plan_id]||fail('Demo plan expired',409);return task(plan.operation||name,'software:'+plan.instance_id,()=>{const found=store.instances.find(s=>s.id===plan.instance_id||s.instance_id===plan.instance_id);if(plan.operation==='uninstall'){if(found)found.status='uninstalled';}else if(found){found.status='running';found.updated_at=now();}else store.instances.push({id:plan.instance_id,plugin_id:plan.plugin_id,instance_id:plan.instance_id,status:'running',installed_version:plan.version||'demo',updated_at:now(),revision:1});});}
    if(name==='software_service_control'){const item=pick(store.instances,p.id||p.instance_id);return task(name,'software:'+item.id,()=>{item.status=(p.operation||p.action)==='stop'?'stopped':'running';item.updated_at=now();});}
    if(name==='database_connections_list')return page(store.dbs,p);
    if(name==='cache_connections_list')return page(store.caches,p);
    if(name==='database_connection_get')return pick(store.dbs,p.id);
    if(name==='cache_connection_get')return pick(store.caches,p.id);
    if(/^(database|cache)_connection_register$/.test(name)) {const rows=name.startsWith('database')?store.dbs:store.caches;let item=rows.find(r=>r.id===p.id);if(item){check(item,p);Object.assign(item,safe(p),{revision:item.revision+1});}else{item={...safe(p),id:p.id||id('connection'),name:p.name||p.id,revision:1,status:'connected'};rows.push(item);}audit(name,item.id);emit();return item;}
    if(/^(database|cache)_connection_delete$/.test(name)){const key=name.startsWith('database')?'dbs':'caches';store[key]=store[key].filter(r=>r.id!==p.id);emit();return {deleted:true};}
    if(name==='database_status'||name==='cache_status')return {status:'connected',available:true,engine:'demo',latency_ms:3};
    if(name==='database_tables_list')return {items:[{name:'users',rows:24},{name:'orders',rows:128},{name:'sessions',rows:8}],total:3};
    if(name==='database_query'||name==='database_instance_query')return {columns:[{name:'id',type:'integer'},{name:'name',type:'text'},{name:'status',type:'text'}],rows:[[1,'Northstar','active'],[2,'Demo record','active']],affected_rows:0,truncated:false};
    if(name==='managed_database_receipt')return {receipt_id:'demo-receipt',instance_id:p.instance_id,revision:1,expires_at:exp(),state:'ready'};
    if(name==='managed_databases_list')return {items:[{database:'northstar',username:'northstar',engine:'mysql',permission:'data_write',revision:1,state:'active'}],total:1};
    if(name==='managed_database_backups_list')return {items:[],total:0};
    if(name==='cache_keys_scan')return {keys:['session:alice','session:bob','jobs:pending'],cursor:'0',next_cursor:'0',items:['session:alice','session:bob','jobs:pending']};
    if(name==='cache_key_get')return {key:p.key,value:'{"demo":true,"user":"Northstar"}',type:'string',ttl:3600};
    if(name==='cache_key_type')return {key:p.key,type:'string',ttl:3600};
    if(/^cache_(key_set|keys_delete|hash_set|list_push|set_add|set_remove)/.test(name)){audit(name,p.id);emit();return {updated:true,demo:true};}
    if(name==='fs_roots')return clone(seed.fixture_actions.fs_roots);
    if(name==='fs_list'){const directory=(p.path||'').replace(/\/$/,'');const rows=store.files.filter(f=>{const parent=f.path.includes('/')?f.path.slice(0,f.path.lastIndexOf('/')):'';return parent===directory});return {root:p.root||'workspace',path:directory,...page(rows,p)};}
    if(name==='fs_read'){const content=store.contents[p.path]??'';return {root:p.root,path:p.path,content,encoding:'utf-8',size:new TextEncoder().encode(content).length,sha256:await hash(content),truncated:false};}
    if(name==='fs_write'||name==='fs_edit'||name==='fs_create'||name==='fs_text_save') {const content=p.content||'',old=store.contents[p.path]??'';if(p.expected_hash&&p.expected_hash!==await hash(old))fail(text('文件已有变化，请刷新或另存。','File changed. Refresh or save a copy.'),409,'file_conflict');const histories=store.histories[p.path]??=[];histories.unshift({id:id('history'),content:old,created_at:now(),sha256:await hash(old),size:old.length});store.contents[p.path]=content;let f=store.files.find(f=>f.path===p.path);if(!f){f={name:p.path.split('/').pop(),path:p.path,type:'file',mode:'-rw-r--r--'};store.files.push(f);}Object.assign(f,{size:new TextEncoder().encode(content).length,sha256:await hash(content),modified_at:now()});audit(name,p.path);emit();return {...f,content,encoding:'utf-8'};}
    if(name==='fs_mkdir'){const path=p.path;store.files.push({name:path.split('/').pop(),path,type:'directory',size:0,mode:'drwxr-xr-x',modified_at:now()});emit();return {path};}
    if(name==='fs_trash_list')return {...store.fixtures.fs_trash_list,total:store.fixtures.fs_trash_list.items.length};
    if(name==='fs_trash_settings')return {enabled:true,max_age_days:30,max_bytes:1073741824};
    if(name==='fs_trash_clear'){store.fixtures.fs_trash_list.items=[];emit();return {deleted:true};}
    if(name==='fs_formats')return {formats:['zip','tar.gz']};
    if(name==='fs_preflight')return {valid:true,conflicts:[],items:p.items||[],allowed:true};
    if(name==='fs_delete'||name==='fs_trash') {const found=pick(store.files.map(f=>({...f,id:f.path})),p.path);return task(name,p.path,()=>{store.files=store.files.filter(f=>f.path!==p.path);store.fixtures.fs_trash_list.items.push({...found,id:id('trash'),root:p.root,path:p.path,name:found.name,state:'trashed',deleted_at:now()});});}
    if(name==='fs_trash_restore'||name==='fs_restore'){const item=pick(store.fixtures.fs_trash_list.items,p.id);store.files.push({...item,type:item.directory?'directory':'file',modified_at:now()});store.fixtures.fs_trash_list.items=store.fixtures.fs_trash_list.items.filter(t=>t.id!==item.id);emit();return {restored:true};}
    if(name==='fs_trash_purge'||name==='fs_purge'){store.fixtures.fs_trash_list.items=store.fixtures.fs_trash_list.items.filter(t=>t.id!==p.id);emit();return {deleted:true};}
    if(name==='fs_history_list'||name==='fs_history')return {items:store.histories[p.path]||[],total:(store.histories[p.path]||[]).length};
    if(name==='fs_upload_begin'){if(p.size>2*1024*1024)fail(text('静态演示每个文件限 2 MiB。','Static demo files are limited to 2 MiB.'),413);const upload={...p,id:id('upload'),offset:0,state:'uploading',chunks:[]};store.uploads[upload.id]=upload;save();return upload;}
    if(name==='fs_upload_status')return store.uploads[p.id]||fail('Demo upload not found',404);
    if(name==='fs_upload_cancel'){delete store.uploads[p.id];save();return {canceled:true};}
    if(name==='fs_upload_commit'){const u=store.uploads[p.id]||fail('Demo upload not found',404);return task(name,u.path,()=>{const bytes=Uint8Array.from(u.chunks.flat());store.contents[u.path]=new TextDecoder().decode(bytes);u.state='completed';u.offset=u.size;store.files=store.files.filter(f=>f.path!==u.path);store.files.push({name:u.path.split('/').pop(),path:u.path,type:'file',size:u.size,sha256:u.sha256,modified_at:now(),mode:'-rw-r--r--'});delete u.chunks;});}
    if(name==='monitor_history') {const samples=clone(seed.samples);const time=Date.now();samples.forEach((s,i)=>s.timestamp=new Date(time-(samples.length-1-i)*60000).toISOString());return {samples,sample_interval_seconds:60};}
    if(name==='monitor_alerts_list')return page(store.rules,p);
    if(name==='monitor_alerts_save'){let r=store.rules.find(r=>r.id===p.id);if(r){check(r,p);Object.assign(r,safe(p),{revision:r.revision+1});}else{r={...safe(p),id:p.id||id('rule'),revision:1,state:{phase:'unknown',value:null,last_sample:'',events:[]}};store.rules.push(r);}emit();return r;}
    if(name==='monitor_alerts_delete'){store.rules=store.rules.filter(r=>r.id!==p.id);emit();return {deleted:true};}
    if(name.endsWith('_retention_get'))return store.policies[p.id||p.database]||{keep_count:7,keep_days:30,revision:0};
    if(name.endsWith('_retention_save')){const value={...safe(p),revision:Number(p.expected_revision||0)+1};store.policies[p.id||p.database]=value;emit();return value;}
    if(name.endsWith('_retention_plan'))return {plan_id:id('cleanup'),expires_at:exp(),policy:{keep_count:7,keep_days:30},candidates:[],total:1};
    if(name==='services_list')return store.fixtures.services_list;
    if(name==='services_get')return pick(store.fixtures.services_list.items,p.id);
    if(name==='schedules_list'||name==='notifications_channels_list')return page(store.fixtures[name].items,p);
    if(name==='schedules_get')return pick(store.fixtures.schedules_list.items,p.id);
    if(name==='schedules_history')return {items:[],total:0};
    if(/^(schedules|notifications_channels)_(create|save|update|delete|run)$/.test(name)){const key=name.startsWith('schedules')?'schedules_list':'notifications_channels_list', rows=store.fixtures[key].items;let r=rows.find(r=>r.id===p.id);if(name.endsWith('_run'))return task(name,p.id);if(name.endsWith('_delete'))store.fixtures[key].items=rows.filter(r=>r.id!==p.id);else if(r){check(r,p);Object.assign(r,safe(p),{revision:r.revision+1});}else{r={...safe(p),id:p.id||id('schedule'),revision:1,next_run_at:exp(),last_run_status:'not_started'};rows.push(r);}emit();return r||{deleted:true};}
    if(name==='browser_apps_list')return store.fixtures.browser_apps_list;
    if(name==='browser_app_change'){const a=pick(store.fixtures.browser_apps_list.items,p.id);return task(name,'browser:'+p.id,()=>{a.installed=p.operation==='install';a.revision++;if(!a.installed&&a.id==='panel')store.sessions=[];});}
    if(name==='browser_status')return {available:store.fixtures.browser_apps_list.items.some(a=>a.id==='panel'&&a.installed),engine:'static-demo',sessions:store.sessions};
    if(name==='browser_open'){const tab={id:id('tab'),title:'Folio · Static demo',url:'about:blank',can_back:false,can_forward:false};const s={id:id('browser'),tabs:[tab],active_tab_id:tab.id,width:p.width||1280,height:p.height||800,expires_at:exp()};store.sessions.push(s);save();return s;}
    if(name==='browser_close'){store.sessions=store.sessions.filter(s=>s.id!==p.id);emit();return {};}
    if(name==='browser_tabs')return browser(p);
    if(name==='browser_resize'){const s=browser(p);s.width=p.width;s.height=p.height;save();return s;}
    if(name==='browser_navigate'){const s=browser(p),t=currentTab(s,p);t.previous=t.url;t.url=p.url;t.title=p.url==='about:blank'?'Folio · Static demo':new URL(p.url).hostname;t.can_back=true;save();return s;}
    if(name==='browser_history'){const s=browser(p),t=currentTab(s,p);if(t.previous){[t.url,t.previous]=[t.previous,t.url];t.can_forward=true;}save();return s;}
    if(name==='browser_tab_open'){const s=browser(p),t={id:id('tab'),title:'New tab',url:p.url||'about:blank',can_back:false,can_forward:false};s.tabs.push(t);s.active_tab_id=t.id;save();return s;}
    if(name==='browser_tab_close'){const s=browser(p);if(s.tabs.length<2)fail(text('请保留一个标签页，或关闭整个浏览器。','Keep one tab or close the browser.'));s.tabs=s.tabs.filter(t=>t.id!==p.tab_id);s.active_tab_id=s.tabs[0].id;save();return s;}
    if(name==='browser_input'){toast(text('演示画面为示例，可体验地址栏、标签页和尺寸切换。','The page image is a sample. Try the address bar, tabs and viewport controls.'));return {accepted:true};}
    if(name==='terminal_targets_list'||name==='terminal_commands_list')return seed.fixture_actions[name];
    if(name==='terminal_sessions_list')return Object.values(store.terminals);
    if(name==='terminal_open'){const t={id:id('terminal'),target_id:p.target_id,rows:p.rows,cols:p.cols,state:'running',expires_at:exp(),frames:[],sequence:0,input:''};termOutput(t,'Folio static demo terminal\r\nNo server command is executed. Try: help, ls, pwd, uname, echo\r\nfolio@demo:~$ ');store.terminals[t.id]=t;return t;}
    if(name==='terminal_read'){const t=store.terminals[p.id]||fail('Demo terminal closed',404);return {...t,frames:t.frames.filter(f=>f.sequence>(p.after||0)),next:t.sequence,exited:t.state==='closed',exit_code:0,truncated:false};}
    if(name==='terminal_write'||name==='terminal_input'){const t=store.terminals[p.id]||fail('Demo terminal closed',404);const input=decodeURIComponent(escape(atob(p.data||'')));for(const c of input){if(c==='\r'||c==='\n'){const cmd=t.input.trim();let out='';if(cmd==='help')out='Simulated commands: help, ls, pwd, uname, df, echo, clear';else if(cmd==='ls')out=store.files.filter(f=>!f.path.includes('/')).map(f=>f.name).join('  ');else if(cmd==='pwd')out='/srv/folio/workspace';else if(cmd.startsWith('uname'))out='Linux folio-demo 6.8.0 x86_64 (static demo)';else if(cmd==='df'||cmd==='df -h')out='Filesystem  Size  Used  Avail\r\n/demo       500G  190G  310G';else if(cmd.startsWith('echo '))out=cmd.slice(5);else if(cmd==='clear')out='\x1b[2J\x1b[H';else if(cmd)out='Demo: this command is not simulated.';termOutput(t,'\r\n'+out+'\r\nfolio@demo:~$ ');t.input='';}else if(c==='\x7f'){t.input=t.input.slice(0,-1);termOutput(t,'\b \b');}else{t.input+=c;termOutput(t,c);}}return {accepted_bytes:new TextEncoder().encode(input).length};}
    if(name==='terminal_resize'||name==='terminal_renew'){const t=store.terminals[p.id]||fail('Demo terminal closed',404);Object.assign(t,p,{expires_at:exp()});return t;}
    if(name==='terminal_close'){const t=store.terminals[p.id];if(t)t.state='closed';return {closed:true};}
    if(name==='tasks_list')return page(store.tasks,p);
    if(name==='tasks_get')return pick(store.tasks,p.id);
    if(name==='tasks_logs')return {lines:[text('静态演示任务，不执行服务器命令。','Static demo task; no server command is executed.')],content:'[demo] Browser-local task completed.\n',next_offset:1};
    if(name==='tasks_cancel'){const t=pick(store.tasks,p.id);t.status=t.state='canceled';emit();return t;}
    if(name==='audit_list')return page(store.audits,p);
    if(name==='settings_panel_get')return store.settings;
    if(/^settings_(branding|security|basic_auth)_update$/.test(name)){Object.assign(store.settings,safe(p),{revision:store.settings.revision+1});emit();return store.settings;}
    if(name==='software_agent_health'||name==='agent_health')return {available:true,configured:true,version:'demo',status:'ready'};
    if(name==='terminal_status')return {available:true,sessions:[]};
    if(name==='certificates_get')return pick(store.fixtures.certificates_list.items,p.id);
    if(name==='certificates_dns_credentials_list')return {items:[],total:0};
    if(store.fixtures[name])return clone(store.fixtures[name]);
    const capability=capabilities.find(c=>c.name===name);
    // Empty inventories are deliberate demo data; unsupported mutations never claim success.
    if(capability?.read_only && /_list$/.test(name))return {items:[],total:0};
    return unknown(name);
  }
  const capabilities = Array.isArray(seed.capabilities)?seed.capabilities:(seed.capabilities.items||seed.capabilities.actions||[]);
  async function route(url,options) {
    const path=url.pathname.slice('/api/v1'.length), method=(options.method||'GET').toUpperCase();
    let p={};if(options.body&&typeof options.body==='string'){try{p=JSON.parse(options.body);}catch{}}
    if(path==='/session')return session();
    if(path==='/auth/login'){authenticated=true;localStorage.setItem(authKey,'yes');return session();}
    if(path==='/auth/logout'){authenticated=false;localStorage.removeItem(authKey);return {};}
    if(!authenticated)fail(text('请进入演示面板。','Enter the demo panel first.'),401,'unauthenticated');
    if(path==='/capabilities')return capabilities;
    if(path==='/system/status')return status();
    if(path.startsWith('/actions/'))return run(decodeURIComponent(path.slice(9)),p);
    if(path==='/tasks')return store.tasks;
    if(path.startsWith('/tasks/'))return pick(store.tasks,path.split('/')[2]);
    if(path==='/audit')return store.audits;
    if(path==='/notes'&&method==='GET')return store.notes.filter(n=>n.archived===(url.searchParams.get('archived')==='true'));
    if(path==='/notes'&&method==='POST'){const n={...safe(p),id:id('note'),revision:1,archived:false,pinned:!!p.pinned,created_at:now(),updated_at:now(),created_by:seed.actor,updated_by:seed.actor};store.notes.unshift(n);emit();return n;}
    if(path.startsWith('/notes/')){const parts=path.split('/'), n=pick(store.notes,parts[2]);if(parts[3]==='history')return store.histories[n.id]||[];if(parts[3]==='archive'||parts[3]==='restore'){n.archived=parts[3]==='archive';n.revision++;emit();return n;}if(method==='GET')return n;if(method==='DELETE'){store.notes=store.notes.filter(x=>x.id!==n.id);emit();return {deleted:true};}check(n,p);(store.histories[n.id]??=[]).unshift(clone(n));Object.assign(n,safe(p),{revision:n.revision+1,updated_at:now(),updated_by:seed.actor});emit();return n;}
    if(path==='/tokens'&&method==='GET')return store.tokens;
    if(path==='/tokens'&&method==='POST'){const t={...safe(p),id:id('token'),created_at:now(),last_used_at:null};store.tokens.push(t);emit();return {...t,token:'demo-only-not-a-real-token-'+id('local')};}
    if(path.startsWith('/tokens/')&&method==='DELETE'){store.tokens=store.tokens.filter(t=>t.id!==path.split('/')[2]);emit();return {revoked:true};}
    if(path.startsWith('/account/')){toast(text('演示模式：账户安全设置不会更改真实密码或两步验证。','Demo mode: real passwords and two-step verification are not changed.'));return {demo:true,secret:'DEMOONLYNOTFORLOGIN',uri:'otpauth://totp/Folio:demo?secret=DEMOONLYNOTFORLOGIN',recovery_codes:['demo-only-not-valid']};}
    if(path.startsWith('/files/text'))return new Response(store.contents[url.searchParams.get('path')]||'',{headers:{'Content-Type':'text/plain;charset=utf-8'}});
    if(path.includes('/files/uploads/')&&method==='PATCH'){const upload=store.uploads[path.split('/')[3]]||fail('Demo upload not found',404);const bytes=options.body instanceof Uint8Array?options.body:new Uint8Array(await new Response(options.body).arrayBuffer());upload.chunks.push([...bytes]);upload.offset+=bytes.length;save();return new Response(null,{status:204});}
    if(path.endsWith('/frame')){const ids=path.split('/'),s=pick(store.sessions,ids[3]),t=s.tabs.find(t=>t.id===ids[5])||s.tabs[0];const frame=t.url.includes('mozilla')?'mdn':t.url==='about:blank'?'welcome':'atlas';return nativeFetch('/demo/browser-'+frame+'.jpg');}
    return unknown(path);
  }
  window.fetch=async(input,options={})=>{
    const url=new URL(typeof input==='string'?input:input.url,location.href);
    if(url.origin===location.origin&&url.pathname==='/healthz')return new Response(JSON.stringify({status:'ok',ready:true,version:'1.0-beta7-ui-demo'}),{headers:{'Content-Type':'application/json'}});
    if(url.origin!==location.origin||!url.pathname.startsWith('/api/'))return nativeFetch(input,options);
    if(options.signal?.aborted)throw new DOMException('Aborted','AbortError');
    await new Promise(r=>setTimeout(r,45));
    try{const result=await route(url,options);return result instanceof Response?result:new Response(JSON.stringify({data:result}),{headers:{'Content-Type':'application/json'}});}catch(e){return new Response(JSON.stringify({error:{code:e.code||'demo_error',message:e.message}}),{status:e.status||500,headers:{'Content-Type':'application/json'}});}
  };
  class DemoEvents extends EventTarget {
    static CONNECTING=0;static OPEN=1;static CLOSED=2;
    constructor(){super();this.readyState=0;streams.add(this);setTimeout(()=>{if(this.readyState===2)return;this.readyState=1;this.onopen?.(new Event('open'));},100);}
    close(){this.readyState=2;streams.delete(this);}
  }
  window.EventSource=DemoEvents;
  let toastTimer;
  function toast(message){let el=document.getElementById('demo-toast');if(!el){el=document.createElement('div');el.id='demo-toast';el.setAttribute('role','status');document.body.appendChild(el);}el.textContent=message;el.hidden=false;clearTimeout(toastTimer);toastTimer=setTimeout(()=>el.hidden=true,5000);}
  function boot(){
    const style=document.createElement('style');style.textContent=`#folio-demo-tools{position:fixed;right:16px;bottom:104px;z-index:90;font:13px/1.5 system-ui,sans-serif;color:var(--text,#182230)}#folio-demo-tools>summary{cursor:pointer;list-style:none;border:1px solid var(--border,#dde3ec);border-radius:20px;padding:8px 12px;background:var(--surface,#fff);box-shadow:0 4px 20px #18223012}#folio-demo-tools[open]{max-width:300px;border:1px solid var(--border,#dde3ec);border-radius:12px;background:var(--surface,#fff);padding:12px;box-shadow:0 8px 32px #18223020}#folio-demo-tools p{margin:12px 0}#folio-demo-tools button,#demo-enter{cursor:pointer;font:inherit;min-height:40px;border:1px solid var(--border,#dde3ec);border-radius:8px;padding:8px 12px;color:var(--text,#182230);background:var(--bg,#f5f7fb);margin:4px 4px 0 0}#demo-enter{display:block;width:100%;background:var(--soft,#eaf0ff);color:var(--accent,#2859d6);margin:16px 0}#demo-toast{position:fixed;z-index:150;bottom:108px;left:50%;transform:translateX(-50%);max-width:min(560px,90vw);background:var(--surface,#fff);color:var(--text,#182230);border:1px solid var(--border,#dde3ec);border-radius:10px;padding:12px 16px;box-shadow:0 4px 24px #18223020;font:14px/1.5 system-ui,sans-serif}@media(max-width:640px){#folio-demo-tools{right:12px;bottom:84px}#demo-toast{bottom:132px}}`;document.head.appendChild(style);
    const panel=document.createElement('details');panel.id='folio-demo-tools';panel.innerHTML='<summary></summary><p></p><button id="demo-reset" type="button"></button><button id="demo-login-view" type="button"></button><button id="demo-copy-page" type="button"></button>';document.body.appendChild(panel);
    function labels(){const enter=document.getElementById('demo-enter');if(enter)enter.textContent=text('直接进入演示，无需真实账号','Enter demo — no account needed');panel.querySelector('summary').textContent=text('演示数据','Demo data');panel.querySelector('p').textContent=text('所有修改只保存在当前浏览器。终端和浏览器使用模拟内容，不连接服务器。请勿输入真实密码或令牌。','Changes stay in this browser. Terminal and browser content is simulated, with no server connection. Do not enter real passwords or tokens.');document.getElementById('demo-reset').textContent=text('重置演示','Reset demo');document.getElementById('demo-login-view').textContent=text('查看登录页','View login');document.getElementById('demo-copy-page').textContent=text('复制当前页面链接','Copy page link');}
    labels();setInterval(labels,1200);
    document.getElementById('demo-reset').onclick=()=>{if(confirm(text('重置当前浏览器里的全部演示数据？配色和语言会保留。','Reset demo data in this browser? Appearance and language are kept.'))){store=fresh();save();location.reload();}};
    document.getElementById('demo-login-view').onclick=()=>{localStorage.removeItem(authKey);location.hash='#/overview';location.reload();};
    document.getElementById('demo-copy-page').onclick=()=>navigator.clipboard.writeText(location.href).then(()=>toast(text('已复制当前页面链接，可连同问题描述一起反馈。','Page link copied. Include it with your feedback.')));
    const observe=new MutationObserver(()=>{const form=document.querySelector('.auth-card form');if(form&&!document.getElementById('demo-enter')){const button=document.createElement('button');button.id='demo-enter';button.type='button';button.textContent=text('直接进入演示，无需真实账号','Enter demo — no account needed');button.onclick=()=>{localStorage.setItem(authKey,'yes');location.reload();};form.before(button);for(const input of form.querySelectorAll('input[type=password]'))input.autocomplete='off';}});observe.observe(document.body,{subtree:true,childList:true});
    document.addEventListener('click',event=>{const a=event.target.closest('a');if(!a)return;const url=new URL(a.href,location.href);if(url.origin!==location.origin||!url.pathname.startsWith('/api/'))return;event.preventDefault();if(url.pathname.includes('/files/download')){const path=url.searchParams.get('path'),blob=new Blob([store.contents[path]||'Demo file'],{type:'text/plain;charset=utf-8'}),link=document.createElement('a');link.href=URL.createObjectURL(blob);link.download=path?.split('/').pop()||'demo.txt';link.click();setTimeout(()=>URL.revokeObjectURL(link.href),5000);}else if(url.pathname.includes('firefox-wasm'))window.open('/demo/firefox.html','_blank','noopener');else toast(text('此链接在静态演示中使用模拟结果。','This link uses a simulated result in the static demo.'));});
  }
  if(document.readyState==='loading')document.addEventListener('DOMContentLoaded',boot);else boot();
  window.FolioDemo={reset(){store=fresh();emit();},getState:()=>clone(store),version:'1'};
})();
