import type { Capability } from "./api";

export type SitePHPSlowlog = {
  site_id: string;
  site_revision: number;
  php_instance: string;
  configured: boolean;
  generated: boolean;
  lines: string[];
  truncated: boolean;
};

export type SitePHPSessionStats = {
  site_id: string;
  site_revision: number;
  php_instance: string;
  generated: boolean;
  total_sessions: number;
  expired_sessions: number;
  older_than_seconds: number;
  oldest_age_seconds?: number;
  allow_actions: string[];
};

export type SitePHPSessionPreview = {
  site_id: string;
  site_revision: number;
  php_instance: string;
  preview_id: string;
  captured_at: string;
  expires_at: string;
  total_sessions: number;
  candidate_count: number;
  candidate_bytes: number;
  older_than_seconds: number;
  allow_actions: string[];
};

const isRecord = (value: unknown): value is Record<string, unknown> =>
  !!value && typeof value === "object" && !Array.isArray(value);
const safeCount = (value: unknown): value is number =>
  typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
const validRevision = (value: unknown): value is number =>
  typeof value === "number" && Number.isSafeInteger(value) && value > 0;
const validDate = (value: unknown) =>
  typeof value === "string" && Number.isFinite(Date.parse(value));
const safeActions = (value: unknown, allowed: string[]) =>
  Array.isArray(value) && value.every((entry) => typeof entry === "string")
    ? [...new Set(value.filter((entry) => allowed.includes(entry)))]
    : undefined;

export function sitePHPCapability(
  capabilities: Capability[],
  name: string,
  scope: "sites:read" | "sites:write",
  required: string[],
  task: boolean,
  readOnly: boolean,
  allowedFields: string[] = required,
): Capability | undefined {
  return capabilities.find((capability) => {
    if (
      capability.name !== name ||
      capability.scope !== scope ||
      capability.task !== task ||
      capability.read_only !== readOnly
    )
      return false;
    const schema = capability.input_schema;
    const properties = schema?.properties;
    const schemaRequired = schema?.required;
    return (
      isRecord(properties) &&
      allowedFields.every((field) => Object.prototype.hasOwnProperty.call(properties, field)) &&
      Array.isArray(schemaRequired) &&
      required.every((field) => schemaRequired.includes(field)) &&
      schemaRequired.every((field) => typeof field === "string" && allowedFields.includes(field))
    );
  });
}

export function sitePHPSlowlog(
  value: unknown,
  siteID: string,
  siteRevision: number,
  phpInstance: string,
): SitePHPSlowlog | undefined {
  if (!isRecord(value)) return;
  if (
    value.site_id !== siteID ||
    value.site_revision !== siteRevision ||
    !validRevision(value.site_revision) ||
    value.php_instance !== phpInstance ||
    typeof value.configured !== "boolean" ||
    typeof value.generated !== "boolean" ||
    !Array.isArray(value.lines) ||
    value.lines.length > 500 ||
    !value.lines.every((line) => typeof line === "string") ||
    typeof value.truncated !== "boolean" ||
    (!value.generated && value.lines.length > 0) ||
    (!value.configured && (value.generated || value.lines.length > 0))
  )
    return;
  return {
    site_id: siteID,
    site_revision: siteRevision,
    php_instance: phpInstance,
    configured: value.configured,
    generated: value.generated,
    lines: [...value.lines] as string[],
    truncated: value.truncated,
  };
}

export function sitePHPSessionStats(
  value: unknown,
  siteID: string,
  siteRevision: number,
  phpInstance: string,
): SitePHPSessionStats | undefined {
  if (!isRecord(value)) return;
  const allowActions = safeActions(value.allow_actions, ["preview"]);
  if (
    value.site_id !== siteID ||
    value.site_revision !== siteRevision ||
    !validRevision(value.site_revision) ||
    value.php_instance !== phpInstance ||
    typeof value.generated !== "boolean" ||
    !safeCount(value.total_sessions) ||
    !safeCount(value.expired_sessions) ||
    value.expired_sessions > value.total_sessions ||
    !safeCount(value.older_than_seconds) ||
    value.older_than_seconds < 3600 ||
    value.older_than_seconds > 31536000 ||
    (!value.generated && value.total_sessions > 0) ||
    (value.oldest_age_seconds !== undefined && !safeCount(value.oldest_age_seconds)) ||
    !allowActions
  )
    return;
  return {
    site_id: siteID,
    site_revision: siteRevision,
    php_instance: phpInstance,
    generated: value.generated,
    total_sessions: value.total_sessions,
    expired_sessions: value.expired_sessions,
    older_than_seconds: value.older_than_seconds,
    ...(value.oldest_age_seconds === undefined
      ? {}
      : { oldest_age_seconds: value.oldest_age_seconds as number }),
    allow_actions: allowActions,
  };
}

export function sitePHPSessionPreview(
  value: unknown,
  siteID: string,
  siteRevision: number,
  phpInstance: string,
): SitePHPSessionPreview | undefined {
  if (!isRecord(value)) return;
  const allowActions = safeActions(value.allow_actions, ["cleanup"]);
  if (
    value.site_id !== siteID ||
    value.site_revision !== siteRevision ||
    !validRevision(value.site_revision) ||
    value.php_instance !== phpInstance ||
    typeof value.preview_id !== "string" ||
    !/^spv_[a-f0-9]{48}$/.test(value.preview_id) ||
    !validDate(value.captured_at) ||
    !validDate(value.expires_at) ||
    !safeCount(value.total_sessions) ||
    !safeCount(value.candidate_count) ||
    value.candidate_count > value.total_sessions ||
    !safeCount(value.candidate_bytes) ||
    !safeCount(value.older_than_seconds) ||
    value.older_than_seconds < 3600 ||
    value.older_than_seconds > 31536000 ||
    (value.candidate_count === 0 && allowActions?.includes("cleanup")) ||
    !allowActions
  )
    return;
  return {
    site_id: siteID,
    site_revision: siteRevision,
    php_instance: phpInstance,
    preview_id: value.preview_id,
    captured_at: value.captured_at as string,
    expires_at: value.expires_at as string,
    total_sessions: value.total_sessions,
    candidate_count: value.candidate_count,
    candidate_bytes: value.candidate_bytes,
    older_than_seconds: value.older_than_seconds,
    allow_actions: allowActions,
  };
}

export function sitePHPSessionPreviewExpired(
  preview: SitePHPSessionPreview,
  now = Date.now(),
): boolean {
  const expiry = Date.parse(preview.expires_at);
  return !Number.isFinite(expiry) || expiry <= now;
}

export function sitePHPSessionCleanupInput(
  preview: SitePHPSessionPreview,
  currentSiteRevision: number,
  now = Date.now(),
): { id: string; expected_revision: number; preview_id: string } {
  if (
    !preview.allow_actions.includes("cleanup") ||
    preview.site_revision !== currentSiteRevision ||
    preview.candidate_count < 1 ||
    sitePHPSessionPreviewExpired(preview, now)
  )
    throw new Error("网站会话预览已过期或版本已变化，请重新读取并预览。");
  return {
    id: preview.site_id,
    expected_revision: preview.site_revision,
    preview_id: preview.preview_id,
  };
}
