"use client";
import { useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { Capability, list } from "@/lib/api";
import { useResource } from "@/lib/resource-hook";
import { projectInput, Schema } from "@/lib/schema";
import { ActionForm } from "./actions";
import { State, Unavailable } from "./resource-ui";
type Configuration = {
  id: string;
  revision: number;
  values: Record<string, unknown>;
  worker_isolation?: string;
};
export function PHPResources({
  id,
  capabilities,
  onExecuted,
  active = true,
}: {
  id: string;
  capabilities: Capability[];
  onExecuted: () => void;
  active?: boolean;
}) {
  const [tab, setTab] = useState("extensions");
  const [selected, setSelected] = useState<{
    name: string;
    enabled: boolean;
  }>();
  const has = (name: string) => capabilities.some((c) => c.name === name);
  const modules = useResource<{ items: string[]; revision: number }>(
    "php_extensions",
    { id },
    active && has("php_extensions"),
  );
  const configuration = useResource<Configuration>(
    "php_configuration",
    { id },
    active && has("php_configuration"),
  );
  const status = useResource<{
    version: string;
    health: { status: string; reason?: string };
  }>("php_status", { id }, active && has("php_status"));
  const toggle = capabilities.find((c) => c.name === "php_extension_toggle");
  const configure = capabilities.find((c) => c.name === "php_configure");
  const schema = toggle?.input_schema?.properties as
    | Record<string, Schema>
    | undefined;
  const approved = (schema?.extension?.enum ?? []).filter(
    (name): name is string => typeof name === "string",
  );
  const normalize = (name: string) =>
    name.toLowerCase() === "zend opcache" ? "opcache" : name.toLowerCase();
  const loaded = new Set(list(modules.data).map(normalize));
  const names = modules.data
    ? [...new Set([...approved, ...list(modules.data).map(normalize)])].sort()
    : [];
  const refresh = () => {
    void modules.reload();
    void configuration.reload();
    void status.reload();
    onExecuted();
  };
  return (
    <section className="stack" aria-label="PHP运行环境">
      <div className="card-heading">
        <h3>PHP运行环境</h3>
        {status.data && (
          <State
            value={status.data.health.status}
            reason={status.data.health.reason}
          />
        )}
      </div>
      {status.error && (
        <div className="notice danger" role="alert">
          {status.error}
        </div>
      )}
      {status.data?.version && (
        <pre className="runtime-version">{status.data.version}</pre>
      )}
      <div className="section-tabs" role="tablist" aria-label="PHP管理分类">
        {[
          { id: "extensions", label: "扩展模块" },
          { id: "configuration", label: "运行参数" },
        ].map((item) => (
          <button
            role="tab"
            key={item.id}
            aria-selected={tab === item.id}
            onClick={() => setTab(item.id)}
          >
            {item.label}
          </button>
        ))}
      </div>
      {tab === "extensions" ? (
        <>
          {modules.error && (
            <div className="notice danger" role="alert">
              {modules.error}
            </div>
          )}
          {!has("php_extensions") ? (
            <Unavailable title="当前没有扩展读取权限" />
          ) : modules.data ? (
            <>
              <p className="muted">
                载入状态来自该运行环境。启用前会检查模块文件与实际载入结果；内置模块不能通过开关卸载。
              </p>
              <div className="extension-grid">
                {names.map((name) => (
                  <div className="extension-row" key={name}>
                    <div>
                      <strong>{name}</strong>
                      <small>{loaded.has(name) ? "已载入" : "未载入"}</small>
                    </div>
                    {toggle && approved.includes(name) && (
                      <Button
                        label={`${loaded.has(name) ? "停用" : "启用"} ${name}`}
                        variant="ghost"
                        onClick={() =>
                          setSelected({ name, enabled: !loaded.has(name) })
                        }
                      />
                    )}
                  </div>
                ))}
              </div>
              {selected && toggle && (
                <ActionForm
                  key={id + selected.name + selected.enabled}
                  capability={toggle}
                  preset={{
                    id,
                    extension: selected.name,
                    enabled: selected.enabled,
                    expected_revision: modules.data.revision,
                  }}
                  onBack={() => setSelected(undefined)}
                  onExecuted={refresh}
                />
              )}
            </>
          ) : (
            <p role="status">正在读取实际扩展…</p>
          )}
        </>
      ) : (
        <>
          {configuration.error && (
            <div className="notice danger" role="alert">
              {configuration.error}
            </div>
          )}
          {configure && configuration.data ? (
            <ActionForm
              capability={configure}
              preset={
                projectInput(configure.input_schema as Schema, {
                  id,
                  expected_revision: configuration.data.revision,
                  values: configuration.data.values,
                }) as Record<string, unknown>
              }
              onExecuted={refresh}
            />
          ) : (
            <Unavailable title="运行参数尚未读取，或没有配置权限" />
          )}
        </>
      )}
    </section>
  );
}
