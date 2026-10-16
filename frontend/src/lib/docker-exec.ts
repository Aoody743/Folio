import type { Capability } from "./api";
import { phase2Capability } from "./database-binlog";

export type DockerExecInput = {
  id: string;
  name: string;
  argv: string[];
  env?: Record<string, string>;
  working_dir?: string;
  stdin?: string;
  timeout_seconds?: number;
  output_limit_bytes?: number;
};

export type DockerExecOutput = {
  stdout: string;
  stderr: string;
  exit_code: number;
  truncated: boolean;
};

type Row = Record<string, unknown>;
const row = (value: unknown): value is Row =>
  !!value && typeof value === "object" && !Array.isArray(value);
const encoder = new TextEncoder();
const byteLength = (value: string) => encoder.encode(value).byteLength;

export function dockerExecCapability(capabilities: Capability[]) {
  const capability = phase2Capability(
    capabilities,
    "docker_container_exec",
    "containers:manage",
    ["id", "name", "argv"],
    true,
    false,
  );
  const properties = capability?.input_schema?.properties;
  if (!capability || !properties || typeof properties !== "object" || Array.isArray(properties))
    return undefined;
  const propertyMap = properties as Record<string, unknown>;
  const argv = propertyMap.argv as Row | undefined;
  const items = argv?.items as Row | undefined;
  if (
    argv?.type !== "array" ||
    items?.type !== "string" ||
    items.maxLength !== 4096 ||
    argv.minItems !== 1 ||
    argv.maxItems !== 16 ||
    (propertyMap.name as Row | undefined)?.pattern !== "^[a-z][a-z0-9_-]{0,31}$" ||
    !["env", "working_dir", "stdin", "timeout_seconds", "output_limit_bytes"].every(
      (field) => Object.prototype.hasOwnProperty.call(propertyMap, field),
    )
  )
    return undefined;
  return capability;
}

export function dockerExecInput(
  values: Omit<DockerExecInput, "id" | "name">,
  id: string,
  name: string,
): DockerExecInput {
  if (!id || !/^[a-z][a-z0-9_-]{0,31}$/.test(name))
    throw new Error("当前容器不是有效的面板受管容器。");
  if (
    !Array.isArray(values.argv) ||
    values.argv.length < 1 ||
    values.argv.length > 16 ||
    values.argv.some(
      (item) =>
        typeof item !== "string" ||
        byteLength(item) < 1 ||
        byteLength(item) > 4096,
    ) ||
    values.argv.reduce((sum, item) => sum + byteLength(item), 0) > 16 * 1024
  )
    throw new Error("请填写 1 到 16 个参数项，每项最多 4096 字节，总计最多 16 KiB。");

  const result: DockerExecInput = { id, name, argv: [...values.argv] };
  if (values.env && Object.keys(values.env).length) {
    const env = values.env;
    if (
      Object.keys(env).length > 16 ||
      Object.entries(env).some(
        ([key, value]) =>
          !/^[A-Za-z_][A-Za-z0-9_]{0,63}$/.test(key) ||
          typeof value !== "string" ||
          byteLength(value) > 4096,
      ) ||
      Object.entries(env).reduce(
        (sum, [key, value]) => sum + byteLength(key) + byteLength(value),
        0,
      ) > 4 * 1024
    )
      throw new Error("环境变量最多 16 项；键名和每项大小必须符合服务端限制。");
    result.env = { ...env };
  }
  if (values.working_dir) {
    if (
      byteLength(values.working_dir) > 256 ||
      !values.working_dir.startsWith("/") ||
      values.working_dir.includes("\\") ||
      values.working_dir.includes("\0") ||
      values.working_dir.split("/").some((part) => part === "." || part === "..") ||
      values.working_dir.includes("//")
    )
      throw new Error("工作目录必须是最长 256 字节的规范绝对路径。");
    result.working_dir = values.working_dir;
  }
  if (values.stdin !== undefined && values.stdin !== "") {
    if (byteLength(values.stdin) > 16 * 1024 || values.stdin.includes("\0"))
      throw new Error("标准输入最多 16 KiB，且不能包含 NUL 字符。");
    result.stdin = values.stdin;
  }
  if (values.timeout_seconds !== undefined) {
    if (
      !Number.isInteger(values.timeout_seconds) ||
      values.timeout_seconds < 1 ||
      values.timeout_seconds > 120
    )
      throw new Error("执行超时必须在 1 到 120 秒之间。");
    result.timeout_seconds = values.timeout_seconds;
  }
  if (values.output_limit_bytes !== undefined) {
    if (
      !Number.isInteger(values.output_limit_bytes) ||
      values.output_limit_bytes < 1 ||
      values.output_limit_bytes > 65536
    )
      throw new Error("输出限制必须在 1 到 65536 字节之间。");
    result.output_limit_bytes = values.output_limit_bytes;
  }
  return result;
}

export function dockerExecTaskID(value: unknown): string | undefined {
  if (!row(value)) return undefined;
  const id = typeof value.id === "string" ? value.id : value.task_id;
  return typeof id === "string" && id ? id : undefined;
}

export function dockerExecOutput(value: unknown): DockerExecOutput | undefined {
  const candidate = !row(value)
    ? undefined
    : row(value.result)
      ? value.result
      : typeof value.stdout === "string"
        ? value
        : row(value.data)
          ? value.data
          : undefined;
  if (
    !candidate ||
    typeof candidate.stdout !== "string" ||
    typeof candidate.stderr !== "string" ||
    typeof candidate.exit_code !== "number" ||
    !Number.isInteger(candidate.exit_code) ||
    typeof candidate.truncated !== "boolean"
  )
    return undefined;
  return {
    stdout: candidate.stdout,
    stderr: candidate.stderr,
    exit_code: candidate.exit_code,
    truncated: candidate.truncated,
  };
}

export type DockerExecRecovery = {
  execution_id: string;
  execution_state: "running" | "unknown";
};

export type DockerExecStatus = {
  execution_id: string;
  execution_state: "running" | "completed";
  running: boolean;
  exit_code?: number;
};

export function dockerExecRecovery(value: unknown): DockerExecRecovery | undefined {
  const source = row(value) && row(value.data) ? value.data : value;
  if (
    !row(source) ||
    typeof source.execution_id !== "string" ||
    !source.execution_id ||
    !["running", "unknown"].includes(String(source.execution_state))
  )
    return undefined;
  return {
    execution_id: source.execution_id,
    execution_state: source.execution_state as DockerExecRecovery["execution_state"],
  };
}

export function dockerExecStatus(value: unknown): DockerExecStatus | undefined {
  if (
    !row(value) ||
    typeof value.execution_id !== "string" ||
    !value.execution_id ||
    !["running", "completed"].includes(String(value.execution_state)) ||
    typeof value.running !== "boolean" ||
    value.running !== (value.execution_state === "running") ||
    (value.exit_code !== undefined &&
      (typeof value.exit_code !== "number" || !Number.isInteger(value.exit_code)))
  )
    return undefined;
  return {
    execution_id: value.execution_id,
    execution_state: value.execution_state as DockerExecStatus["execution_state"],
    running: value.running,
    ...(typeof value.exit_code === "number" ? { exit_code: value.exit_code } : {}),
  };
}
