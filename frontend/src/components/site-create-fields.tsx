"use client";
import { useEffect, useState } from "react";
import { TextInput } from "@astryxdesign/core/TextInput";
import { TextArea } from "@astryxdesign/core/TextArea";
import { t as translate } from "@/lib/i18n";
import { Schema } from "@/lib/schema";
import { domainLines, websiteIdentity } from "@/lib/resource-identity";
import { Field, References } from "./actions";

/** Controlled fields share ActionForm's draft, confirmation and idempotency handling. */
export function SiteCreateFields({ schema, value, onChange, references }: {
  schema: Schema; value: unknown; onChange: (value: unknown) => void; references: References;
}) {
  const draft = (value && typeof value === "object" && !Array.isArray(value) ? value : {}) as Record<string, unknown>;
  const domains = Array.isArray(draft.domains) ? draft.domains.filter((item): item is string => typeof item === "string") : [];
  const serialized = domains.join("\n");
  const [text, setText] = useState(serialized);
  useEffect(() => {
    setText(previous => domainLines(previous).join("\n") === serialized ? previous : serialized);
  }, [serialized]);
  const set = (key: string, next: unknown) => onChange({ ...draft, [key]: next });
  const known = new Set(["id", "name", "domains", "web_instance", "php_instance", "expected_revision"]);
  const extra = (required: boolean): Schema => ({
    ...schema,
    properties: Object.fromEntries(Object.entries(schema.properties ?? {}).filter(([key]) => !known.has(key) && (schema.required?.includes(key) ?? false) === required)),
    required: required ? schema.required?.filter(key => !known.has(key)) : [],
  });
  const requiredExtras = extra(true), optionalExtras = extra(false);
  return (
    <div className="business-fields">
      <TextArea
        label={translate("网站域名")}
        description={translate("每行一个域名，也可以用空格或逗号分隔。")}
        rows={3}
        isRequired
        value={text}
        placeholder="example.com\nwww.example.com"
        onChange={nextText => {
          setText(nextText);
          const nextDomains = domainLines(nextText);
          const automatic = !draft.id || draft.id === websiteIdentity(domains[0] ?? "");
          onChange({ ...draft, domains: nextDomains,
            ...(automatic ? { id: websiteIdentity(nextDomains[0] ?? "") } : {}),
            ...(!draft.name || draft.name === domains[0] ? { name: nextDomains[0] ?? "" } : {}),
          });
        }}
      />
      <TextInput label={translate("网站名称")} value={String(draft.name ?? "")} onChange={next => set("name", next)} isRequired />
      <div className="business-field-pair">
        <Field schema={schema.properties?.web_instance ?? { type: "string" }} value={draft.web_instance ?? ""} onChange={next => set("web_instance", next)} label={translate("网站服务")} fieldKey="web_instance" references={references} required />
        {schema.properties?.php_instance && <Field schema={schema.properties.php_instance} value={draft.php_instance ?? ""} onChange={next => set("php_instance", next)} label={translate("PHP 运行环境")} fieldKey="php_instance" references={references} />}
      </div>
      {Object.keys(requiredExtras.properties ?? {}).length > 0 && <Field schema={requiredExtras} value={draft} onChange={onChange} label={translate("网站设置")} fieldKey="" references={references} />}
      <details className="optional-options">
        <summary>{translate("可选设置")}</summary>
        <TextInput label={translate("资源标识")} value={String(draft.id ?? "")} onChange={next => set("id", next)} description={translate("已根据域名生成；同名冲突时可以修改。")}/>
        {Object.keys(optionalExtras.properties ?? {}).length > 0 && <Field schema={optionalExtras} value={draft} onChange={onChange} label={translate("网站设置")} fieldKey="" references={references} />}
      </details>
    </div>
  );
}
