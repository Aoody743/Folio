"use client";
import { Icon } from "./icon";
import { useEffect,useState } from "react";
import { t as translate } from "@/lib/i18n";
import { bookmarkURL,readBookmarks } from "@/lib/browser-usability";
const key="folio.browser.bookmarks.v1";
export function BrowserBookmarks({currentURL,onOpen}:{currentURL?:string;onOpen:(url:string)=>void}) {
  const [items,setItems]=useState<string[]>([]);
  const [error,setError]=useState("");
  useEffect(()=>{try{setItems(readBookmarks(JSON.parse(localStorage.getItem(key)??"[]")));}catch{}},[]);
  const save=(next:string[])=>{try{localStorage.setItem(key,JSON.stringify(next));setItems(next);setError("");}catch{setError(translate("浏览器未允许保存偏好，请检查存储设置。"));}};
  const candidate=currentURL?bookmarkURL(currentURL):undefined;
  return <details className="browser-bookmarks"><summary><Icon name="bookmark" size={18} />{translate("常用地址")} ({items.length})</summary>
    <p className="muted">{translate("保存在当前浏览器，同一浏览器的使用者可见。仅保存网址路径，自动去掉账号、查询参数和片段；不记录浏览历史。")}</p>
    <button type="button" disabled={!candidate || items.includes(candidate) || items.length>=12} onClick={()=>candidate && save([...items,candidate])}>{translate("保存当前网址")}</button>
    {candidate && <p><small>{candidate}</small></p>}
    {items.map(url=><div className="bookmark-row" key={url}><button type="button" onClick={()=>onOpen(url)}>{url}</button><button type="button" aria-label={translate("移除常用地址 {0}",[url])} onClick={()=>save(items.filter(i=>i!==url))}>{translate("移除")}</button></div>)}
    {error && <p role="alert">{error}</p>}
  </details>;
}
