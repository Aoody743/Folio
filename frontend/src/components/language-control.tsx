"use client";
import { t as translate } from "@/lib/i18n";
import { useSyncExternalStore } from "react";
import { Selector } from "@astryxdesign/core/Selector";
import {
  getLocale,
  serverLocale,
  setLocale,
  subscribeLocale,
} from "@/lib/i18n";

export function LanguageControl() {
  const locale = useSyncExternalStore(subscribeLocale, getLocale, serverLocale);
  return (
    <div className="language-control">
      <Selector
        label={translate("语言 / Language")}
        width="100%"
        isLabelHidden
        value={locale}
        options={[{ value: "zh-CN", label: "中文" }, { value: "en", label: "English" }]}
        onChange={(value) => setLocale(value === "en" ? "en" : "zh-CN")}
        presentation="popover"
        placement="above"
      />
    </div>
  );
}
