import { t as translate } from "./i18n";
import type { Capability } from "./api";

export type PHPMaintenanceLog = {
  id: string;
  stream: "fpm_error" | "slow";
  configured: boolean;
  generated: boolean;
  lines: string[];
  revision: number;
  truncated: boolean;
};

export type PHPWorkerSessionStats = {
  id: string;
  revision: number;
  generated: boolean;
  total_sessions: number;
  expired_sessions: number;
  older_than_seconds: number;
  oldest_age_seconds?: number;
  allow_actions: string[];
};

export type PHPWorkerSessionPreview = {
  id: string;
  revision: number;
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
const actions = (value: unknown, allowed: string[]) =>
  Array.isArray(value) && value.every((item) => typeof item === "string")
    ? [...new Set(value.filter((item) => allowed.includes(item)))]
    : undefined;

export function phpMaintenanceCapability(
  capabilities: Capability[],
  name: string,
  scope: "php:read" | "php:manage",
  fields: string[],
  task: boolean,
  readOnly = !task,
  streamName?: "fpm_error" | "slow",
): Capability | undefined {
  return capabilities.find((capability) => {
    if (
      capability.name !== name ||
      capability.scope !== scope ||
      (capability.task === true) !== task ||
      capability.read_only !== readOnly
    )
      return false;
    const properties = capability.input_schema?.properties;
    if (!isRecord(properties) || !fields.every((field) => field in properties))
      return false;
    if (name === "php_maintenance_log_read") {
      const stream = properties.stream;
      return (
        isRecord(stream) &&
        Array.isArray(stream.enum) &&
        stream.enum.includes(streamName ?? "fpm_error") &&
        (!fields.includes("limit") || isRecord(properties.limit))
      );
    }
    return true;
  });
}

export function phpMaintenanceLog(
  value: unknown,
  instanceID: string,
  streamName: "fpm_error" | "slow" = "fpm_error",
): PHPMaintenanceLog | undefined {
  if (!isRecord(value)) return;
  const configured = value.configured;
  if (
    value.id !== instanceID ||
    value.stream !== streamName ||
    (streamName === "slow" && typeof configured !== "boolean") ||
    (configured !== undefined && typeof configured !== "boolean") ||
    typeof value.generated !== "boolean" ||
    !Array.isArray(value.lines) ||
    value.lines.length > 500 ||
    !value.lines.every((line) => typeof line === "string") ||
    !safeCount(value.revision) ||
    typeof value.truncated !== "boolean" ||
    (!value.generated && value.lines.length > 0) ||
    (configured === false && (value.generated || value.lines.length > 0))
  )
    return;
  return {
    id: instanceID,
    stream: streamName,
    configured: configured !== false,
    generated: value.generated,
    lines: [...value.lines] as string[],
    revision: value.revision,
    truncated: value.truncated,
  };
}

export function phpWorkerSessionStats(
  value: unknown,
  instanceID: string,
): PHPWorkerSessionStats | undefined {
  if (!isRecord(value)) return;
  const allowActions = actions(value.allow_actions, ["preview"]);
  if (
    value.id !== instanceID ||
    !safeCount(value.revision) ||
    typeof value.generated !== "boolean" ||
    !safeCount(value.total_sessions) ||
    !safeCount(value.expired_sessions) ||
    value.expired_sessions > value.total_sessions ||
    !safeCount(value.older_than_seconds) ||
    value.older_than_seconds < 3600 ||
    value.older_than_seconds > 31536000 ||
    (value.oldest_age_seconds !== undefined &&
      !safeCount(value.oldest_age_seconds)) ||
    !allowActions
  )
    return;
  return {
    id: instanceID,
    revision: value.revision,
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

export function phpWorkerSessionPreview(
  value: unknown,
  instanceID: string,
  expectedRevision: number,
): PHPWorkerSessionPreview | undefined {
  if (!isRecord(value)) return;
  const allowActions = actions(value.allow_actions, ["cleanup"]);
  if (
    value.id !== instanceID ||
    value.revision !== expectedRevision ||
    !safeCount(value.revision) ||
    typeof value.preview_id !== "string" ||
    !value.preview_id ||
    value.preview_id.length > 256 ||
    typeof value.captured_at !== "string" ||
    !Number.isFinite(Date.parse(value.captured_at)) ||
    typeof value.expires_at !== "string" ||
    !Number.isFinite(Date.parse(value.expires_at)) ||
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
    id: instanceID,
    revision: value.revision,
    preview_id: value.preview_id,
    captured_at: value.captured_at,
    expires_at: value.expires_at,
    total_sessions: value.total_sessions,
    candidate_count: value.candidate_count,
    candidate_bytes: value.candidate_bytes,
    older_than_seconds: value.older_than_seconds,
    allow_actions: allowActions,
  };
}

export function phpWorkerPreviewExpired(
  preview: PHPWorkerSessionPreview,
  now = Date.now(),
): boolean {
  const expiresAt = Date.parse(preview.expires_at);
  return !Number.isFinite(expiresAt) || expiresAt <= now;
}

export function phpWorkerCleanupInput(
  preview: PHPWorkerSessionPreview,
  now = Date.now(),
): { id: string; expected_revision: number; preview_id: string } {
  if (
    !preview.allow_actions.includes("cleanup") ||
    preview.candidate_count < 1 ||
    phpWorkerPreviewExpired(preview, now)
  )
    throw new Error(
      translate("清理预览不可用或已过期，请重新读取状态并预览。"),
    );
  return {
    id: preview.id,
    expected_revision: preview.revision,
    preview_id: preview.preview_id,
  };
}
