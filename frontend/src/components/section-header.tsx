import { Children, isValidElement, type ReactNode } from "react";
import { Icon, type IconName } from "./icon";

/** One heading, one action area, and one icon language across all modules. */
export function SectionHeader({ icon, children, className = "card-heading" }: { icon: IconName; children: ReactNode; className?: string }) {
  return <div className={className}>{Children.map(children, child =>
    isValidElement(child) && child.type === "h2"
      ? <div className="section-title"><Icon name={icon} size={20} />{child}</div>
      : child
  )}</div>;
}
