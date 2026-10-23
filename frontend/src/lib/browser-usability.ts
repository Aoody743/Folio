export function wasmCompatibility(secure:boolean,api:unknown):"ready"|"insecure"|"unsupported" {
  if(!secure)return "insecure";
  const value=api as {Suspending?:unknown;promising?:unknown}|undefined;
  return typeof value?.Suspending==="function" && typeof value?.promising==="function"?"ready":"unsupported";
}
// Bookmarks deliberately omit credentials, query strings and fragments. They
// are explicit local preferences; browser history is never recorded here.
export function bookmarkURL(raw:string):string|undefined {
  try{const url=new URL(raw);if(!["http:","https:"].includes(url.protocol) || url.username || url.password)return undefined;
    const safe=url.origin+url.pathname;return safe.length<=2048?safe:undefined;
  }catch{return undefined;}
}
export function readBookmarks(value:unknown):string[] {
  if(!Array.isArray(value))return [];
  return [...new Set(value.filter((v):v is string=>typeof v==="string").map(bookmarkURL).filter((v):v is string=>!!v))].slice(0,12);
}
