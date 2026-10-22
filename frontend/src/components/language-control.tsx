"use client";
import { t as translate } from "@/lib/i18n";
import { useSyncExternalStore } from "react";
import {
  getLocale,
  serverLocale,
  setLocale,
  subscribeLocale,
} from "@/lib/i18n";

export function LanguageControl() {
  const locale = useSyncExternalStore(subscribeLocale, getLocale, serverLocale);
  return (
    <label className="language-control">
      <span aria-hidden="true">{translate("文 / A")}</span>
      <select
        aria-label={translate("语言 / Language")}
        value={locale}
        onChange={(event) =>
          setLocale(event.target.value === "en" ? "en" : "zh-CN")
        }
      >
        <option value="zh-CN">{translate("中文")}</option>
        <option value="en">English</option>
      </select>
    </label>
  );
}
