"use client";
import { useEffect, useState } from "react";
import { Capability, list } from "@/lib/api";
import { Page, Site, SoftwareInstance } from "@/lib/resources";
import { useResource } from "@/lib/resource-hook";
import { navigate, Section } from "@/lib/navigation";
import { isFirstReleaseCatalogPlugin } from "@/lib/first-release-surface";
import { t as translate } from "@/lib/i18n";
import { ResourceError } from "./resource-ui";
import { Icon } from "./icon";
type Named = { id: string; name?: string; engine?: string; host?: string };
export function ResourceSearch({query, capabilities, onPick}: {query:string; capabilities:Capability[]; onPick:()=>void}) {
  const [settled, setSettled] = useState("");
  useEffect(() => { const timer=setTimeout(()=>setSettled(query.trim()),250); return ()=>clearTimeout(timer); },[query]);
  const enabled = !!settled && settled === query.trim();
  const allowed=(name:string)=>enabled && capabilities.some(c=>c.name===name);
  const sites=useResource<Page<Site>>("sites_list",{query:settled,limit:20,offset:0},allowed("sites_list"),0);
  const apps=useResource<Page<Named>>("hosted_apps_list",{},allowed("hosted_apps_list"),0);
  const db=useResource<Page<Named>>("database_connections_list",{},allowed("database_connections_list"),0);
  const cache=useResource<Page<Named>>("cache_connections_list",{},allowed("cache_connections_list"),0);
  const software=useResource<Page<SoftwareInstance>>("plugins_list",{},allowed("plugins_list"),0);
  const matches=(...parts:(string|undefined)[])=>parts.some(p=>p?.toLocaleLowerCase().includes(settled.toLocaleLowerCase()));
  const groups = [
    {label:translate("网站"),resource:sites,items:list(sites.data).map(s=>({id:s.id,title:s.name || s.domains.join(", "),detail:s.domains.join(", "),section:"sites" as Section,params:{id:s.id}}))},
    {label:translate("应用"),resource:apps,items:list(apps.data).filter(s=>matches(s.name,s.id)).map(s=>({id:s.id,title:s.name || s.id,detail:"",section:"software" as Section,params:{page:"applications",subgroup:"hosted",id:s.id}}))},
    {label:translate("数据库"),resource:db,items:list(db.data).filter(s=>s.engine!=="mongodb" && matches(s.name,s.id,s.host)).map(s=>({id:s.id,title:s.name || s.id,detail:s.engine ?? "",section:"software" as Section,params:{page:"database",id:s.id}}))},
    {label:translate("缓存"),resource:cache,items:list(cache.data).filter(s=>matches(s.name,s.id,s.host)).map(s=>({id:s.id,title:s.name || s.id,detail:s.engine ?? "",section:"software" as Section,params:{page:"cache",id:s.id}}))},
    {label:translate("已安装服务"),resource:software,items:list(software.data).filter(s=>isFirstReleaseCatalogPlugin(s.plugin_id) && matches(s.id,s.instance_id,s.plugin_id)).map(s=>({id:s.id,title:s.instance_id || s.id,detail:s.plugin_id,section:"software" as Section,params:{page:"installed",id:s.id}}))},
  ];
  if (!enabled) return query.trim() ? <p role="status">{translate("正在搜索…")}</p> : null;
  return <div className="resource-search">
    {groups.map(group=><section key={group.label}>
      {group.resource.state!=="unknown" && <h3>{group.label}</h3>}
      {group.resource.loading && <p role="status">{translate("正在搜索…")}</p>}
      {group.resource.error && <ResourceError error={group.resource.error} state={group.resource.state} retry={()=>void group.resource.reload()}/>}
      {group.resource.state==="unconfigured" && !group.resource.error && <p>{translate("执行器尚未配置")}</p>}
      {group.items.slice(0,8).map(item=><button key={item.id} onClick={()=>{ if(navigate(item.section,item.params)) onPick(); }}><Icon name="search"/><span>{item.title}{item.detail && <small>{item.detail}</small>}</span><small>{group.label}</small></button>)}
      {group.resource.data && !group.resource.error && group.resource.state!=="unconfigured" && !group.items.length && <p className="muted">{translate("没有匹配的资源")}</p>}
      {group.items.length>8 && <p className="muted">{translate("仅显示前 {0} 项，请缩小搜索范围。",[8])}</p>}
      {group===groups[0] && sites.data?.total!==undefined && sites.data.total>20 && <p className="muted">{translate("还有更多网站，请缩小搜索范围。")}</p>}
    </section>)}
  </div>;
}
