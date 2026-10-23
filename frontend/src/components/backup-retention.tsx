"use client";
import { useState } from "react";
import { Capability,bytes,fullDate,list,Task } from "@/lib/api";
import { useResource } from "@/lib/resource-hook";
import { t as translate } from "@/lib/i18n";
import { ActionForm } from "./actions";
import { ResourceError,State } from "./resource-ui";
import { TaskResult } from "./task-result";
type Policy={keep_count:number;keep_days:number;revision:number};
type Plan={plan_id:string;expires_at:string;policy:Policy;candidates:{id:string;created_at:string;size:number}[];total:number};
export function BackupRetention({kind,target,capabilities,onExecuted}:{kind:"site"|"database";target:Record<string,string>;capabilities:Capability[];onExecuted:()=>void}){
 const [open,setOpen]=useState(false);const [saved,setSaved]=useState(false);const [plan,setPlan]=useState<Plan>();const [selectedTask,setSelectedTask]=useState<string>();
 const prefix=kind==="site"?"sites_retention_":"managed_database_retention_";
 const find=(name:string)=>capabilities.find(c=>c.name===prefix+name);
 const policy=useResource<Policy>(prefix+"get",target,open && !!find("get"),0);
 const history=useResource<Task[]>("tasks_list",{},open && capabilities.some(c=>c.name==="tasks_list"),0);
 const resource=kind==="site"?`site:${target.id}`:`db:${target.instance_id}:${target.database}`;
 const restores=list(history.data).filter(task=>task.resource===resource && /sites_backup_restore|site_backup_restore|managed_database_restore/.test([task.action,task.operation].join(" "))).slice(0,10);
 const save=find("save"),preview=find("plan"),apply=find("apply");
 if(!find("get"))return null;
 return <details className="advanced-actions backup-retention" open={open} onToggle={e=>setOpen(e.currentTarget.open)}><summary>{translate("保留策略、清理与恢复记录")}</summary>
  {open && <div className="form-stack">
   <p>{translate("同时保留最新指定份数及指定天数内的备份。未知日期或摘要的备份不会自动列入清理。保存策略不会触发删除。")}</p>
   {policy.error && <ResourceError error={policy.error} state={policy.state} retry={()=>void policy.reload()}/>}
   {policy.data && save && <ActionForm capability={save} showInputTools={false} showResult={false} presentation="business" showTitle={false} preset={{...target,keep_count:policy.data.keep_count,keep_days:policy.data.keep_days,expected_revision:policy.data.revision}} lockedPresetFields={[...Object.keys(target),"expected_revision"]} onExecuted={()=>{setSaved(true);setPlan(undefined);void policy.reload();}}/>}
   {saved && <p role="status">{translate("保留策略已保存。")}</p>}
   {policy.data && preview && <ActionForm capability={preview} showInputTools={false} showResult={false} presentation="business" showTitle={false} preset={target} lockedPresetFields={Object.keys(target)} renderFields={()=>null} onExecuted={data=>{if(data && typeof data==="object" && Array.isArray((data as Plan).candidates))setPlan(data as Plan);}}/>}
   {plan && <section><h3>{translate("待清理备份")}</h3><p>{translate("本次保留至少 {0} 份及最近 {1} 天。",[plan.policy.keep_count,plan.policy.keep_days])}</p><p>{translate("共 {0} 份备份，本次清理 {1} 份。",[plan.total,plan.candidates.length])}</p><p className="muted">{translate("预览有效至 {0}",[fullDate(plan.expires_at)])}</p>
    <ul>{plan.candidates.map(item=><li key={item.id}>{fullDate(item.created_at)} · {bytes(item.size)} <code>{item.id}</code></li>)}</ul>
    {!!plan.candidates.length && apply && <ActionForm key={plan.plan_id} cacheKeySuffix={plan.plan_id} capability={apply} showInputTools={false} presentation="business" showTitle={false} preset={{...target,plan_id:plan.plan_id}} lockedPresetFields={[...Object.keys(target),"plan_id"]} renderFields={()=>null} onExecuted={onExecuted}/>}
   </section>}
   <section><h3>{translate("最近恢复任务")}</h3><p className="muted">{translate("这里只记录已授权任务的执行结果，不代表已经完成应用可用性验证；恢复后仍需检查网站或数据库。")}</p>
    {history.error && <ResourceError error={history.error} state={history.state} retry={()=>void history.reload()}/>}
    {restores.map(task=><button key={task.id} className="alert-rule-row" onClick={()=>setSelectedTask(task.id)}><span>{fullDate(task.created_at)}</span><State value={task.status??task.state}/></button>)}
    {!restores.length && !history.error && <p>{translate("当前可见任务中没有恢复记录")}</p>}
    {selectedTask && <TaskResult submitted={{task_id:selectedTask}}/>}
   </section>
  </div>}
 </details>;
}
