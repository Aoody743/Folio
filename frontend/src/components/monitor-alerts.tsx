"use client";
import { useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Capability,fullDate,list } from "@/lib/api";
import { useResource } from "@/lib/resource-hook";
import { t as translate } from "@/lib/i18n";
import { ActionForm } from "./actions";
import { ResourceError } from "./resource-ui";
import { TaskResult } from "./task-result";
type Rule={id:string;name:string;metric:string;threshold:number;duration_seconds:number;cooldown_seconds:number;channel_id?:string;enabled:boolean;revision:number;state:{phase:string;value:number|null;last_sample:string;notification_state?:string;task_id?:string;error_code?:string;events:{timestamp:string;phase:string;value:number|null}[]}};
const phase=(value:string)=>({normal:translate("正常"),pending:translate("持续观察中"),firing:translate("已触发"),unknown:translate("数据未知"),authorization_lost:translate("授权已失效")}[value]??translate("状态未知"));
export function MonitorAlerts({capabilities}:{capabilities:Capability[]}){
 const allowed=(name:string)=>capabilities.some(c=>c.name===name);
 const rules=useResource<Rule[]>("monitor_alerts_list",{},allowed("monitor_alerts_list"));
 const channels=useResource<{id:string;name:string}[]>("notifications_channels_list",{},allowed("notifications_channels_list"));
 const [saved,setSaved]=useState(false);const [selected,setSelected]=useState<Rule>();const [creating,setCreating]=useState(false);const [showTask,setShowTask]=useState(false);
 const save=capabilities.find(c=>c.name==="monitor_alerts_save"),remove=capabilities.find(c=>c.name==="monitor_alerts_delete");
 const current=list(rules.data).find(r=>r.id===selected?.id)??selected;
 const refresh=()=>{void rules.reload();window.dispatchEvent(new Event("powerlinux:invalidate"));};
 const metrics=[{id:"cpu",label:"CPU"},{id:"memory",label:translate("内存")},{id:"disk",label:translate("磁盘")}];
 return <div className="form-stack alert-rules">
  <p>{translate("按真实采样判断 CPU、内存或磁盘是否持续超限。缺失数据不算正常，也不计入持续时间。")}</p>
  <p className="muted">{translate("规则默认停用。可只记录本地状态，或选择通知渠道；提交通知任务不代表收件人已收到。恢复需回落到阈值以下 5 个百分点。")}</p>
  {rules.error && <ResourceError error={rules.error} state={rules.state} retry={()=>void rules.reload()}/>}
  {save && !creating && !current && <Button label={translate("新增告警规则")} onClick={()=>{setCreating(true);setSaved(false);}}/>}
  {(creating || current) && save ? <section>
   <Button label={translate("返回规则列表")} variant="ghost" onClick={()=>{setSelected(undefined);setCreating(false);setShowTask(false);}}/>
   <h2>{creating?translate("新增告警规则"):current?.name}</h2>
   {saved && <p role="status">{translate("告警规则已保存。")}</p>}
   <ActionForm key={current?.id??"new"} capability={save} showInputTools={false} showResult={false} presentation="business" showTitle={false} preset={current?{id:current.id,expected_revision:current.revision,name:current.name,metric:current.metric,threshold:current.threshold,duration_seconds:current.duration_seconds,cooldown_seconds:current.cooldown_seconds,enabled:current.enabled,channel_id:current.channel_id??""}:{metric:"cpu",threshold:90,duration_seconds:300,cooldown_seconds:3600,enabled:false,channel_id:""}} lockedPresetFields={current?["id","expected_revision"]:[]} onExecuted={data=>{const saved=data as Rule;if(saved && typeof saved.id==="string"){setSelected({...saved,state:current?.state??{phase:"unknown",value:null,last_sample:"",events:[]}});setCreating(false);setSaved(true);}refresh();}} renderFields={({value,onChange})=>{
     const v=value as Record<string,unknown>;const change=(key:string,next:unknown)=>onChange({...v,[key]:next});
     return <div className="form-stack">
      <TextInput label={translate("规则名称")} value={String(v.name??"")} onChange={name=>change("name",name)}/>
      <label className="native-field">{translate("监控指标")}<select aria-label={translate("监控指标")} value={String(v.metric??"cpu")} onChange={e=>change("metric",e.target.value)}>{metrics.map(m=><option value={m.id} key={m.id}>{m.label}</option>)}</select></label>
      <label className="native-field">{translate("触发阈值 (%)")}<input type="number" min={1} max={100} value={v.threshold==null?"":String(v.threshold)} onChange={e=>change("threshold",e.target.value===""?undefined:Number(e.target.value))}/></label>
      <label className="native-field">{translate("持续时间（秒）")}<input type="number" min={60} max={3600} value={v.duration_seconds==null?"":String(v.duration_seconds)} onChange={e=>change("duration_seconds",e.target.value===""?undefined:Number(e.target.value))}/></label>
      <label className="native-field">{translate("通知间隔（秒）")}<input type="number" min={60} max={86400} value={v.cooldown_seconds==null?"":String(v.cooldown_seconds)} onChange={e=>change("cooldown_seconds",e.target.value===""?undefined:Number(e.target.value))}/></label>
      <label className="native-field">{translate("通知渠道")}<select aria-label={translate("通知渠道")} value={String(v.channel_id??"")} onChange={e=>change("channel_id",e.target.value)}><option value="">{translate("仅记录本地状态")}</option>{list(channels.data).map(c=><option key={c.id} value={c.id}>{c.name}</option>)}{v.channel_id && !list(channels.data).some(c=>c.id===v.channel_id)?<option value={String(v.channel_id)}>{translate("原通知渠道")}</option>:null}</select></label>
      {channels.error && <ResourceError error={channels.error} state={channels.state} retry={()=>void channels.reload()}/>}
      <label className="check-label"><input type="checkbox" checked={v.enabled===true} onChange={e=>change("enabled",e.target.checked)}/>{translate("启用此规则")}</label>
     </div>;
   }}/>
   {current && <><p role="status">{phase(current.state.phase)} · {current.state.value===null?"—":`${current.state.value.toFixed(1)}%`} · {Date.parse(current.state.last_sample)>0?fullDate(current.state.last_sample):"—"}</p>{current.state.error_code && <details><summary>{translate("技术详情")}</summary><code>{current.state.error_code}</code></details>}<h3>{translate("最近状态变化")}</h3><ul>{current.state.events?.slice().reverse().map((event,i)=><li key={i}>{fullDate(event.timestamp)} · {phase(event.phase)} · {event.value===null?"—":`${event.value.toFixed(1)}%`}</li>)}</ul>
     {current.state.task_id && allowed("tasks_get") && <><Button label={translate("查看通知任务")} variant="ghost" onClick={()=>setShowTask(!showTask)}/>{showTask && <TaskResult submitted={{task_id:current.state.task_id}}/>}</>}
     {remove && <details className="advanced-actions"><summary>{translate("删除规则")}</summary><ActionForm capability={remove} showInputTools={false} preset={{id:current.id,expected_revision:current.revision}} lockedPresetFields={["id","expected_revision"]} presentation="business" showTitle={false} onExecuted={()=>{setSelected(undefined);refresh();}}/></details>}
   </>}
  </section>:<div className="rows">{list(rules.data).map(rule=><button className="alert-rule-row" key={rule.id} onClick={()=>{setSelected(rule);setCreating(false);setShowTask(false);setSaved(false);}}><strong>{rule.name}</strong><span>{!rule.enabled?translate("已停用"):Date.now()-Date.parse(rule.state.last_sample)>120000?translate("数据已过期"):phase(rule.state.phase)}</span><small>{metrics.find(m=>m.id===rule.metric)?.label} ≥ {rule.threshold}% · {rule.duration_seconds}s</small></button>)}{rules.data && !rules.error && !list(rules.data).length && <p>{translate("尚未配置告警规则")}</p>}</div>}
 </div>;
}
