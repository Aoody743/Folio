"use client";
import { useSyncExternalStore } from "react";
export type FontPreset = "modern" | "native" | "reading";
const valid = (value: unknown): value is FontPreset =>
  ["modern", "native", "reading"].includes(String(value));
let current: FontPreset = "modern";
const listeners = new Set<() => void>();
export function setFont(preset: FontPreset) {
  if (!valid(preset)) return;
  current = preset;
  document.documentElement.dataset.font = preset;
  try {
    localStorage.setItem("folio.font", preset);
  } catch {}
  listeners.forEach((fn) => fn());
}
export function initializeFont() {
  let value: string | null = null;
  try {
    value = localStorage.getItem("folio.font");
  } catch {}
  setFont(valid(value) ? value : "modern");
}
export function useFont() {
  return useSyncExternalStore(
    (fn) => {
      listeners.add(fn);
      return () => {
        listeners.delete(fn);
      };
    },
    () => current,
    () => "modern" as FontPreset,
  );
}
