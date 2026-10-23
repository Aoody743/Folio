"use client";
import { useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { action, Capability, intentKey } from "@/lib/api";
import { t as translate } from "@/lib/i18n";
import { navigate, usePageState } from "@/lib/navigation";
import { useResource } from "@/lib/resource-hook";
import dynamic from "next/dynamic";
const ServerBrowserView = dynamic(
  () => import("./server-browser").then((m) => m.ServerBrowserView),
  { ssr: false },
);
import "./browser.css";

type BrowserApp = {
  id: "panel" | "firefox-wasm";
  installed: boolean;
  available: boolean;
  reason?: string;
  revision: number;
  version?: string;
  task_id?: string;
  task_status?: string;
};
const name = (id: BrowserApp["id"]) =>
  id === "panel" ? translate("面板浏览器") : "Firefox WASM";
const pending = (app: BrowserApp) =>
  ["queued", "running"].includes(app.task_status || "");
const appsRoute = () =>
  navigate("software", { page: "applications", subgroup: "browsers" });
const wasmURL = "/api/v1/browser/firefox-wasm/";

export function BrowserApps({
  capabilities,
  active,
  onExecuted,
}: {
  capabilities: Capability[];
  active: boolean;
  onExecuted: () => void;
}) {
  const allowed = (action: string) =>
    capabilities.some((c) => c.name === action);
  const apps = useResource<{ items: BrowserApp[] }>(
    "browser_apps_list",
    {},
    active && allowed("browser_apps_list"),
    5000,
  );
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [submitted, setSubmitted] = useState(false);
  const refresh = () => {
    void apps.reload();
    window.dispatchEvent(new Event("powerlinux:invalidate"));
    onExecuted();
  };
  const change = async (app: BrowserApp) => {
    const operation = app.installed ? "uninstall" : "install";
    if (
      app.installed &&
      !window.confirm(
        name(app.id) + "\n" + translate(
          "卸载将关闭此模式的所有浏览器连接，并删除面板下载的组件。系统浏览器和另一模式不受影响。是否继续？",
        ),
      )
    )
      return;
    setBusy(app.id);
    setError("");
    setSubmitted(false);
    try {
      await action(
        "browser_app_change",
        { id: app.id, operation, expected_revision: app.revision },
        { idempotencyKey: intentKey() },
      );
      setSubmitted(true);
      refresh();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy("");
    }
  };
  if (!allowed("browser_apps_list")) return null;
  return (
    <Card className="panel-card">
      <div className="card-heading">
        <h2>{translate("浏览器应用")}</h2>
      </div>
      <p className="muted">
        {translate(
          "按需安装，两个模式均通过服务器网络访问网页。可以都不安装，也可以同时安装。",
        )}
      </p>
      {apps.error && <p role="alert">{apps.error}</p>}
      {!apps.data && !apps.error && (
        <p role="status">{translate("正在读取…")}</p>
      )}
      <div className="stack">
        {apps.data?.items.map((app) => (
          <section key={app.id} className="browser-app-row">
            <div>
              <h3>{name(app.id)}</h3>
              <p>
                {app.id === "panel"
                  ? translate(
                      "Chromium 在服务器运行，兼容手机与常用浏览器。优先复用系统浏览器；未安装时下载独立引擎（Linux x64）。",
                    )
                  : translate(
                      "完整 Firefox 界面在当前设备运行，网络通过本服务器转发。需要 HTTPS、新版 Chrome/Edge 和 JSPI，首次加载较大。",
                    )}
              </p>
              <p className="muted" role="status">
                {pending(app)
                  ? translate("正在处理，请查看任务进度。")
                  : app.installed
                    ? translate("已安装")
                    : translate("未安装")}
                {app.installed && app.version ? ` · ${app.version}` : ""}
              </p>
              {!app.available && (
                <div>
                  <p>
                    {translate("浏览器环境暂不可用，请查看详情或重新安装。")}
                  </p>
                  <details>
                    <summary>{translate("技术详情")}</summary>
                    {app.reason}
                  </details>
                </div>
              )}
            </div>
            <div className="button-row">
              {app.installed &&
                app.available &&
                !pending(app) &&
                (app.id === "panel" ? (
                  <Button
                    label={translate("打开浏览器")}
                    onClick={() =>
                      navigate("more", { page: "browser", kind: "panel" })
                    }
                  />
                ) : (
                  <a
                    className="button-link"
                    href={wasmURL}
                    target="_blank"
                    rel="noopener noreferrer"
                  >
                    {translate("打开 Firefox")}
                  </a>
                ))}
              {allowed("browser_app_change") && (
                <Button
                  label={
                    pending(app)
                      ? translate("正在处理…")
                      : app.installed
                        ? translate("卸载")
                        : translate("安装")
                  }
                  isDisabled={
                    !!busy || pending(app) || (!app.available && !app.installed)
                  }
                  onClick={() => void change(app)}
                />
              )}
              {app.task_id && (
                <Button
                  label={translate("查看任务")}
                  variant="ghost"
                  onClick={() =>
                    navigate("more", {
                      page: "activity",
                      kind: "task",
                      id: app.task_id!,
                    })
                  }
                />
              )}
            </div>
          </section>
        ))}
      </div>
      {error && <p role="alert">{error}</p>}
      {submitted && (
        <p role="status">{translate("任务已提交，可从对应应用查看进度。")}</p>
      )}
    </Card>
  );
}
export function BrowserHub({ active = true }: { active?: boolean }) {
  const apps = useResource<{ items: BrowserApp[] }>(
    "browser_apps_list",
    {},
    active,
    5000,
  );
  const [mode, setMode] = usePageState("more", "kind", "panel");
  const installed = apps.data?.items.filter((a) => a.installed) || [];
  const selected = installed.find((a) => a.id === mode) || installed[0];
  return (
    <div className="stack">
      <div className="card-heading">
        <h2>{translate("浏览器")}</h2>
        <Button
          label={translate("管理浏览器应用")}
          variant="ghost"
          onClick={appsRoute}
        />
      </div>
      {apps.error && <p role="alert">{apps.error}</p>}
      {!apps.data && !apps.error && (
        <p role="status">{translate("正在读取…")}</p>
      )}
      {apps.data && !installed.length && (
        <Card className="panel-card">
          <p>{translate("安装一个浏览器模式，即可通过服务器网络浏览网页。")}</p>
          <Button
            label={translate("前往应用安装")}
            variant="primary"
            onClick={appsRoute}
          />
        </Card>
      )}
      {installed.length > 1 && (
        <div
          className="section-tabs"
          role="tablist"
          aria-label={translate("浏览器模式")}
        >
          {installed.map((a) => (
            <button
              key={a.id}
              role="tab"
              aria-selected={selected?.id === a.id}
              onClick={() => setMode(a.id)}
            >
              {name(a.id)}
            </button>
          ))}
        </div>
      )}
      {selected?.id === "panel" && <ServerBrowserView active={active} />}
      {selected?.id === "firefox-wasm" && (
        <Card className="panel-card">
          <h3>Firefox WASM</h3>
          <p>
            {translate(
              "在独立窗口使用完整 Firefox 界面，网页流量走本服务器。关闭窗口即可断开网络连接。",
            )}
          </p>
          <p className="muted">
            {translate(
              "需要 HTTPS 和支持 JSPI 的浏览器。Firefox 中的网站内容和按钮语言由上游 Firefox 决定。",
            )}
          </p>
          <a
            className="button-link"
            href={wasmURL}
            target="_blank"
            rel="noopener noreferrer"
          >
            {translate("打开 Firefox")}
          </a>
        </Card>
      )}
    </div>
  );
}
