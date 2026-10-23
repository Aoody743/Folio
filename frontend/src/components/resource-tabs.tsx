"use client";
import { Children, Fragment, isValidElement, type ReactElement, type ReactNode } from "react";
import { TabList, Tab } from "@astryxdesign/core/TabList";
import { Icon, type IconName } from "./icon";
const tabIcons: Record<string, IconName> = {
  panel: "browser", "firefox-wasm": "sites",
  installed: "server", catalog: "package", applications: "software", browsers: "browser",
  database: "database", cache: "cache", connections: "network", websites: "sites",
  status: "overview", data: "database",
  sites: "sites", certificates: "shield", retained: "backup",
  static: "files", php: "code", git: "git", proxy: "network",
  overview: "overview", config: "settings", logs: "logs", backup: "backup",
  source: "key", instances: "server", operations: "settings", domains: "sites",
  tls: "lock", runtime: "terminal", containers: "package", all: "overview",
  branding: "settings", policy: "shield", gateway: "lock", temporary: "clock",
  bookmarks: "bookmark", databases: "database", users: "shield", history: "history",
};
type Entry = { children?: ReactNode; "aria-selected"?: boolean; disabled?: boolean; onClick?: () => void };
function entries(children: ReactNode): ReactElement<Entry>[] {
  return Children.toArray(children).flatMap(child => {
    if (!isValidElement<Entry>(child)) return [];
    return child.type === Fragment ? entries(child.props.children) : [child];
  });
}
/** Modules own resource state; Astryx owns the shared tab behavior. */
export function ResourceTabs({ children, "aria-label": label }: { children: ReactNode; "aria-label"?: string }) {
  const items = entries(children);
  const values = items.map((item, index) => String(item.key ?? index));
  const value = values[items.findIndex(item => item.props["aria-selected"])] ?? values[0] ?? "";
  return <TabList className="resource-tabs" role="tablist" aria-label={label} value={value}
    overflow="scroll" hasDivider onChange={next => { const entry = items[values.indexOf(next)]; if (!entry?.props.disabled) entry?.props.onClick?.(); }}>
    {items.map((item, index) => {
      const key = String(item.key ?? "").replace(/^.*\$/, "");
      const icon = tabIcons[key];
      return <Tab key={values[index]} value={values[index]} label={item.props.children as string}
        aria-disabled={item.props.disabled || undefined} icon={icon ? <Icon name={icon} size={18} /> : undefined} />;
    })}
  </TabList>;
}
