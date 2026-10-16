import type { Capability } from "./api";
import { phase2Capability } from "./database-binlog";

export type FTPDestination = {
  id: string;
  host: string;
  port: number;
  username: string;
  tls_mode: "explicit" | "implicit" | "loopback";
  directory: string;
  revision: number;
};

export type FTPEntry = {
  name: string;
  kind: "file" | "directory" | "link" | "unsupported";
  size: number;
  modified: string;
};

export type FTPDirectory = {
  id: string;
  revision: number;
  path: string;
  items: FTPEntry[];
};

export type FTPDeletePreview = {
  preview_id: string;
  expires_at: string;
  revision: number;
  candidate: {
    path: string;
    kind: "file" | "directory";
    size: number;
    modified: string;
  };
};

const timestamp = (value: unknown): value is string =>
  typeof value === "string" &&
  (value === "" ||
    value === "0001-01-01T00:00:00Z" ||
    Number.isFinite(Date.parse(value)));
const visibleTimestamp = (value: string) =>
  value === "0001-01-01T00:00:00Z" ? "" : value;

type Row = Record<string, unknown>;
const row = (value: unknown): value is Row =>
  !!value && typeof value === "object" && !Array.isArray(value);
const nonnegativeInteger = (value: unknown): value is number =>
  typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
const pathComponent = (value: unknown): value is string =>
  typeof value === "string" &&
  value.length > 0 &&
  value !== "." &&
  value !== ".." &&
  !/[\\/\x00\r\n]/.test(value);

export function ftpCapability(
  capabilities: Capability[],
  name: string,
  scope: "backup:read" | "backup:manage",
  fields: string[],
  task: boolean,
  readOnly?: boolean,
) {
  const capability = phase2Capability(
    capabilities,
    name,
    scope,
    fields,
    task,
    readOnly,
  );
  if (!capability) return undefined;
  const properties: Row = row(capability.input_schema?.properties)
    ? (capability.input_schema.properties as Row)
    : {};
  const password = properties.password;
  const kind = properties.kind;
  if (
    fields.includes("password") &&
    (!row(password) || password.format !== "password")
  )
    return undefined;
  if (
    fields.includes("kind") &&
    (!row(kind) ||
      !Array.isArray(kind.enum) ||
      !["file", "directory"].every((value) =>
        (kind.enum as unknown[]).includes(value),
      ))
  )
    return undefined;
  return capability;
}

export function ftpDestinations(value: unknown): FTPDestination[] | undefined {
  if (!row(value) || !Array.isArray(value.items)) return undefined;
  const destinations: FTPDestination[] = [];
  const seen = new Set<string>();
  for (const entry of value.items) {
    if (
      !row(entry) ||
      typeof entry.id !== "string" ||
      !entry.id ||
      typeof entry.host !== "string" ||
      typeof entry.port !== "number" ||
      !Number.isInteger(entry.port) ||
      entry.port < 1 ||
      entry.port > 65535 ||
      typeof entry.username !== "string" ||
      !["explicit", "implicit", "loopback"].includes(String(entry.tls_mode)) ||
      typeof entry.directory !== "string" ||
      !nonnegativeInteger(entry.revision) ||
      seen.has(entry.id)
    )
      return undefined;
    seen.add(entry.id);
    destinations.push({
      id: entry.id,
      host: entry.host,
      port: entry.port,
      username: entry.username,
      tls_mode: entry.tls_mode as FTPDestination["tls_mode"],
      directory: entry.directory,
      revision: entry.revision,
    });
  }
  return destinations;
}

export function ftpDirectory(
  value: unknown,
  id: string,
  path: string,
  revision: number,
): FTPDirectory | undefined {
  if (!row(value) || !Array.isArray(value.items) || value.items.length > 256)
    return undefined;
  const items: FTPEntry[] = [];
  const seen = new Set<string>();
  for (const entry of value.items) {
    if (
      !row(entry) ||
      !pathComponent(entry.name) ||
      !["file", "directory", "link", "unsupported"].includes(String(entry.kind)) ||
      !nonnegativeInteger(entry.size) ||
      !timestamp(entry.modified) ||
      seen.has(entry.name)
    )
      return undefined;
    seen.add(entry.name);
    items.push({
      name: entry.name,
      kind: entry.kind as FTPEntry["kind"],
      size: entry.size,
      modified: visibleTimestamp(entry.modified),
    });
  }
  if (
    value.id !== id ||
    value.path !== path ||
    value.revision !== revision
  )
    return undefined;
  return { id, path, revision, items };
}

export function ftpChildPath(path: string, name: string) {
  if (!pathComponent(name)) throw new Error("远程名称不是单一路径片段。");
  return path ? `${path}/${name}` : name;
}

export function ftpParentPath(path: string) {
  const parts = path.split("/").filter(Boolean);
  parts.pop();
  return parts.join("/");
}

export function ftpDeletePreview(
  value: unknown,
  expected: { path: string; kind: "file" | "directory"; revision: number },
): FTPDeletePreview | undefined {
  if (!row(value) || !row(value.candidate)) return undefined;
  const candidate = value.candidate;
  if (
    typeof value.preview_id !== "string" ||
    !value.preview_id ||
    typeof value.expires_at !== "string" ||
    !Number.isFinite(Date.parse(value.expires_at)) ||
    !nonnegativeInteger(value.revision) ||
    value.revision !== expected.revision ||
    candidate.path !== expected.path ||
    candidate.kind !== expected.kind ||
    !nonnegativeInteger(candidate.size) ||
    !timestamp(candidate.modified)
  )
    return undefined;
  return {
    preview_id: value.preview_id,
    expires_at: value.expires_at,
    revision: expected.revision,
    candidate: {
      path: candidate.path,
      kind: expected.kind,
      size: candidate.size,
      modified: visibleTimestamp(candidate.modified),
    },
  };
}

export function ftpDeleteInput(
  id: string,
  path: string,
  name: string,
  preview: FTPDeletePreview,
  now = Date.now(),
) {
  if (!id || Date.parse(preview.expires_at) <= now)
    throw new Error("FTP 删除预览已过期，请重新读取并预览。");
  if (ftpChildPath(path, name) !== preview.candidate.path)
    throw new Error("FTP 删除目标与预览不一致，请重新读取。");
  return {
    id,
    path,
    name,
    kind: preview.candidate.kind,
    expected_revision: preview.revision,
    preview_id: preview.preview_id,
  };
}
