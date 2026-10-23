"use client";
import { ResourceError } from "./resource-ui";
import { ClientTable } from "./client-table";
import { t as translate } from "@/lib/i18n";
import { useRef, useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { TextInput } from "@astryxdesign/core/TextInput";
import { action, Capability, fullDate, list, Task } from "@/lib/api";
import { copyText } from "@/lib/clipboard";
import { choiceLabel } from "@/lib/labels";
import { manualHTTPOrder } from "@/lib/manual-http-acme";
import { Page } from "@/lib/resources";
import { useResource } from "@/lib/resource-hook";
import { ActionContext, ActionForm, References } from "./actions";
import { TaskResult } from "./task-result";
import {
  ResourceActions,
  ResourceDrawer,
  State,
  Unavailable,
} from "./resource-ui";
type Certificate = {
  id: string;
  name: string;
  domains: string[];
  issuer: string;
  expires_at: string;
  revision: number;
  source: string;
};
type DNSProvider = {
  id: string;
  name: string;
  fields: string[];
  available: boolean;
  reason: string;
};
type DNSCredential = {
  id: string;
  name: string;
  provider: string;
  revision: number;
  updated_at: string;
  credential_configured: boolean;
};
type Order = {
  certificate_id: string;
  state: string;
  expires_at: string;
  provider?: string;
  confirmation_action?: string;
  cancellation_action?: string;
  records?: { name: string; type: string; value: string }[];
  http_challenges?: {
    domain: string;
    url: string;
    path: string;
    body: string;
  }[];
};
import { usePageState } from "@/lib/navigation";
export function CertificatesView({
  capabilities,
  onExecuted,
  active = true,
  siteID,
}: {
  capabilities: Capability[];
  onExecuted: () => void;
  active?: boolean;
  siteID?: string;
}) {
  const [page, setPage] = usePageState("sites", "kind", "certificates", true);
  const [resourceID, setResourceID] = usePageState(
    "sites",
    "resource",
    "",
    true,
  );
  const [selected, setSelectedLocal] = useState<Certificate>();
  const setSelected = (value: Certificate) => {
    setSelectedLocal(value);
    setResourceID(value.id);
  };
  const [credential, setCredentialLocal] = useState<DNSCredential>();
  const setCredential = (value: DNSCredential) => {
    setCredentialLocal(value);
    setResourceID(value.id);
  };
  const open = !!resourceID;
  const setOpen = (value: boolean) => {
    if (!value) setResourceID("");
  };
  const [create, setCreate] = useState<string>();
  const [manualHTTPCreate, setManualHTTPCreate] = useState(false);
  const [operation, setOperation] = useState<string>();
  const [orderID, setOrderID] = useState("");
  const [loadedID, setLoadedID] = useState("");
  const [orderTask, setOrderTask] = useState<Task>();
  const [lookupError, setLookupError] = useState("");
  const [httpAcknowledged, setHTTPAcknowledged] = useState(false);
  const [copiedHTTP, setCopiedHTTP] = useState("");
  const [copyError, setCopyError] = useState("");
  const lookupGeneration = useRef(0);
  const has = (name: string) => capabilities.some((c) => c.name === name);
  const pageTabs = [
    ...(has("certificates_list") || has("certificates_import")
      ? [{ id: "certificates", label: translate("证书") }]
      : []),
    ...(capabilities.some((capability) =>
      capability.name.startsWith("certificates_acme_"),
    )
      ? [{ id: "manual", label: translate("验证与恢复") }]
      : []),
    ...(capabilities.some((capability) =>
      capability.name.startsWith("certificates_dns_"),
    )
      ? [{ id: "dns", label: translate("DNS凭据与提供商") }]
      : []),
  ];
  const activePage = pageTabs.some((tab) => tab.id === page)
    ? page
    : pageTabs[0]?.id;
  const certificates = useResource<Page<Certificate>>(
    "certificates_list",
    {},
    active && has("certificates_list"),
  );
  const providers = useResource<Page<DNSProvider>>(
    "certificates_dns_providers",
    {},
    active &&
      (activePage === "dns" ||
        create === "certificates_acme_issue" ||
        create === "certificates_dns_credentials_save") &&
      has("certificates_dns_providers"),
  );
  const credentials = useResource<Page<DNSCredential>>(
    "certificates_dns_credentials_list",
    {},
    active &&
      (activePage === "dns" || create === "certificates_acme_issue") &&
      has("certificates_dns_credentials_list"),
  );
  const order = useResource<Order>(
    "certificates_acme_status",
    { id: loadedID },
    active &&
      activePage === "manual" &&
      !!loadedID &&
      has("certificates_acme_status"),
    5000,
  );
  const current =
    list(certificates.data).find((c) => c.id === resourceID) ??
    (selected?.id === resourceID ? selected : undefined);
  const currentCredential =
    list(credentials.data).find((c) => c.id === resourceID) ??
    (credential?.id === resourceID ? credential : undefined);
  const httpOrder =
    order.data?.state === "pending_http"
      ? manualHTTPOrder(order.data, loadedID)
      : undefined;
  const selectedAction = capabilities.find((c) => c.name === create);
  const issueCapability = capabilities.find(
    (c) => c.name === "certificates_acme_issue",
  );
  const issueSchema = issueCapability?.input_schema as
    | {
        properties?: Record<string, unknown>;
        required?: string[];
        [key: string]: unknown;
      }
    | undefined;
  const challengeSchema = issueSchema?.properties?.challenge as
    | { enum?: unknown[]; [key: string]: unknown }
    | undefined;
  const hasManualHTTP = !!(
    issueCapability?.scope === "certificates:write" &&
    issueCapability.task === true &&
    issueCapability.read_only === false &&
    challengeSchema?.enum?.includes("manual_http")
  );
  const manualHTTPAction = (() => {
    if (!issueCapability || !issueSchema?.properties || !hasManualHTTP)
      return undefined;
    const inputFields = [
      "id",
      "expected_revision",
      "domains",
      "email",
      "challenge",
    ];
    if (
      !inputFields.every((name) =>
        Object.prototype.hasOwnProperty.call(issueSchema.properties, name),
      )
    )
      return undefined;
    const properties = Object.fromEntries(
      inputFields.map((name) => [
        name,
        name === "challenge"
          ? {
              ...(issueSchema.properties![name] as object),
              const: "manual_http",
            }
          : issueSchema.properties![name],
      ]),
    );
    return {
      ...issueCapability,
      input_schema: {
        ...issueSchema,
        properties,
        required: ["id", "expected_revision", "domains", "email"],
      },
    } as Capability;
  })();
  const manualHTTPConfirm = capabilities.find(
    (c) =>
      c.name === "certificates_acme_confirm" &&
      c.scope === "certificates:write" &&
      c.task === true &&
      c.read_only === false &&
      Object.prototype.hasOwnProperty.call(
        c.input_schema?.properties ?? {},
        "id",
      ),
  );
  const selectedFormAction =
    manualHTTPCreate && create === "certificates_acme_issue"
      ? manualHTTPAction
      : create === "certificates_acme_issue" &&
          selectedAction &&
          challengeSchema?.enum
        ? ({
            ...selectedAction,
            input_schema: {
              ...issueSchema,
              properties: {
                ...issueSchema?.properties,
                challenge: {
                  ...challengeSchema,
                  enum: challengeSchema.enum.filter(
                    (value) => value !== "manual_http",
                  ),
                },
              },
            },
          } as Capability)
        : selectedAction;
  const references: References = {
    ...(providers.data
      ? {
          provider: list(providers.data).map((row) => ({
            value: row.id,
            label: row.name + (row.available ? "" : translate(" · 不可用")),
            disabled: !row.available,
          })),
        }
      : {}),
    ...(credentials.data
      ? {
          credential_id: list(credentials.data).map((row) => ({
            value: row.id,
            label: `${row.name} · ${row.provider}`,
          })),
        }
      : {}),
  };
  const refresh = () => {
    void certificates.reload();
    void credentials.reload();
    void order.reload();
    onExecuted();
  };
  const copyHTTP = async (key: string, value: string) => {
    setCopyError("");
    try {
      await copyText(value);
      setCopiedHTTP(key);
    } catch (error) {
      setCopyError(
        error instanceof Error
          ? error.message
          : translate("无法复制验证内容。"),
      );
    }
  };
  const showOrder = (value: Order, task?: Task) => {
    setOrderID(value.certificate_id);
    setLoadedID(value.certificate_id);
    if (
      [
        "pending_dns",
        "pending_http",
        "recovery_required",
        "dns_cleanup_required",
      ].includes(value.state)
    ) {
      setPage("manual");
      setHTTPAcknowledged(false);
      setCopyError("");
      setCreate(undefined);
      setManualHTTPCreate(false);
      setOpen(false);
      setOperation(undefined);
      if (task) setOrderTask(task);
    }
    if (value.state === "cancelled") {
      setLoadedID("");
      setOperation(undefined);
      if (task) setOrderTask(task);
    }
  };
  const saved = (result?: unknown, context?: ActionContext) => {
    const generation = ++lookupGeneration.current;
    refresh();
    const value = result as {
      certificate_id?: string;
      id?: string;
      state?: string;
      domains?: string[];
      expires_at?: string;
      certificate?: Certificate;
    };
    const certificate = value?.certificate ?? value;
    if (
      certificate?.id &&
      Array.isArray(certificate.domains) &&
      certificate.expires_at
    ) {
      setPage("certificates");
      setCreate(undefined);
      setLoadedID("");
      setOpen(false);
      setOperation(undefined);
      setOrderTask(undefined);
      setLookupError("");
      return;
    }
    if (value?.certificate_id) {
      showOrder(value as Order, context?.task);
      setLookupError("");
      return;
    }
    if (
      context?.task &&
      context.input.id &&
      /^certificates_acme_(issue|renew|confirm|cancel)$/.test(context.action) &&
      has("certificates_acme_status")
    ) {
      setLookupError("");
      void action<Order>("certificates_acme_status", { id: context.input.id })
        .then((value) => {
          if (generation !== lookupGeneration.current) return;
          showOrder(value, context.task);
        })
        .catch((e) => {
          if (generation === lookupGeneration.current)
            setLookupError(
              translate("保留订单状态未能读取：{0}", [(e as Error).message]),
            );
        });
    }
  };
  const statusNames: Record<string, string> = {
    pending_dns: translate("待确认DNS记录"),
    pending_http: translate("待放置HTTP验证文件"),
    issued: translate("已签发"),
    cancelled: translate("已取消"),
    not_due: translate("尚未到续期时间"),
    interrupted: translate("已中断"),
    expired: translate("订单已到期"),
    recovery_required: translate("DNS订单待恢复"),
    dns_cleanup_required: translate("DNS记录待清理"),
  };
  return (
    <div className="stack">
      <Card className="panel-card">
        {lookupError && (
          <div className="notice danger" role="alert">
            {lookupError}
          </div>
        )}
        <div className="card-heading">
          <h2>{translate("证书与验证")}</h2>
          <div className="button-row">
            {has("certificates_import") && (
              <Button
                label={translate("导入证书")}
                onClick={() => {
                  setManualHTTPCreate(false);
                  setCreate("certificates_import");
                }}
              />
            )}{" "}
            {has("certificates_acme_issue") && (
              <>
                <Button
                  label={translate("申请ACME证书")}
                  variant="primary"
                  onClick={() => {
                    setManualHTTPCreate(false);
                    setCreate("certificates_acme_issue");
                  }}
                />
                {manualHTTPAction && (
                  <Button
                    label={translate("手动HTTP验证")}
                    variant="ghost"
                    onClick={() => {
                      setManualHTTPCreate(true);
                      setCreate("certificates_acme_issue");
                    }}
                  />
                )}
              </>
            )}
          </div>
        </div>
        <div
          className="section-tabs"
          role="tablist"
          aria-label={translate("证书资源类型")}
        >
          {pageTabs.map((tab) => (
            <button
              role="tab"
              aria-selected={activePage === tab.id}
              key={tab.id}
              onClick={() => {
                setPage(tab.id);
                setOpen(false);
                setOperation(undefined);
              }}
            >
              {tab.label}
            </button>
          ))}
        </div>
        {activePage === "certificates" ? (
          <>
            {certificates.error && (
              <ResourceError error={certificates.error} state={certificates.state} retry={() => void certificates.reload()} />
            )}
            {!has("certificates_list") ? (
              <Unavailable title={translate("证书服务暂不可用")} />
            ) : (
              <>
                <ClientTable>
                  <table className="resource-table">
                    <thead>
                      <tr>
                        <th>{translate("名称 / 域名")}</th>
                        <th>{translate("颁发者")}</th>
                        <th>{translate("到期")}</th>
                        <th>{translate("来源")}</th>
                      </tr>
                    </thead>
                    <tbody>
                      {list(certificates.data).map((cert) => (
                        <tr key={cert.id}>
                          <td>
                            <button
                              className="resource-name"
                              onClick={() => {
                                setSelected(cert);
                                setOperation(undefined);
                                setOpen(true);
                              }}
                            >
                              {cert.name || cert.id}
                            </button>
                            <small>{cert.domains.join(" · ")}</small>
                          </td>
                          <td>{cert.issuer}</td>
                          <td>{fullDate(cert.expires_at)}</td>
                          <td>{choiceLabel(cert.source)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </ClientTable>
                {!list(certificates.data).length &&
                  !certificates.loading &&
                  !certificates.error && (
                    <Unavailable
                      title={translate("暂无可见证书")}
                      detail={
                        has("certificates_acme_issue")
                          ? translate(
                              "导入已有证书，或通过真实ACME流程申请。没有签发产物时不会显示已签发。",
                            )
                          : translate(
                              "导入已有证书，再绑定到需要启用HTTPS的网站。",
                            )
                      }
                    />
                  )}
              </>
            )}
          </>
        ) : activePage === "manual" ? (
          <div className="form-stack">
            <form
              onSubmit={(e) => {
                e.preventDefault();
                ++lookupGeneration.current;
                if (orderID !== loadedID) setOrderTask(undefined);
                setLookupError("");
                setLoadedID(orderID);
                void order.reload();
              }}
              className="button-row"
            >
              <TextInput
                label={translate("待验证证书标识")}
                value={orderID}
                onChange={setOrderID}
              />
              <Button
                label={translate("读取验证订单")}
                type="submit"
                isDisabled={!orderID || !has("certificates_acme_status")}
              />
            </form>
            {order.error && (
              <ResourceError error={order.error} state={order.state} retry={() => void order.reload()} />
            )}
            {orderTask && (
              <TaskResult submitted={orderTask} capabilities={capabilities} />
            )}
            {order.data && (
              <>
                <p className="muted">
                  {translate("读取于")}
                  {fullDate(order.loadedAt)}
                  {order.error ? translate(" · 当前状态未能刷新") : ""}
                </p>
                <div className="resource-summary">
                  <strong>
                    {statusNames[order.data.state] ?? order.data.state}
                  </strong>
                  <span className="muted">
                    {translate("到期")}
                    {fullDate(order.data.expires_at)}
                  </span>
                </div>
                {order.data.state === "pending_http" ? (
                  <p className="muted">
                    {translate(
                      "为每个域名在可通过 HTTP 访问的网站根目录中放置指定文件。必须写入完全相同的正文，不要添加换行；服务不会代替你上传或探测文件。",
                    )}
                  </p>
                ) : order.data.state === "pending_dns" ? (
                  <p className="muted">
                    {translate(
                      "在域名DNS中添加以下实际TXT记录，再确认验证。等待记录与创建订单均不代表证书已经签发。",
                    )}
                  </p>
                ) : (
                  <div className="notice warning">
                    {order.data.provider && (
                      <p>
                        {translate("DNS提供商：")}
                        {order.data.provider}
                      </p>
                    )}
                    <p>
                      {translate(
                        "此订单保留了DNS验证或清理状态。确认会使用原凭据继续验证；取消会清理此订单创建的记录。清理失败时订单仍保留，可以再次恢复。",
                      )}
                    </p>
                    <p>{translate("订单待恢复不代表证书已经签发。")}</p>
                  </div>
                )}
                {order.data.state === "pending_http" && httpOrder && (
                  <section
                    className="stack"
                    aria-label={translate("HTTP验证文件")}
                  >
                    {httpOrder.http_challenges.map((challenge) => (
                      <div
                        className="stack notice warning"
                        key={challenge.domain}
                      >
                        <strong>{challenge.domain}</strong>
                        <div>
                          <small>{translate("完整验证 URL")}</small>
                          <code className="wrap-code">{challenge.url}</code>
                          <Button
                            label={
                              copiedHTTP === `${challenge.domain}:url`
                                ? translate("已复制 URL")
                                : translate("复制 URL")
                            }
                            variant="ghost"
                            onClick={() =>
                              void copyHTTP(
                                `${challenge.domain}:url`,
                                challenge.url,
                              )
                            }
                          />
                        </div>
                        <div>
                          <small>{translate("网站根目录下的文件路径")}</small>
                          <code className="wrap-code">{challenge.path}</code>
                          <Button
                            label={
                              copiedHTTP === `${challenge.domain}:path`
                                ? translate("已复制路径")
                                : translate("复制路径")
                            }
                            variant="ghost"
                            onClick={() =>
                              void copyHTTP(
                                `${challenge.domain}:path`,
                                challenge.path,
                              )
                            }
                          />
                        </div>
                        <div>
                          <small>{translate("文件正文（精确内容）")}</small>
                          <pre className="json-result">{challenge.body}</pre>
                          <Button
                            label={
                              copiedHTTP === `${challenge.domain}:body`
                                ? translate("已复制正文")
                                : translate("复制正文")
                            }
                            variant="ghost"
                            onClick={() =>
                              void copyHTTP(
                                `${challenge.domain}:body`,
                                challenge.body,
                              )
                            }
                          />
                        </div>
                      </div>
                    ))}
                    {copyError && (
                      <div className="notice danger" role="alert">
                        {copyError}
                      </div>
                    )}
                    <p className="muted">
                      {translate(
                        "放置完成后，请从外部网络确认上方 URL 能通过普通 HTTP 返回完全相同的正文，再继续验证。",
                      )}
                    </p>
                    <label className="check-label">
                      <input
                        type="checkbox"
                        checked={httpAcknowledged}
                        onChange={(event) =>
                          setHTTPAcknowledged(event.target.checked)
                        }
                      />
                      <span>
                        {translate(
                          "已放置所有验证文件；我确认 URL 与正文都与此订单完全一致，继续验证。",
                        )}
                      </span>
                    </label>
                    {httpAcknowledged && manualHTTPConfirm && (
                      <ActionForm
                        key={`acme-http-confirm:${loadedID}:${httpOrder.order_ref}`}
                        capability={manualHTTPConfirm}
                        preset={{ id: loadedID }}
                        lockedPresetFields={["id"]}
                        onBack={() => setHTTPAcknowledged(false)}
                        onExecuted={saved}
                      />
                    )}
                    {httpAcknowledged && !manualHTTPConfirm && (
                      <Unavailable
                        title={translate(
                          "当前账户没有继续验证此证书订单的权限",
                        )}
                      />
                    )}
                  </section>
                )}
                {order.data.state === "pending_http" && !httpOrder && (
                  <Unavailable
                    title={translate("HTTP挑战响应无效")}
                    detail={translate(
                      "返回内容没有通过证书标识、URL、路径和正文校验。请重新读取订单或检查任务结果。",
                    )}
                  />
                )}
                {!!order.data.records?.length && (
                  <ClientTable>
                    <table className="resource-table">
                      <thead>
                        <tr>
                          <th>{translate("记录名称")}</th>
                          <th>{translate("类型")}</th>
                          <th>{translate("实际值")}</th>
                        </tr>
                      </thead>
                      <tbody>
                        {order.data.records?.map((record, i) => (
                          <tr key={i}>
                            <td>
                              <code>{record.name}</code>
                            </td>
                            <td>{record.type}</td>
                            <td>
                              <code className="wrap-code">{record.value}</code>
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </ClientTable>
                )}
                <ResourceActions
                  capabilities={capabilities}
                  names={(name) =>
                    name ===
                      (order.data?.cancellation_action ??
                        "certificates_acme_cancel") ||
                    (order.data?.state !== "pending_http" &&
                      name ===
                        (order.data?.confirmation_action ??
                          "certificates_acme_confirm"))
                  }
                  preset={{ id: loadedID }}
                  selected={operation}
                  onSelect={setOperation}
                  onExecuted={saved}
                />
              </>
            )}
          </div>
        ) : (
          <>
            <div className="card-heading">
              <h3>{translate("受保护的DNS凭据")}</h3>
              {has("certificates_dns_credentials_save") && (
                <Button
                  label={translate("添加DNS凭据")}
                  onClick={() => {
                    setManualHTTPCreate(false);
                    setCreate("certificates_dns_credentials_save");
                  }}
                />
              )}
            </div>
            {credentials.error && (
              <ResourceError error={credentials.error} state={credentials.state} retry={() => void credentials.reload()} />
            )}
            <div className="instance-list">
              {list(credentials.data).map((item) => (
                <button
                  className="instance-row"
                  key={item.id}
                  onClick={() => {
                    setCredential(item);
                    setOpen(true);
                    setOperation(undefined);
                  }}
                >
                  <div>
                    <strong>{item.name}</strong>
                    <p>
                      {item.provider}· {fullDate(item.updated_at)}
                    </p>
                  </div>
                  <span>
                    {item.credential_configured
                      ? translate("已保存凭据")
                      : translate("未配置凭据")}
                  </span>
                </button>
              ))}
            </div>
            {!list(credentials.data).length && !credentials.loading && (
              <Unavailable
                title={translate("暂无DNS凭据")}
                detail={translate(
                  "凭据保存在服务器，网页和MCP读取列表都不会得到密钥。",
                )}
              />
            )}
            <h3>{translate("DNS提供商")}</h3>
            {providers.error && (
              <ResourceError error={providers.error} state={providers.state} retry={() => void providers.reload()} />
            )}
            <div className="provider-grid">
              {list(providers.data).map((provider) => (
                <div key={provider.id} className="provider-card">
                  <div className="resource-summary">
                    <strong>{provider.name}</strong>
                    <State
                      value={provider.available ? "not_tested" : "unavailable"}
                    />
                  </div>
                  <code>{provider.id}</code>
                  <p className="muted">{provider.reason}</p>
                  <details>
                    <summary>{translate("凭据字段")}</summary>
                    <code className="wrap-code">
                      {provider.fields.join(" · ")}
                    </code>
                  </details>
                </div>
              ))}
            </div>
          </>
        )}
      </Card>
      {selectedFormAction && (
        <ResourceDrawer
          open={!!create}
          title={
            create === "certificates_import"
              ? translate("导入证书")
              : create === "certificates_dns_credentials_save"
                ? translate("添加DNS凭据")
                : manualHTTPCreate
                  ? translate("申请ACME证书 · 手动HTTP验证")
                  : translate("申请ACME证书")
          }
          onClose={() => {
            setCreate(undefined);
            setManualHTTPCreate(false);
          }}
        >
          {manualHTTPCreate && (
            <p className="muted">
              {translate(
                "只填写证书标识、域名和联系人邮箱。此流程不需要网站选择、DNS别名或提供商凭据；验证文件由你放置后再明确继续验证。",
              )}
            </p>
          )}
          <ActionForm
            capability={selectedFormAction}
            preset={{
              expected_revision: 0,
              ...(!manualHTTPCreate && siteID ? { site_id: siteID } : {}),
              ...(create === "certificates_acme_issue"
                ? {
                    challenge: manualHTTPCreate
                      ? "manual_http"
                      : siteID
                        ? "http_webroot"
                        : "manual_dns",
                  }
                : {}),
            }}
            lockedPresetFields={
              manualHTTPCreate ? ["expected_revision", "challenge"] : []
            }
            onExecuted={saved}
            references={references}
          />
        </ResourceDrawer>
      )}
      {activePage === "certificates" && current && (
        <ResourceDrawer
          open={open}
          title={translate("证书 · {0}", [current.name])}
          onClose={() => setOpen(false)}
        >
          <div className="detail-grid">
            <div>
              <small>{translate("域名")}</small>
              <span>{current.domains.join(" · ")}</span>
            </div>
            <div>
              <small>{translate("到期")}</small>
              <span>{fullDate(current.expires_at)}</span>
            </div>
          </div>
          <ResourceActions
            capabilities={capabilities}
            names={(name) =>
              /^certificates_/.test(name) &&
              !name.includes("dns_") &&
              !name.endsWith("_list") &&
              !name.endsWith("_issue") &&
              !name.endsWith("_import") &&
              !name.includes("_confirm") &&
              !name.includes("_cancel") &&
              name !== "certificates_acme_status"
            }
            preset={{
              id: current.id,
              expected_revision: current.revision,
              ...(siteID ? { site_id: siteID } : {}),
            }}
            selected={operation}
            onSelect={setOperation}
            onExecuted={saved}
          />
        </ResourceDrawer>
      )}
      {activePage === "dns" && currentCredential && (
        <ResourceDrawer
          open={open}
          title={translate("DNS凭据 · {0}", [currentCredential.name])}
          onClose={() => setOpen(false)}
        >
          <ResourceActions
            capabilities={capabilities}
            names={(name) =>
              name === "certificates_dns_credentials_save" ||
              name === "certificates_dns_credentials_delete"
            }
            preset={{
              id: currentCredential.id,
              name: currentCredential.name,
              provider: currentCredential.provider,
              expected_revision: currentCredential.revision,
            }}
            selected={operation}
            onSelect={setOperation}
            onExecuted={refresh}
            references={references}
          />
        </ResourceDrawer>
      )}
    </div>
  );
}
