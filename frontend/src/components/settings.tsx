"use client";
import { useEffect, useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { TextInput } from "@astryxdesign/core/TextInput";
import { api, Capability, fullDate, list, Session } from "@/lib/api";
import { copyText } from "@/lib/clipboard";
import { Palette, palettes } from "./panel";
import { Icon } from "./icon";
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
  const [tokenScopes, setTokenScopes] = useState<string[]>([
    "system:read",
    "notes:read",
  ]);
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
        <div className="card-heading">
          <h2>外观与配色</h2>
          <span className="status-chip neutral">当前浏览器偏好</span>
        </div>
        <p className="muted">
          选择后立即应用。配色只改变个人外观，不改变业务状态语义。
        </p>
        <div className="theme-grid" role="radiogroup" aria-label="主题配色">
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
              aria-label={`${p.name}主题`}
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
          <div className="card-heading">
            <h2>MCP 接入</h2>
            <span className="status-chip neutral">原生协议</span>
          </div>
          <p>
            已授权的 AI 客户端可通过 MCP
            查询信息和管理资源。面板不运行模型；所有操作使用相同权限、便签版本与审计记录。
          </p>
          <div className="connection-options">
            <div>
              <h3>远程连接</h3>
              <code>
                {typeof location !== "undefined"
                  ? location.origin
                  : "你的面板地址"}
                /mcp
              </code>
              <p className="muted">
                使用支持 Streamable HTTP 的客户端，通过 OAuth
                授权或访问令牌连接。
              </p>
            </div>
            <div>
              <h3>本地连接</h3>
              <code>
                powerlinux mcp --stdio --endpoint http://127.0.0.1:9000
              </code>
              <p className="muted">
                本地客户端连接受保护的面板服务。具体启动参数参见发行包文档。
              </p>
            </div>
          </div>
          <p className="muted">
            MCP 服务可用代表接入能力，不能据此判断有 AI 客户端在线。
          </p>
          <div className="button-row">
            <Button label="查看操作记录" variant="ghost" href="/?view=audit" />
          </div>
        </Card>
        <Card className="panel-card">
          <h2>客户端访问令牌</h2>
          <p className="muted">
            仅授权所需范围。令牌只在创建时显示一次，撤销后立即失效。
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
              label="客户端名称"
              value={tokenName}
              onChange={setTokenName}
              isRequired
              placeholder="例如 Claude / 运维自动化"
            />
            <fieldset>
              <legend>授权范围</legend>
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
                        onChange={(e) =>
                          setTokenScopes((s) =>
                            e.target.checked
                              ? [...s, scope]
                              : s.filter((v) => v !== scope),
                          )
                        }
                      />
                      <code>{scope}</code>
                    </label>
                  ))}
              </div>
            </fieldset>
            <TextInput
              label="资源范围"
              value={resources}
              onChange={setResources}
              description="逗号分隔的资源 ID。使用 * 会授权所有资源，请按实际需要设置。"
            />
            <Button
              label="创建访问令牌"
              variant="primary"
              isLoading={tokenBusy}
              type="submit"
            />
          </form>
          {newToken && (
            <section className="notice warning" aria-label="新访问令牌">
              <strong>请立即保存令牌</strong>
              <TextInput
                label="访问令牌"
                type="password"
                value={newToken}
                isReadOnly
                autoComplete="off"
              />
              <div className="button-row">
                <Button
                  label="复制令牌"
                  onClick={() =>
                    void copyText(newToken).catch(() =>
                      setTokenError("无法自动复制，请手动选择复制。"),
                    )
                  }
                />
                <Button
                  label="已保存，隐藏令牌"
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
                  资源 {token.resources?.join(", ") || "未提供"} · 最近访问{" "}
                  {fullDate(token.last_used_at)}
                </small>
              </div>
              <Button
                label={`撤销 ${token.name}`}
                variant="ghost"
                onClick={() => void revoke(token.id)}
              />
            </div>
          ))}
          {!tokens.length && !tokenError && (
            <p className="muted">暂无客户端令牌。</p>
          )}
        </Card>
      </div>
    );
  return (
    <div className="stack">
      <Card className="panel-card">
        <h2>账户与安全</h2>
        <p className="muted">当前账户 · {session.user?.name}</p>
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
              setMessage("密码已更新，请重新登录。");
              await onSession();
            });
          }}
        >
          <TextInput
            label="当前密码"
            type="password"
            value={oldPassword}
            onChange={setOldPassword}
            isRequired
            autoComplete="current-password"
          />
          <TextInput
            label="新密码"
            type="password"
            value={newPassword}
            onChange={setNewPassword}
            isRequired
            autoComplete="new-password"
            description="使用至少 12 个字符的独立密码。"
          />
          <TextInput
            label="两步验证码（更新密码）"
            value={otp}
            onChange={setOTP}
            autoComplete="one-time-code"
            description="已启用两步验证时填写。"
          />
          <Button
            label="更新密码"
            variant="primary"
            type="submit"
            isLoading={securityBusy}
          />
        </form>
      </Card>
      <Card className="panel-card">
        <h2>两步验证</h2>
        <p className="muted">登录和敏感账户操作按同一验证规则执行。</p>
        <TextInput
          label="当前密码（两步验证）"
          type="password"
          value={oldPassword}
          onChange={setOldPassword}
          autoComplete="current-password"
        />
        <Button
          label="开始设置两步验证"
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
            <p>将以下密钥添加到认证器，随后输入验证码完成启用。</p>
            <TextInput
              label="认证器密钥"
              type="password"
              value={enrollment.secret ?? ""}
              isReadOnly
            />
            <Button
              label="复制认证器链接"
              onClick={() =>
                void copyText(
                  enrollment.otpauth_url ?? enrollment.uri ?? "",
                ).catch((e) => setError((e as Error).message))
              }
            />
            <TextInput
              label="认证器验证码"
              value={otp}
              onChange={setOTP}
              autoComplete="one-time-code"
            />
            <Button
              label="启用两步验证"
              variant="primary"
              onClick={() =>
                void securityAction(async () => {
                  await api("/account/totp/confirm", {
                    method: "POST",
                    body: { otp },
                  });
                  setEnrollment(undefined);
                  setOTP("");
                  setMessage("两步验证已启用。");
                })
              }
              isLoading={securityBusy}
            />
          </div>
        )}
        <details>
          <summary>关闭两步验证</summary>
          <div className="form-stack">
            <TextInput
              label="当前密码（关闭验证）"
              type="password"
              value={oldPassword}
              onChange={setOldPassword}
              autoComplete="current-password"
            />
            <TextInput
              label="当前两步验证码"
              value={otp}
              onChange={setOTP}
              autoComplete="one-time-code"
            />
            <Button
              label="关闭两步验证"
              onClick={() =>
                void securityAction(async () => {
                  await api("/account/totp/disable", {
                    method: "POST",
                    body: { password: oldPassword, otp },
                  });
                  setOTP("");
                  setMessage("两步验证已关闭。");
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
