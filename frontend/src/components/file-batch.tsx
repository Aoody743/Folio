"use client";
import { useState } from "react";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Capability } from "@/lib/api";
import { FileEntry, FileRoot } from "@/lib/resources";
import { t as translate } from "@/lib/i18n";
import { ActionForm } from "./actions";
import { ResourceDrawer } from "./resource-ui";
type Preview={items:{name:string;destination?:string;exists:boolean;current_hash?:string;entry:FileEntry}[];total:number;conflicts:number};
export function FileBatch({root,path,items,roots,capabilities,onClose,onSaved}:{root:string;path:string;items:FileEntry[];roots:FileRoot[];capabilities:Capability[];onClose:()=>void;onSaved:()=>void}) {
  const [operation,setOperation]=useState("copy");
  const [destinationRoot,setDestinationRoot]=useState(root);
  const [destinationPath,setDestinationPath]=useState("");
  const [recursive,setRecursive]=useState(false);
  const [overwrite,setOverwrite]=useState(false);
  const [preview,setPreview]=useState<{signature:string;value:Preview}>();
  const input={root,path,items:items.map(i=>i.name),operation,...(operation!=="delete"?{destination_root:destinationRoot,destination_path:destinationPath}:{}),recursive};
  const signature=JSON.stringify(input);
  const current=preview?.signature===signature?preview.value:undefined;
  const preflight=capabilities.find(c=>c.name==="fs_preflight");
  const batch=capabilities.find(c=>c.name==="fs_batch");
  const hasDirectories=items.some(i=>i.type==="directory");
  const canOverwrite=!!current && current.items.filter(i=>i.exists).every(i=>!!i.current_hash && i.entry.type!=="directory");
  const expected=Object.fromEntries(current?.items.filter(i=>i.exists && i.destination && i.current_hash).map(i=>[i.destination!,i.current_hash!])??[]);
  return <ResourceDrawer open title={translate("批量文件操作")} onClose={onClose}>
    <div className="form-stack">
      <p>{translate("已选择 {0} 项",[items.length])}</p><ul className="batch-file-names">{items.map(i=><li key={i.path}>{i.name}</li>)}</ul>
      <label className="native-field">{translate("操作")}<select aria-label={translate("操作")} value={operation} onChange={e=>{setOperation(e.target.value);setOverwrite(false);}}><option value="copy">{translate("复制")}</option><option value="move">{translate("移动")}</option><option value="delete">{translate("删除")}</option></select></label>
      {operation!=="delete" && <><label className="native-field">{translate("目标根目录")}<select aria-label={translate("目标根目录")} value={destinationRoot} onChange={e=>{setDestinationRoot(e.target.value);setOverwrite(false);}}>{roots.filter(r=>!r.read_only).map(r=><option value={r.id} key={r.id}>{r.label || r.id}</option>)}</select></label><TextInput label={translate("目标目录")} value={destinationPath} onChange={v=>{setDestinationPath(v);setOverwrite(false);}}/></>}
      {hasDirectories && <label className="check-label"><input type="checkbox" checked={recursive} onChange={e=>setRecursive(e.target.checked)}/>{translate("包含所选目录内的全部内容")}</label>}
      {preflight && (!hasDirectories || recursive) && <ActionForm key={signature} cacheKeySuffix={signature} capability={preflight} showInputTools={false} preset={input} lockedPresetFields={Object.keys(input)} presentation="business" showTitle={false} renderFields={()=>null} onExecuted={(data,context)=>{if(context?.task && (context.task.status==="succeeded" || context.task.state==="succeeded") && data && typeof data==="object" && Array.isArray((data as Preview).items)){setPreview({signature,value:data as Preview});setOverwrite(false);}}}/>}
      {current && <section aria-label={translate("变更预检")}><p>{translate("预检 {0} 项，目标冲突 {1} 项。",[current.total,current.conflicts])}</p>
        {!!current.conflicts && (canOverwrite?<label className="check-label"><input type="checkbox" checked={overwrite} onChange={e=>setOverwrite(e.target.checked)}/>{translate("确认覆盖预检中已存在的文件；提交时再次核对版本。")}</label>:<p className="notice warning">{translate("存在不能覆盖的目录或目标，请更换目录后重新预检。")}</p>)}
        <p className="muted">{translate("批量操作可能部分完成，请查看任务中的逐项结果。删除是否进入回收站由根目录设置决定。")}</p>
        {batch && (!current.conflicts || (canOverwrite && overwrite)) && <ActionForm key={signature+JSON.stringify(expected)} cacheKeySuffix={signature+JSON.stringify(expected)} capability={batch} showInputTools={false} preset={{...input,expected_hashes:expected}} lockedPresetFields={[...Object.keys(input),"expected_hashes"]} presentation="business" showTitle={false} renderFields={()=>null} onExecuted={onSaved}/>}
      </section>}
    </div>
  </ResourceDrawer>;
}
