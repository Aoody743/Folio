"use client";
import {
  useCallback,
  useEffect,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";

export type Section = "overview" | "sites" | "software" | "files" | "more";
export type PageState =
  | "loading"
  | "ready"
  | "empty"
  | "forbidden"
  | "unconfigured"
  | "error"
  | "unknown";
export type PageRoute = { section: Section; params: Record<string, string> };
const sections = new Set(["overview", "sites", "software", "files", "more"]);
// Only view state is addressable. Form values, SQL and credentials never enter URLs.
const keys = new Set([
  "page",
  "q",
  "offset",
  "category",
  "status",
  "id",
  "group",
  "create",
  "root",
  "path",
  "file",
  "view",
  "filter",
  "instance",
  "kind",
  "resource",
  "new",
  "subgroup",
]);
export function parseRoute(hash: string): PageRoute {
  const [path, query = ""] = hash.replace(/^#\/?/, "").split("?", 2);
  const section = sections.has(path) ? (path as Section) : "overview";
  return {
    section,
    params: Object.fromEntries(
      [...new URLSearchParams(query)].filter(([k]) => keys.has(k)),
    ),
  };
}
export function routeHash(route: PageRoute): string {
  const query = new URLSearchParams(
    Object.entries(route.params).filter(([k, v]) => keys.has(k) && v !== ""),
  );
  return `#/${route.section}${query.size ? `?${query}` : ""}`;
}
const listeners = new Set<() => void>();
const remembered: Partial<Record<Section, Record<string, string>>> = {};
const emit = () => listeners.forEach((fn) => fn());
function snapshot() {
  return typeof location === "undefined" ? "#/overview" : location.hash;
}
// Folio owns a single static page with hash routes. Next patches the instance
// history methods and asynchronously restores its own canonical URL; using
// those methods can roll a newer hash back to an older route.
function writeHistory(method: "pushState" | "replaceState", hash: string) {
  const native = typeof History !== "undefined" ? History.prototype[method] : history[method];
  native.call(history, { folio: true }, "", hash);
}
export function replaceNavigationHash(hash: string) {
  writeHistory("replaceState", hash);
  acceptedHash = snapshot();
  const route = parseRoute(acceptedHash);
  remembered[route.section] = route.params;
  emit();
}

type NavigationGuard = (next: PageRoute, current: PageRoute) => boolean;
const guards = new Set<NavigationGuard>();
let acceptedHash: string | undefined;
export function addNavigationGuard(guard: NavigationGuard) {
  guards.add(guard);
  return () => {
    guards.delete(guard);
  };
}
function permitted(next: PageRoute, current: PageRoute) {
  if ([...guards].every((guard) => guard(next, current))) return true;
  queueMicrotask(() =>
    window.dispatchEvent(new Event("folio:navigation-blocked")),
  );
  return false;
}
function changed() {
  const next = snapshot();
  if (acceptedHash && !permitted(parseRoute(next), parseRoute(acceptedHash)))
    writeHistory("replaceState", acceptedHash);
  else {
    if (acceptedHash) {
      const previous = parseRoute(acceptedHash);
      remembered[previous.section] = previous.params;
    }
    const route = parseRoute(next);
    remembered[route.section] = route.params;
    acceptedHash = next;
  }
  emit();
}
function popped(event: PopStateEvent) {
  // Keep Next's popstate restore/reload handler out of this static hash router.
  event.stopImmediatePropagation();
  changed();
}
function subscribe(fn: () => void) {
  if (!listeners.size) {
    acceptedHash = snapshot();
    window.addEventListener("hashchange", changed);
    window.addEventListener("popstate", popped, true);
  }
  listeners.add(fn);
  return () => {
    listeners.delete(fn);
    if (!listeners.size) {
      window.removeEventListener("hashchange", changed);
      window.removeEventListener("popstate", popped, true);
    }
  };
}
export function navigate(
  section: Section,
  params?: Record<string, string>,
  replace = false,
) {
  const current = parseRoute(snapshot());
  remembered[current.section] = current.params;
  const next = { section, params: params ?? remembered[section] ?? {} };
  const hash = routeHash(next);
  if (hash === snapshot()) return;
  if (typeof window !== "undefined" && !permitted(next, current)) return;
  writeHistory(replace ? "replaceState" : "pushState", hash);
  acceptedHash = hash;
  remembered[next.section] = next.params;
  emit();
}
export function resetNavigation() {
  for (const section of Object.keys(remembered))
    delete remembered[section as Section];
  acceptedHash = "#/overview";
  writeHistory("replaceState", acceptedHash);
  emit();
}
export function updateRoute(
  section: Section,
  key: string,
  value: string,
  push = false,
) {
  const route = parseRoute(snapshot());
  if (route.section !== section) return;
  const params = { ...route.params };
  if (value) params[key] = value;
  else delete params[key];
  navigate(section, params, !push);
}
export function useRoute() {
  const hash = useSyncExternalStore(subscribe, snapshot, () => "#/overview");
  return parseRoute(hash);
}
type Wide<T> = T extends string ? string : T extends number ? number : boolean;
export function usePageState<T extends string | number | boolean>(
  section: Section,
  key: string,
  initial: T,
  push = false,
): [Wide<T>, (value: Wide<T> | ((previous: Wide<T>) => Wide<T>)) => void] {
  const route = useRoute();
  const decode = useCallback(
    (raw: string | undefined): Wide<T> => {
      if (raw === undefined) return initial as unknown as Wide<T>;
      if (typeof initial === "number")
        return (Number.isFinite(+raw) && +raw >= 0 ? +raw : initial) as Wide<T>;
      if (typeof initial === "boolean") return (raw === "true") as Wide<T>;
      return raw as Wide<T>;
    },
    [initial],
  );
  const [local, setLocal] = useState<Wide<T>>(() =>
    route.section === section
      ? decode(route.params[key])
      : (initial as unknown as Wide<T>),
  );
  const value = route.section === section ? decode(route.params[key]) : local;
  const current = useRef(value);
  current.current = value;
  useEffect(() => {
    if (route.section === section) setLocal(decode(route.params[key]));
  }, [section, key, route.section, route.params[key], decode]);
  const set = useCallback(
    (next: Wide<T> | ((previous: Wide<T>) => Wide<T>)) => {
      const result = typeof next === "function" ? next(current.current) : next;
      current.current = result;
      setLocal(result);
      updateRoute(
        section,
        key,
        String(result) === String(initial) ? "" : String(result),
        push,
      );
    },
    [section, key, initial, push],
  );
  return [value, set];
}
