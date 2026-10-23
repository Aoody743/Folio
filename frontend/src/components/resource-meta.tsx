import type { ReactNode } from "react";
import { Icon, type IconName } from "./icon";

export function ResourceMeta({ items }: { items: { label: string; icon: IconName; value: ReactNode; detail?: ReactNode }[] }) {
  return <dl className="resource-meta">{items.map(item => <div key={item.label}>
    <dt><Icon name={item.icon} size={18} /><span>{item.label}</span></dt>
    <dd>{item.value}{item.detail && <small>{item.detail}</small>}</dd>
  </div>)}</dl>;
}
