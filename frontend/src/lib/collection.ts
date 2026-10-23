"use client";
import { useEffect, useRef, useState } from "react";
export function pageItems<T>(items: T[], offset: number, limit: number) {
  const safe = Math.min(
    Math.floor(Math.max(0, offset) / limit) * limit,
    Math.max(0, Math.ceil(items.length / limit) - 1) * limit,
  );
  return {
    items: items.slice(safe, safe + limit),
    offset: safe,
    total: items.length,
  };
}
// Desktop pages fit the available height. Phones keep at least four rows and
// allow natural scrolling, so a tall toolbar does not force one-item pages.
export function usePageSize(rowHeight = 64, columns: number | "auto" = 1) {
  const ref = useRef<HTMLDivElement>(null);
  const [limit, setLimit] = useState(4 * (columns === "auto" ? 1 : columns));
  const measured = useRef({
    row: rowHeight,
    header: 44,
    reserve: 200,
    width: 0,
  });
  useEffect(() => {
    const measure = () => {
      if (!ref.current || !ref.current.getClientRects().length) return;
      const visibleRows = [
        ...ref.current.querySelectorAll(
          "tbody tr, .software-card, .instance-row, .action-tile, .resource-summary-card",
        ),
      ];
      // A server-page change temporarily removes rows. Keep the last measured
      // geometry until the response arrives instead of resizing from an empty table.
      if (!visibleRows.length) return;
      const height = window.visualViewport?.height ?? window.innerHeight;
      if (measured.current.width !== window.innerWidth)
        measured.current = {
          row: rowHeight,
          header: 44,
          reserve: 200,
          width: window.innerWidth,
        };
      const main = document.querySelector(".main");
      const card = ref.current.closest(".panel-card");
      const pager = card?.querySelector(".pagination");
      const padding = (
        element: Element | null | undefined,
        property: string,
      ) =>
        element
          ? parseFloat(getComputedStyle(element).getPropertyValue(property)) ||
            0
          : 0;
      const reserve = Math.max(
        180,
        padding(main, "padding-bottom") +
          padding(card, "padding-bottom") +
          (pager?.getBoundingClientRect().height ?? 64) +
          padding(pager, "margin-top") +
          16,
      );
      measured.current.reserve = Math.max(measured.current.reserve, reserve);
      const available =
        height -
        ref.current.getBoundingClientRect().top -
        measured.current.reserve;
      measured.current.row = Math.max(
        measured.current.row,
        rowHeight,
        ...visibleRows.map((row) => row.getBoundingClientRect().height),
      );
      const actualHeader = ref.current
        .querySelector("thead")
        ?.getBoundingClientRect().height;
      if (actualHeader)
        measured.current.header = Math.max(
          measured.current.header,
          actualHeader,
        );
      const actualRowHeight = measured.current.row;
      const header = measured.current.header;
      const columnCount = columns === "auto"
        ? Math.max(1, getComputedStyle(ref.current).gridTemplateColumns.split(/\s+/).length)
        : columns;
      setLimit(
        Math.max(window.innerWidth <= 700 ? 4 : 1, Math.floor((available - header) / actualRowHeight)) * columnCount,
      );
    };
    measure();
    const observer = new ResizeObserver(measure);
    if (ref.current) observer.observe(ref.current);
    const main = document.querySelector(".main");
    if (main) observer.observe(main);
    window.addEventListener("resize", measure);
    window.visualViewport?.addEventListener("resize", measure);
    return () => {
      observer.disconnect();
      window.removeEventListener("resize", measure);
      window.visualViewport?.removeEventListener("resize", measure);
    };
  }, [rowHeight, columns]);
  return { ref, limit };
}
