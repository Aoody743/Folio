"use client";
import {
  Children,
  cloneElement,
  isValidElement,
  ReactNode,
  useState,
} from "react";
import { pageItems, usePageSize } from "@/lib/collection";
import { Pager } from "./resource-ui";
// Complete client collections share the same viewport-sized table/pager contract.
// Server-paginated website and file lists retain their own request pagination.
export function ClientTable({ children }: { children: ReactNode }) {
  const [offset, setOffset] = useState(0);
  const { ref, limit } = usePageSize(76);
  let total = 0,
    count = 0,
    currentOffset = 0;
  const table = Children.map(children, (child) => {
    if (
      !isValidElement<{ children: ReactNode }>(child) ||
      child.type !== "table"
    )
      return child;
    return cloneElement(
      child,
      {},
      Children.map(child.props.children, (part) => {
        if (
          !isValidElement<{ children: ReactNode }>(part) ||
          part.type !== "tbody"
        )
          return part;
        const rows = Children.toArray(part.props.children);
        const page = pageItems(rows, offset, limit);
        total = page.total;
        count = page.items.length;
        currentOffset = page.offset;
        return cloneElement(part, {}, page.items);
      }),
    );
  });
  return (
    <>
      <div ref={ref} className="resource-table-wrap">
        {table}
      </div>
      {total > 0 && (
        <Pager
          offset={currentOffset}
          limit={limit}
          total={total}
          count={count}
          loading={false}
          onOffset={setOffset}
        />
      )}
    </>
  );
}
