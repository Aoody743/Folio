import { english } from "./messages.en";

export type Locale = "zh-CN" | "en";
export const localeKey = "folio.language";
let locale: Locale = "zh-CN";
const listeners = new Set<() => void>();
export const getLocale = () => locale;
export const serverLocale = (): Locale => "zh-CN";
export function subscribeLocale(listener: () => void) {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}
export function setLocale(value: Locale, persist = true) {
  if (typeof document !== "undefined") {
    document.documentElement.lang = value;
    if (
      ["", "Folio", "折纸面板", "Folio · Server Panel", "Folio · 折纸面板"].includes(document.title)
    ) {
      document.title =
        value === "en" ? "Folio" : "折纸面板";
    }
    if (persist) {
      try {
        localStorage.setItem(localeKey, value);
      } catch {
        /* Storage is optional. */
      }
    }
  }
  if (locale === value) return;
  locale = value;
  listeners.forEach((listener) => listener());
}
export function initializeLocale() {
  let saved: string | null = null;
  try {
    saved = localStorage.getItem(localeKey);
  } catch {
    /* Use browser language. */
  }
  const detected =
    saved === "en" || saved === "zh-CN"
      ? saved
      : navigator.language.toLowerCase().startsWith("zh")
        ? "zh-CN"
        : "en";
  setLocale(detected, false);
}
/** Only application copy is translated. Interpolated resource names and data stay verbatim. */
export function t(message: string, values: readonly unknown[] = []): string {
  const translated = locale === "en" ? (english[message] ?? message) : message;
  return translated.replace(/\{(\d+)\}/g, (match, index: string) =>
    Number(index) < values.length ? String(values[Number(index)]) : match,
  );
}
