"use client";
import { SectionHeader } from "./section-header";
import { t as translate } from "@/lib/i18n";
import { useEffect, useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { TextInput } from "@astryxdesign/core/TextInput";
import { api, Capability, fullDate, list, Session } from "@/lib/api";
import { copyText } from "@/lib/clipboard";
import { Palette, palettes } from "./panel";
import { Icon } from "./icon";
import { LanguageControl } from "./language-control";
import { tokenPresetScopes, scopeLabel, type TokenPurpose } from "@/lib/token-presets";
import { FontSettings } from "./font-settings";
type APIToken = {
  id: string;
  name: string;
  scopes: string[];
  resources?: string[];
  created_at: string;
  expires_at?: string;
  last_used_at?: string;
};
export function Settings({
  page,
  palette,
  onPalette,
  session,
  onSession,
  capabilities,
}: {
  page: string;
  palette: Palette;
  onPalette: (p: Palette) => void;
  session: Session;
  onSession: () => Promise<Session | undefined>;
  capabilities: Capability[];
}) {
  const [tokens, setTokens] = useState<APIToken[]>([]);
  const [tokenName, setTokenName] = useState("");
  const [tokenPurpose, setTokenPurpose] = useState<TokenPurpose>("custom");
  const [tokenDays, setTokenDays] = useState(90);
  const availableScopes = [...new Set(capabilities.map(capability => capability.scope).filter((scope): scope is string => !!scope))];
  const [tokenScopes, setTokenScopes] = useState<string[]>([
    "system:read",
    "notes:read",
  ].filter(scope => availableScopes.includes(scope)));
  const scopeSignature = availableScopes.slice().sort().join(",");
  useEffect(() => {
    setTokenScopes(previous => {
      const next = previous.filter(scope => availableScopes.includes(scope));
      return next.length === previous.length ? previous : next;
    });
  }, [scopeSignature]);
  const [resources, setResources] = useState("host:local");
  const [newToken, setNewToken] = useState("");
  const [tokenError, setTokenError] = useState("");
  const [tokenBusy, setTokenBusy] = useState(false);
  const [oldPassword, setOldPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");
  const [otp, setOTP] = useState("");
  const [enrollment, setEnrollment] = useState<{
    secret?: string;
    otpauth_url?: string;
    uri?: string;
  }>();
  const [securityBusy, setSecurityBusy] = useState(false);
  const refreshTokens = async () => {
    try {
      setTokens(list(await api<APIToken[]>("/tokens")));
      setTokenError("");
    } catch (e) {
      setTokenError((e as Error).message);
    }
  };
  useEffect(() => {
    if (page === "mcp") void refreshTokens();
  }, [page]);
  useEffect(() => {
    if (page !== "mcp") setNewToken("");
    if (page !== "security") {
      setEnrollment(undefined);
      setOldPassword("");
      setNewPassword("");
      setOTP("");
    }
  }, [page]);
  const createToken = async () => {
    setTokenBusy(true);
    setTokenError("");
    setNewToken("");
    try {
      const v = await api<{ token?: string; secret?: string }>("/tokens", {
        method: "POST",
        body: {
          name: tokenName,
          kind: "mcp",
          expires_in_seconds: tokenDays * 86400,
          scopes: tokenScopes,
          resources: resources
            .split(",")
            .map((r) => r.trim())
            .filter(Boolean),
        },
      });
      setNewToken(v.token ?? v.secret ?? "");
      setTokenName("");
      await refreshTokens();
    } catch (e) {
      setTokenError((e as Error).message);
    } finally {
      setTokenBusy(false);
    }
  };
  const revoke = async (id: string) => {
    try {
      await api(`/tokens/${encodeURIComponent(id)}`, { method: "DELETE" });
      await refreshTokens();
    } catch (e) {
      setTokenError((e as Error).message);
    }
  };
  const securityAction = async (fn: () => Promise<void>) => {
    setSecurityBusy(true);
    setError("");
    setMessage("");
    try {
      await fn();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setSecurityBusy(false);
    }
  };
  if (page === "appearance")
    return (
      <Card className="panel-card">
        <SectionHeader icon="settings" className="card-heading">
          <h2>{translate("外观与配色")}</h2>
          <span className="meta-tag">
            {translate("当前浏览器偏好")}
          </span>
        </SectionHeader>
        <p className="muted">
          {translate(
            "选择后立即应用。配色只改变个人外观，不改变业务状态语义。",
          )}
        </p>
        <div className="language-settings">
          <h2>{translate("界面语言")}</h2>
          <LanguageControl />
        </div>
        <FontSettings />
        <div
          className="theme-grid"
          role="radiogroup"
          aria-label={translate("主题配色")}
        >
          {palettes.map((p) => (
            <button
              key={p.id}
              className={`theme-option preview-${p.id}${palette === p.id ? " selected" : ""}`}
              role="radio"
              aria-checked={palette === p.id}
              tabIndex={palette === p.id ? 0 : -1}
              onKeyDown={(e) => {
                const i = palettes.findIndex((v) => v.id === p.id);
                const next =
                  e.key === "ArrowRight" || e.key === "ArrowDown"
                    ? (i + 1) % palettes.length
                    : e.key === "ArrowLeft" || e.key === "ArrowUp"
                      ? (i + palettes.length - 1) % palettes.length
                      : e.key === "Home"
                        ? 0
                        : e.key === "End"
                          ? palettes.length - 1
                          : -1;
                if (next >= 0) {
                  e.preventDefault();
                  onPalette(palettes[next].id);
                  (
                    e.currentTarget.parentElement?.children[
                      next
                    ] as HTMLButtonElement
                  )?.focus();
                }
              }}
              onClick={() => onPalette(p.id)}
              aria-label={translate("{0}主题", [p.name])}
            >
              <div className="theme-preview">
                <div className="preview-mini-card">
                  <span />
                  <i />
                  <i />
                </div>
                <div className="preview-mini-card">
                  <span />
                  <i />
                  <i />
                </div>
                <div className="preview-mini-nav">
                  <i />
                  <i />
                  <i />
                </div>
              </div>
              <div className="theme-name">
                <div>
                  <strong>{p.name}</strong>
                  <small>{p.desc}</small>
                </div>
                {palette === p.id && <Icon name="check" size={18} />}
              </div>
            </button>
          ))}
        </div>
      </Card>
    );
  if (page === "mcp")
    return (
      <div className="stack">
        <Card className="panel-card">
          <SectionHeader icon="settings" className="card-heading">
            <h2>{translate("连接方式")}</h2>
            <span className="meta-tag">{translate("原生协议")}</span>
          </SectionHeader>
          <p>
            {translate(
              "通过 MCP 连接客户端，按所选权限查看信息和管理资源，操作可在记录中查看。",
            )}
          </p>
          <div className="connection-options">
            <div>
              <h3>{translate("远程连接")}</h3>
              <code>
                {typeof location !== "undefined"
                  ? location.origin
                  : translate("你的面板地址")}
                /mcp
              </code>
              <p className="muted">
                {translate(
                  "使用支持 Streamable HTTP 的客户端，通过 OAuth 授权或访问令牌连接。",
                )}
              </p>
            </div>
            <div>
              <h3>{translate("本地连接")}</h3>
              <code>folio mcp --stdio --endpoint http://127.0.0.1:9000</code>
              <p className="muted">
                {translate(
                  "本地客户端连接受保护的面板服务。具体启动参数参见发行包文档。",
                )}
              </p>
            </div>
          </div>
          <details className="optional-options"><summary>{translate("连接说明")}</summary><p className="muted">{translate("MCP 服务可用代表接入能力，不能据此判断有 AI 客户端在线。")}</p></details>
          <div className="button-row">
            <Button
              label={translate("查看操作记录")}
              variant="ghost"
              href="/?view=audit"
            />
          </div>
        </Card>
        <Card className="panel-card">
          <SectionHeader icon="key"><h2>{translate("客户端访问令牌")}</h2></SectionHeader>
          <p className="muted">
            {translate(
              "仅授权所需范围。令牌只在创建时显示一次，撤销后立即失效。",
            )}
          </p>
          {tokenError && (
            <div className="notice danger" role="alert">
              {tokenError}
            </div>
          )}
          <form
            className="form-stack"
            onSubmit={(e) => {
              e.preventDefault();
              void createToken();
            }}
          >
            <TextInput
              label={translate("客户端名称")}
              value={tokenName}
              onChange={setTokenName}
              isRequired
              placeholder={translate("例如 Claude / 运维自动化")}
            />
            <div className="business-field-pair">
              <label className="native-field">{translate("授权用途")}
                <select aria-label={translate("授权用途")} value={tokenPurpose} onChange={event => {
                  const purpose = event.target.value as TokenPurpose;
                  setTokenPurpose(purpose);
                  if (purpose !== "custom") setTokenScopes(tokenPresetScopes(purpose, availableScopes));
                }}>
                  <option value="custom">{translate("自定义授权")}</option>
                  <option value="monitor">{translate("只读监控")}</option>
                  <option value="notes">{translate("便签交接")}</option>
                </select>
              </label>
              <label className="native-field">{translate("令牌有效期")}
                <select aria-label={translate("令牌有效期")} value={tokenDays} onChange={event => setTokenDays(Number(event.target.value))}>
                  {[1, 7, 30, 90, 365].map(days => <option key={days} value={days}>{translate("{0} 天", [days])}</option>)}
                </select>
              </label>
            </div>
            <p className="selected-scopes">{translate("已选权限")}：{tokenScopes.length ? tokenScopes.map(scopeLabel).join("、") : translate("尚未选择权限")}</p>
            <details className="optional-options">
              <summary>{translate("自定义权限")}</summary>
            <fieldset>
              <legend>{translate("授权范围")}</legend>
              <div className="scope-grid">
                {capabilities
                  .map((c) => c.scope)
                  .filter((scope): scope is string => !!scope)
                  .filter((v, i, a) => a.indexOf(v) === i)
                  .map((scope) => (
                    <label key={scope} className="check-label">
                      <input
                        type="checkbox"
                        checked={tokenScopes.includes(scope)}
                        onChange={(e) => {
                          setTokenPurpose("custom");
                          setTokenScopes((s) => e.target.checked ? [...s, scope] : s.filter((v) => v !== scope));
                        }}
                      />
                      <span>{scopeLabel(scope)}<small><code>{scope}</code></small></span>
                    </label>
                  ))}
              </div>
            </fieldset>
            </details>
            <TextInput
              label={translate("资源范围")}
              value={resources}
              onChange={setResources}
              description={translate(
                "逗号分隔的资源 ID。使用 * 会授权所有资源，请按实际需要设置。",
              )}
            />
            <Button
              label={translate("创建访问令牌")}
              variant="primary"
              isLoading={tokenBusy}
              isDisabled={!tokenScopes.length}
              type="submit"
            />
          </form>
          {newToken && (
            <section
              className="notice warning"
              aria-label={translate("新访问令牌")}
            >
              <strong>{translate("请立即保存令牌")}</strong>
              <TextInput
                label={translate("访问令牌")}
                type="password"
                value={newToken}
                isReadOnly
                autoComplete="off"
              />
              <div className="button-row">
                <Button
                  label={translate("复制令牌")}
                  onClick={() =>
                    void copyText(newToken).catch(() =>
                      setTokenError(
                        translate("无法自动复制，请手动选择复制。"),
                      ),
                    )
                  }
                />
                <Button
                  label={translate("已保存，隐藏令牌")}
                  onClick={() => setNewToken("")}
                />
              </div>
            </section>
          )}
          {tokens.map((token) => (
            <div className="token-row" key={token.id}>
              <div>
                <strong>{token.name}</strong>
                <p>{token.scopes?.join(" · ")}</p>
                <small>
                  {translate("资源")}
                  {token.resources?.join(", ") || translate("未提供")}
                  {translate(" · 最近访问")} {fullDate(token.last_used_at)}
                </small>
              </div>
              <Button
                label={translate("撤销 {0}", [token.name])}
                variant="ghost"
                onClick={() => void revoke(token.id)}
              />
            </div>
          ))}
          {!tokens.length && !tokenError && (
            <p className="muted">{translate("暂无客户端令牌。")}</p>
          )}
        </Card>
      </div>
    );
  return (
    <div className="stack">
      <Card className="panel-card">
        <SectionHeader icon="key"><h2>{translate("修改密码")}</h2></SectionHeader>
        <p className="muted">
          {translate("当前账户 · ")}
          {session.user?.name}
        </p>
        {error && (
          <div className="notice danger" role="alert">
            {error}
          </div>
        )}
        {message && (
          <div className="notice success" role="status">
            {message}
          </div>
        )}
        <form
          className="form-stack"
          onSubmit={(e) => {
            e.preventDefault();
            void securityAction(async () => {
              await api("/account/password", {
                method: "POST",
                body: {
                  old_password: oldPassword,
                  new_password: newPassword,
                  otp,
                },
              });
              setOldPassword("");
              setNewPassword("");
              setMessage(translate("密码已更新，请重新登录。"));
              await onSession();
            });
          }}
        >
          <TextInput
            label={translate("当前密码")}
            type="password"
            value={oldPassword}
            onChange={setOldPassword}
            isRequired
            autoComplete="current-password"
          />
          <TextInput
            label={translate("新密码")}
            type="password"
            value={newPassword}
            onChange={setNewPassword}
            isRequired
            autoComplete="new-password"
            description={translate("使用至少 12 个字符的独立密码。")}
          />
          <TextInput
            label={translate("两步验证码（更新密码）")}
            value={otp}
            onChange={setOTP}
            autoComplete="one-time-code"
            description={translate("已启用两步验证时填写。")}
          />
          <Button
            label={translate("更新密码")}
            variant="primary"
            type="submit"
            isLoading={securityBusy}
          />
        </form>
      </Card>
      <Card className="panel-card">
        <SectionHeader icon="shield"><h2>{translate("两步验证")}</h2></SectionHeader>
        <p className="muted">
          {translate("登录和敏感账户操作按同一验证规则执行。")}
        </p>
        <TextInput
          label={translate("当前密码（两步验证）")}
          type="password"
          value={oldPassword}
          onChange={setOldPassword}
          autoComplete="current-password"
        />
        <Button
          label={translate("开始设置两步验证")}
          onClick={() =>
            void securityAction(async () => {
              setEnrollment(
                await api("/account/totp/enroll", {
                  method: "POST",
                  body: { password: oldPassword, otp },
                }),
              );
            })
          }
          isLoading={securityBusy}
        />
        {enrollment && (
          <div className="form-stack">
            <p>
              {translate("将以下密钥添加到认证器，随后输入验证码完成启用。")}
            </p>
            <TextInput
              label={translate("认证器密钥")}
              type="password"
              value={enrollment.secret ?? ""}
              isReadOnly
            />
            <Button
              label={translate("复制认证器链接")}
              onClick={() =>
                void copyText(
                  enrollment.otpauth_url ?? enrollment.uri ?? "",
                ).catch((e) => setError((e as Error).message))
              }
            />
            <TextInput
              label={translate("认证器验证码")}
              value={otp}
              onChange={setOTP}
              autoComplete="one-time-code"
            />
            <Button
              label={translate("启用两步验证")}
              variant="primary"
              onClick={() =>
                void securityAction(async () => {
                  await api("/account/totp/confirm", {
                    method: "POST",
                    body: { otp },
                  });
                  setEnrollment(undefined);
                  setOTP("");
                  setMessage(translate("两步验证已启用。"));
                })
              }
              isLoading={securityBusy}
            />
          </div>
        )}
        <details>
          <summary>{translate("关闭两步验证")}</summary>
          <div className="form-stack">
            <TextInput
              label={translate("当前密码（关闭验证）")}
              type="password"
              value={oldPassword}
              onChange={setOldPassword}
              autoComplete="current-password"
            />
            <TextInput
              label={translate("当前两步验证码")}
              value={otp}
              onChange={setOTP}
              autoComplete="one-time-code"
            />
            <Button
              label={translate("关闭两步验证")}
              onClick={() =>
                void securityAction(async () => {
                  await api("/account/totp/disable", {
                    method: "POST",
                    body: { password: oldPassword, otp },
                  });
                  setOTP("");
                  setMessage(translate("两步验证已关闭。"));
                })
              }
              isLoading={securityBusy}
            />
          </div>
        </details>
      </Card>
    </div>
  );
}
