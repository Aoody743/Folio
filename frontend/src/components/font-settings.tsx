"use client";
import { t as translate } from "@/lib/i18n";
import { setFont, useFont, FontPreset } from "@/lib/fonts";
export function FontSettings() {
  const selected = useFont();
  const presets: { id: FontPreset; name: string; family: string }[] = [
    {
      id: "modern",
      name: translate("现代清晰"),
      family: "Inter · Noto Sans SC",
    },
    {
      id: "native",
      name: translate("系统原生"),
      family: translate("系统 UI 与中文字体"),
    },
    {
      id: "reading",
      name: translate("书面阅读"),
      family: "Source Sans 3 · Noto Serif SC",
    },
  ];
  return (
    <section className="font-settings">
      <h2>{translate("字体")}</h2>
      <div
        className="font-options"
        role="group"
        aria-label={translate("字体预设")}
      >
        {presets.map((preset) => (
          <button
            key={preset.id}
            data-font-preset={preset.id}
            className={`font-option ${selected === preset.id ? "selected" : ""}`}
            aria-pressed={selected === preset.id}
            onClick={() => setFont(preset.id)}
          >
            <strong>{preset.name}</strong>
            <span>{preset.family}</span>
          </button>
        ))}
      </div>
      <div className="font-live-preview">
        <p lang="zh-CN">清楚看状态，轻松做管理。</p>
        <p lang="en">Folio · A clearer view of your server.</p>
      </div>
      <p className="muted">
        {translate("字体保存在此浏览器，立即应用并保留当前输入。")}
      </p>
    </section>
  );
}
