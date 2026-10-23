"use client";
import { SectionHeader } from "./section-header";
import { Icon } from "./icon";
import { ResourceTabs } from "./resource-tabs";
import { ResourceError } from "./resource-ui";
import { useEffect, useState } from "react";
import { wasmCompatibility } from "@/lib/browser-usability";
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
  navigate("software", { page: "browsers" });
const wasmURL = "/api/v1/browser/firefox-wasm/";
function FirefoxLaunch() {
  const [state,setState]=useState<"checking"|"ready"|"insecure"|"unsupported">("checking");
  useEffect(()=>setState(wasmCompatibility(window.isSecureContext,window.WebAssembly)),[]);
  return <div className="wasm-compatibility">
    {state==="ready" ? <><span className="browser-ready" role="status"><Icon name="check" size={16} />{translate("设备兼容")}</span><a className="button-link" href={wasmURL} target="_blank" rel="noopener noreferrer">{translate("打开 Firefox")}</a></> : <p role="status" className="notice warning">{state==="checking"?translate("正在检查设备兼容性…"):state==="insecure"?translate("请通过 HTTPS 访问面板，再打开 Firefox WASM。"):translate("此设备不支持 JSPI，请升级 Chrome/Edge，或改用面板浏览器。")}</p>}
  </div>;
}


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
      <SectionHeader icon="browser" className="card-heading">
        <h2>{translate("浏览器应用")}</h2>
      </SectionHeader>
      <p className="section-caption">{translate("两个模式均使用服务器网络，按需安装。")}</p>
      {apps.error && <ResourceError error={apps.error} state={apps.state} retry={() => void apps.reload()} />}
      {!apps.data && !apps.error && (
        <p role="status">{translate("正在读取…")}</p>
      )}
      <div className="browser-app-grid">
        {apps.data?.items.map((app) => (
          <section key={app.id} className="browser-app-row resource-summary-card">
            <div>
              <div className="browser-app-title"><span className="heading-icon"><Icon name={app.id === "panel" ? "browser" : "sites"} /></span><h3>{name(app.id)}</h3></div>
              <p>
                {app.id === "panel"
                  ? translate(
                      "服务器端 Chromium，支持手机和常用浏览器。",
                    )
                  : translate(
                      "完整 Firefox 界面，需要 HTTPS 和支持 JSPI 的 Chrome / Edge。",
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
                    variant="primary"
                    icon={<Icon name="external" size={18} />}
                    onClick={() =>
                      navigate("more", { page: "browser", kind: "panel" })
                    }
                  />
                ) : (
                  <FirefoxLaunch/>
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
                  variant={app.installed ? "ghost" : "primary"}
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
    <div className="stack browser-hub">
      <div className="browser-mode-bar">
      {installed.length > 0 && (
        <ResourceTabs
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
        </ResourceTabs>
      )}

        <Button
          label={translate("管理浏览器应用")}
          isIconOnly
          icon={<Icon name="settings" size={18} />}
          variant="ghost"
          onClick={appsRoute}
        />
      </div>
      {apps.error && <ResourceError error={apps.error} state={apps.state} retry={() => void apps.reload()} />}
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
      {selected?.id === "panel" && <ServerBrowserView active={active} />}
      {selected?.id === "firefox-wasm" && (
        <Card className="panel-card">
          <h3>Firefox WASM</h3>
          <p>
            {translate("在独立窗口使用完整 Firefox，网页流量走本服务器。")}
          </p>
          <details className="browser-help"><summary><Icon name="help" size={18} />{translate("使用帮助")}</summary><p className="muted">{translate("需要 HTTPS 和支持 JSPI 的浏览器。Firefox 中的网站内容和按钮语言由上游 Firefox 决定。")}</p></details>
          {selected.available ? <FirefoxLaunch/> : <p className="notice warning">{translate("浏览器组件暂不可用，请到应用中检查安装状态。")}</p>}
        </Card>
      )}
    </div>
  );
}
