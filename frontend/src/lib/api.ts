export type Actor = {
  id: string;
  name: string;
  kind: string;
  scopes?: string[];
  resources?: string[];
};
export type Session = {
  initialized: boolean;
  authenticated: boolean;
  user?: Actor;
  csrf_token?: string;
  panel?: {
    panel_name: string;
    footer: string;
    captcha_enabled: boolean;
    custom_login_entry: boolean;
    maintenance: boolean;
  };
};
export type Note = {
  id: string;
  title: string;
  content: string;
  pinned: boolean;
  archived: boolean;
  target_id: string;
  revision: number;
  created_at: string;
  updated_at: string;
  created_by: Actor;
  updated_by: Actor;
};
export type Revision = Note & { note_id?: string };
export type Task = {
  id: string;
  name?: string;
  action?: string;
  operation?: string;
  target?: string;
  resource?: string;
  state?: string;
  status?: string;
  progress?: number;
  created_at: string;
  updated_at?: string;
  actor?: Actor;
  error?: string;
  result?: unknown;
};
export type Audit = {
  id: string;
  action: string;
  actor: Actor;
  created_at: string;
  status?: string;
  success?: boolean;
  error_code?: string;
  summary?: string;
  error?: string;
  target?: string;
  details?: unknown;
};
export type Capability = {
  name: string;
  description: string;
  read_only?: boolean;
  scope?: string;
  input_schema?: Record<string, unknown>;
  output_schema?: Record<string, unknown>;
  task?: boolean;
};
export type SystemStatus = {
  hostname?: string;
  os?: string;
  kernel?: string;
  architecture?: string;
  cpu_percent?: number | null;
  cpu_cores?: number;
  uptime_seconds?: number | null;
  memory?: {
    total_bytes: number;
    used_bytes: number;
    available_bytes?: number;
  } | null;
  disk?: { total_bytes: number; used_bytes: number } | null;
  sampled_at?: string;
  updated_at?: string;
  available?: boolean;
  [key: string]: unknown;
};
export class APIError extends Error {
  constructor(
    public code: string,
    message: string,
    public status: number,
    public details?: unknown,
  ) {
    super(message);
    this.name = "APIError";
  }
}
let csrf = "";
export function setCSRF(token = "") {
  csrf = token;
}
export async function api<T>(
  path: string,
  options: {
    method?: string;
    body?: unknown;
    signal?: AbortSignal;
    headers?: Record<string, string>;
  } = {},
): Promise<T> {
  const method = options.method ?? "GET";
  const headers: Record<string, string> = { Accept: "application/json" };
  Object.assign(headers, options.headers);
  if (options.body !== undefined) headers["Content-Type"] = "application/json";
  if (method !== "GET" && csrf) headers["X-CSRF-Token"] = csrf;
  let response: Response;
  try {
    response = await fetch(`/api/v1${path}`, {
      method,
      headers,
      credentials: "same-origin",
      cache: "no-store",
      body:
        options.body === undefined ? undefined : JSON.stringify(options.body),
      signal: options.signal,
    });
  } catch (error) {
    if (error instanceof Error && error.name === "AbortError") throw error;
    throw new APIError("network_error", "连接中断，请检查面板服务后重试。", 0);
  }
  let result: {
    data?: T;
    error?: { code?: string; message?: string; details?: unknown };
  };
  try {
    result = await response.json();
  } catch {
    throw new APIError(
      "invalid_response",
      "服务返回了无法读取的响应。",
      response.status,
    );
  }
  if (!response.ok || result.error)
    throw new APIError(
      result.error?.code ?? "request_failed",
      result.error?.message ?? `请求失败 (${response.status})`,
      response.status,
      result.error?.details,
    );
  return result.data as T;
}
export function intentKey(): string {
  if (typeof crypto.randomUUID === "function") return crypto.randomUUID();
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) =>
    b.toString(16).padStart(2, "0"),
  ).join("");
}
export function action<T>(
  name: string,
  input: unknown = {},
  options: { signal?: AbortSignal; idempotencyKey?: string } = {},
): Promise<T> {
  return api<T>(`/actions/${encodeURIComponent(name)}`, {
    method: "POST",
    body: input,
    signal: options.signal,
    headers: options.idempotencyKey
      ? { "Idempotency-Key": options.idempotencyKey }
      : undefined,
  });
}
export function streamHeaders(
  extra: Record<string, string> = {},
): Record<string, string> {
  return { ...extra, ...(csrf ? { "X-CSRF-Token": csrf } : {}) };
}
export async function streamRequest(
  path: string,
  options: RequestInit = {},
): Promise<Response> {
  let response: Response;
  try {
    response = await fetch(path, {
      ...options,
      headers: streamHeaders(
        options.headers as Record<string, string> | undefined,
      ),
      credentials: "same-origin",
      cache: "no-store",
    });
  } catch (e) {
    if (e instanceof Error && e.name === "AbortError") throw e;
    throw new APIError(
      "network_error",
      "连接中断，请保留文件或草稿后重试。",
      0,
    );
  }
  if (response.ok) return response;
  const error = await response.json().catch(() => ({
    error: {
      code: "request_failed",
      message: `请求失败 (${response.status})`,
    },
  }));
  throw new APIError(
    error.error?.code ?? "request_failed",
    error.error?.message ?? "传输失败",
    response.status,
    error.error?.details,
  );
}
export function list<T>(
  value:
    | T[]
    | { items?: T[]; notes?: T[]; tasks?: T[]; entries?: T[]; actions?: T[] }
    | undefined,
): T[] {
  if (Array.isArray(value)) return value;
  return (
    value?.items ??
    value?.notes ??
    value?.tasks ??
    value?.entries ??
    value?.actions ??
    []
  );
}
export const source = (actor?: Actor) =>
  actor
    ? `${actor.kind === "human" ? "人" : actor.kind === "mcp" ? "AI" : "客户端"} · ${actor.name}`
    : "来源未提供";
export function fullDate(value?: string) {
  if (!value) return "—";
  const d = new Date(value);
  return Number.isNaN(d.valueOf())
    ? "—"
    : new Intl.DateTimeFormat(undefined, {
        dateStyle: "medium",
        timeStyle: "short",
      }).format(d);
}
export function bytes(value?: number) {
  if (value === undefined || !Number.isFinite(value)) return "—";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  while (value >= 1024 && i < units.length - 1) {
    value /= 1024;
    i++;
  }
  return `${value.toFixed(i ? 1 : 0)} ${units[i]}`;
}
export function duration(seconds?: number | null) {
  if (seconds == null) return "—";
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const mins = Math.floor((seconds % 3600) / 60);
  return days ? `${days} 天 ${hours} 小时` : `${hours} 小时 ${mins} 分钟`;
}
