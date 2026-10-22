"use client";
import { t as translate } from "@/lib/i18n";
import { useState } from "react";
import { Card } from "@astryxdesign/core/Card";
import { Button } from "@astryxdesign/core/Button";
import { Capability, fullDate, list } from "@/lib/api";
import { Page } from "@/lib/resources";
import { useResource } from "@/lib/resource-hook";
import { ActionForm } from "./actions";
import { Pager, ResourceActions, State, Unavailable } from "./resource-ui";
type Settings = {
  panel_name: string;
  footer: string;
  login_entry: string;
  captcha_enabled: boolean;
  api_enabled: boolean;
  maintenance: boolean;
  revision: number;
  basic_auth_enabled?: boolean;
  basic_auth_username?: string;
  unauthorized_status?: number;
};
export function PanelSettings({
  capabilities,
  onExecuted,
}: {
  capabilities: Capability[];
  onExecuted: () => void;
}) {
  const settings = useResource<Settings>(
    "settings_panel_get",
    {},
    capabilities.some((c) => c.name === "settings_panel_get"),
  );
  const [tab, setTab] = useState("branding");
  const refresh = () => {
    void settings.reload();
    onExecuted();
  };
  const action = capabilities.find(
    (c) =>
      c.name ===
      `settings_${tab === "branding" ? "branding" : tab === "gateway" ? "basic_auth" : "security"}_update`,
  );
  const preset = settings.data
    ? tab === "gateway"
      ? {
          enabled: settings.data.basic_auth_enabled,
          basic_username: settings.data.basic_auth_username,
          expected_revision: settings.data.revision,
        }
      : { ...settings.data, expected_revision: settings.data.revision }
    : {};
  return (
    <Card className="panel-card">
      <div className="card-heading">
        <h2>{translate("面板与访问设置")}</h2>
      </div>
      <div
        className="section-tabs"
        role="tablist"
        aria-label={translate("面板访问设置分类")}
      >
        {[
          { id: "branding", label: translate("名称与页脚") },
          { id: "policy", label: translate("入口与访问") },
          ...(capabilities.some((c) => c.name === "settings_basic_auth_update")
            ? [{ id: "gateway", label: translate("浏览器入口密码") }]
            : []),
          { id: "applications", label: translate("应用授权") },
          { id: "temporary", label: translate("临时入口") },
          { id: "bookmarks", label: translate("快捷链接") },
        ].map((item) => (
          <button
            role="tab"
            key={item.id}
            aria-selected={tab === item.id}
            onClick={() => {
              setTab(item.id);
            }}
          >
            {item.label}
          </button>
        ))}
      </div>
      {settings.error && (
        <div className="notice danger" role="alert">
          {settings.error}
        </div>
      )}
      {["branding", "policy", "gateway"].includes(tab) ? (
        action && settings.data ? (
          <>
            <p className="section-caption">
              {tab === "branding"
                ? translate(
                    "名称与页脚按服务器版本保存，网页和MCP读取同一配置。",
                  )
                : tab === "gateway"
                  ? translate(
                      "浏览器入口密码通过浏览器认证对话框验证，之后仍需登录面板账户。启用或修改凭据需要确认当前面板密码与两步验证码。",
                    )
                  : translate(
                      "访问策略更改需要确认密码与两步验证码。入口链接使用 #login_entry=名称，入口仅在浏览器内存中保留。",
                    )}
            </p>
            <ActionForm
              capability={action}
              preset={preset}
              onExecuted={refresh}
            />
          </>
        ) : (
          <Unavailable title={translate("当前没有此设置的读写权限")} />
        )
      ) : (
        <IdentityResources
          key={tab}
          kind={tab}
          capabilities={capabilities}
          onExecuted={refresh}
        />
      )}
    </Card>
  );
}
type Identity = {
  id: string;
  name: string;
  revision?: number;
  url?: string;
  description?: string;
  enabled?: boolean;
  revoked?: boolean;
  consumed?: boolean;
  expires_at?: string;
  scopes?: string[];
  resources?: string[];
};
function externalURL(value?: string) {
  if (!value) return;
  try {
    const parsed = new URL(value);
    return ["http:", "https:"].includes(parsed.protocol) &&
      !parsed.username &&
      !parsed.password
      ? parsed.href
      : undefined;
  } catch {
    return;
  }
}
function IdentityResources({
  kind,
  capabilities,
  onExecuted,
}: {
  kind: string;
  capabilities: Capability[];
  onExecuted: () => void;
}) {
  const prefix =
    kind === "applications"
      ? "applications"
      : kind === "temporary"
        ? "temporary_grants"
        : "panel_bookmarks";
  const has = (name: string) => capabilities.some((c) => c.name === name);
  const [selected, setSelected] = useState<string>();
  const [operation, setOperation] = useState<string>();
  const [create, setCreate] = useState(false);
  const [offset, setOffset] = useState(0);
  const records = useResource<Page<Identity>>(
    prefix + "_list",
    {},
    has(prefix + "_list"),
  );
  const rows = list(records.data);
  const current = rows.find((row) => row.id === selected);
  const createAction = capabilities.find(
    (c) => c.name === prefix + (kind === "bookmarks" ? "_save" : "_create"),
  );
  const refresh = () => {
    void records.reload();
    onExecuted();
  };
  return (
    <div className="stack">
      <div className="button-row">
        {createAction && (
          <Button
            label={
              kind === "applications"
                ? translate("创建应用授权")
                : kind === "temporary"
                  ? translate("创建临时登录入口")
                  : translate("添加快捷链接")
            }
            onClick={() => {
              setCreate(!create);
              setOperation(undefined);
            }}
          />
        )}
        {has(prefix + "_logs") && (
          <Button
            label={translate("读取临时入口登录记录")}
            onClick={() => {
              setOperation(prefix + "_logs");
              setCreate(false);
            }}
          />
        )}
      </div>
      {records.error && (
        <div className="notice danger" role="alert">
          {records.error}
        </div>
      )}
      {!has(prefix + "_list") ? (
        <Unavailable title={translate("当前没有列表读取权限")} />
      ) : (
        <>
          <div className="instance-list">
            {rows.slice(offset, offset + 20).map((row) => (
              <div className="identity-row" key={row.id}>
                <button
                  className="instance-row"
                  aria-pressed={selected === row.id}
                  onClick={() => {
                    setSelected(row.id);
                    setOperation(undefined);
                    setCreate(false);
                  }}
                >
                  <div>
                    <strong>{row.name}</strong>
                    <p>{row.description ?? row.scopes?.join(" · ") ?? ""}</p>
                    {row.expires_at && (
                      <small>
                        {translate("到期 ")}
                        {fullDate(row.expires_at)}
                      </small>
                    )}
                  </div>
                  {row.revoked !== undefined ? (
                    <span>
                      {row.revoked
                        ? translate("已撤销")
                        : row.consumed
                          ? translate("已使用")
                          : translate("等待使用")}
                    </span>
                  ) : row.enabled !== undefined ? (
                    <State value={row.enabled ? "active" : "disabled"} />
                  ) : null}
                </button>
                {externalURL(row.url) && (
                  <a
                    href={externalURL(row.url)}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="text-link"
                  >
                    {translate("打开链接 ↗")}
                  </a>
                )}
              </div>
            ))}
          </div>
          {!rows.length && !records.loading && !records.error && (
            <Unavailable title={translate("暂无可见记录")} />
          )}
          <Pager
            offset={offset}
            limit={20}
            total={rows.length}
            count={Math.min(20, rows.length - offset)}
            loading={records.loading}
            onOffset={setOffset}
          />
        </>
      )}
      {create && createAction && (
        <ActionForm
          capability={createAction}
          preset={kind === "bookmarks" ? { expected_revision: 0 } : {}}
          onBack={() => setCreate(false)}
          onExecuted={refresh}
        />
      )}
      {!create && (current || operation === prefix + "_logs") && (
        <ResourceActions
          capabilities={capabilities}
          names={(name) =>
            (name.startsWith(prefix + "_") &&
              !name.endsWith("_list") &&
              !name.endsWith("_create") &&
              name !== prefix + "_logs") ||
            (operation === prefix + "_logs" && name === operation)
          }
          preset={
            current ? { ...current, expected_revision: current.revision } : {}
          }
          selected={operation}
          onSelect={setOperation}
          onExecuted={refresh}
        />
      )}
    </div>
  );
}
