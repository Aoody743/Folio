"use client";
import { useEffect, useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { Capability, fullDate } from "@/lib/api";
import { useResource } from "@/lib/resource-hook";
import { t as translate } from "@/lib/i18n";
import { navigate } from "@/lib/navigation";
import { ResourceError } from "./resource-ui";
type Health={status:string;version:string;ready:boolean};
export function EnvironmentView({capabilities}:{capabilities:Capability[]}) {
  const [health,setHealth]=useState<Health>();
  const [error,setError]=useState("");
  const [checked,setChecked]=useState("");
  const [sequence,setSequence]=useState(0);
  const allowed=(name:string)=>capabilities.some(c=>c.name===name);
  const executor=useResource<{availability?:string}>("plugins_list",{},allowed("plugins_list"),0);
  const browsers=useResource<{items:{id:string;installed:boolean;available:boolean;reason?:string}[]}>("browser_apps_list",{},allowed("browser_apps_list"),0);
  useEffect(()=>{
    const controller=new AbortController();
    const timeout=setTimeout(()=>{controller.abort();setError(translate("连接检查超时，请重试。"));},10000);
    setHealth(undefined);setError("");setChecked("");
    void fetch("/healthz",{cache:"no-store",credentials:"same-origin",signal:controller.signal}).then(async r=>{
      if(!r.ok) throw new Error(translate("面板连接检查失败"));
      const value=await r.json() as Health;
      if(value.status!=="ok" || !value.ready || typeof value.version!=="string") throw new Error(translate("面板尚未就绪"));
      if(!controller.signal.aborted){setHealth(value);setChecked(new Date().toISOString());}
    }).catch(()=>{if(!controller.signal.aborted) setError(translate("无法确认面板连接，请检查服务后重试。"));}).finally(()=>clearTimeout(timeout));
    return()=>{controller.abort();clearTimeout(timeout);};
  },[sequence]);
  const retry=()=>{setSequence(s=>s+1);void executor.reload();void browsers.reload();};
  return <Card className="panel-card environment-panel">
    <div className="card-heading"><h2>{translate("环境与维护")}</h2><Button label={translate("重新检查")} onClick={retry}/></div>
    <section><h3>{translate("面板连接")}</h3>
      {error ? <ResourceError error={error} retry={retry}/> : health ? <><p>{translate("已连接")} · {translate("当前版本")} <strong>{health.version}</strong></p><p className="muted">{translate("检查时间")} {fullDate(checked)}</p></> : <p role="status">{translate("正在检查…")}</p>}
      <p className="muted">{translate("连接检查只验证面板服务，不代表网站公网可用或主机全部正常。")}</p>
    </section>
    <section><h3>{translate("主机执行器")}</h3>
      {!allowed("plugins_list") ? <p>{translate("当前账号无权检查此项")}</p> : executor.error ? <ResourceError error={executor.error} state={executor.state} retry={()=>void executor.reload()}/> : !executor.data ? <p role="status">{translate("正在检查…")}</p> : executor.data.availability==="agent_not_configured" ? <p className="notice warning">{translate("执行器尚未配置")}</p> : <p>{translate("执行器已响应；具体操作仍受主机策略限制。")}</p>}
      <a href="https://github.com/Aoody743/Folio/blob/main/scripts/rewrite/README.md#optional-privileged-host-executor" target="_blank" rel="noopener noreferrer">{translate("查看执行器配置说明")}</a>
    </section>
    <section><h3>{translate("浏览器组件")}</h3>
      {!allowed("browser_apps_list") ? <p>{translate("当前账号无权检查此项")}</p> : browsers.error ? <ResourceError error={browsers.error} state={browsers.state} retry={()=>void browsers.reload()}/> : !browsers.data ? <p role="status">{translate("正在检查…")}</p> : <ul>{browsers.data.items.map(app=><li key={app.id}>{app.id==="panel"?translate("面板浏览器"):"Firefox WASM"} · {app.installed ? app.available?translate("可用"):translate("需要检查"):translate("未安装")}{app.reason && <details><summary>{translate("技术详情")}</summary>{app.reason}</details>}</li>)}</ul>}
      {allowed("browser_apps_list") && <Button label={translate("管理浏览器应用")} variant="ghost" onClick={()=>navigate("software",{page:"applications",subgroup:"browsers"})}/>}
    </section>
    <section><h3>{translate("版本维护")}</h3><p>{translate("先查看发行说明并备份，再按升级说明更新。此处不会自动安装或重启服务。")}</p><div className="button-row"><a href="https://github.com/Aoody743/Folio/releases" target="_blank" rel="noopener noreferrer">{translate("发行版本与更新说明")}</a><a href="https://github.com/Aoody743/Folio/blob/main/scripts/rewrite/README.md" target="_blank" rel="noopener noreferrer">{translate("安装与升级说明")}</a></div></section>
  </Card>;
}
