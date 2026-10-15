import type { Capability } from "./api";

export type SupervisorJob = { name: string; program_id?: string; state?: string };
export type SupervisorFileIdentity = {
  exists: boolean;
  size: string;
  mod_time_ns: string;
  device: string;
  inode: string;
};
export type SupervisorLogRead = {
  id: string;
  name: string;
  stream: "stdout" | "stderr";
  text: string;
  bytes: number;
  truncated: boolean;
  file_identity: SupervisorFileIdentity;
};
export type SupervisorLogClearPreview = {
  id: string;
  name: string;
  revision: number;
  stdout: SupervisorFileIdentity;
  stderr: SupervisorFileIdentity;
  clears_streams: ["stdout", "stderr"];
};

const isRecord = (value: unknown): value is Record<string, unknown> =>
  !!value && typeof value === "object" && !Array.isArray(value);
const safeRevision = (value: unknown): value is number =>
  typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
const decimal = (value: unknown, maxDigits: number): value is string =>
  typeof value === "string" &&
  value.length <= maxDigits &&
  /^(0|[1-9][0-9]*)$/.test(value);
const signedDecimal = (value: unknown): value is string =>
  typeof value === "string" &&
  value.length <= 20 &&
  /^(0|-?[1-9][0-9]{0,18})$/.test(value);

export function supervisorCapability(
  capabilities: Capability[],
  name: string,
  scope: "supervisor:read" | "supervisor:manage",
  fields: string[],
  task = false,
): Capability | undefined {
  return capabilities.find((capability) => {
    if (
      capability.name !== name ||
      capability.scope !== scope ||
      (capability.task === true) !== task ||
      capability.read_only !== !task
    )
      return false;
    const properties = capability.input_schema?.properties;
    if (!isRecord(properties) || !fields.every((field) => field in properties))
      return false;
    if (name === "supervisor_log_read") {
      const stream = properties.stream;
      return (
        isRecord(stream) &&
        Array.isArray(stream.enum) &&
        stream.enum.includes("stdout") &&
        stream.enum.includes("stderr")
      );
    }
    if (name === "supervisor_log_clear") {
      const patterns: Record<string, string> = {
        size: "^(0|[1-9][0-9]{0,18})$",
        mod_time_ns: "^(0|-?[1-9][0-9]{0,18})$",
        device: "^(0|[1-9][0-9]{0,19})$",
        inode: "^(0|[1-9][0-9]{0,19})$",
      };
      return ["expected_stdout", "expected_stderr"].every((field) => {
        const identity = properties[field];
        const identityProperties = isRecord(identity)
          ? identity.properties
          : undefined;
        const required = isRecord(identity) ? identity.required : undefined;
        return (
          isRecord(identity) &&
          identity.type === "object" &&
          isRecord(identityProperties) &&
          isRecord(identityProperties.exists) &&
          identityProperties.exists.type === "boolean" &&
          ["size", "mod_time_ns", "device", "inode"].every(
            (field) =>
              isRecord(identityProperties[field]) &&
              identityProperties[field].type === "string" &&
              identityProperties[field].pattern === patterns[field],
          ) &&
          Array.isArray(required) &&
          ["exists", "size", "mod_time_ns", "device", "inode"].every(
            (field) => required.includes(field),
          )
        );
      });
    }
    return true;
  });
}

export function supervisorJobs(value: unknown):
  | { items: SupervisorJob[]; revision: number }
  | undefined {
  if (!isRecord(value) || !Array.isArray(value.items) || !safeRevision(value.revision))
    return;
  const items: SupervisorJob[] = [];
  const seen = new Set<string>();
  for (const raw of value.items) {
    if (!isRecord(raw) || typeof raw.name !== "string" || !raw.name) return;
    if (seen.has(raw.name)) return;
    seen.add(raw.name);
    items.push({
      name: raw.name,
      ...(typeof raw.program_id === "string" ? { program_id: raw.program_id } : {}),
      ...(typeof raw.state === "string" ? { state: raw.state } : {}),
    });
  }
  return { items, revision: value.revision };
}

export function supervisorFileIdentity(
  value: unknown,
): SupervisorFileIdentity | undefined {
  if (
    !isRecord(value) ||
    typeof value.exists !== "boolean" ||
    !decimal(value.size, 19) ||
    !signedDecimal(value.mod_time_ns) ||
    !decimal(value.device, 20) ||
    !decimal(value.inode, 20) ||
    (!value.exists &&
      (value.size !== "0" ||
        value.mod_time_ns !== "0" ||
        value.device !== "0" ||
        value.inode !== "0")) ||
    (value.exists && value.inode === "0")
  )
    return;
  return {
    exists: value.exists,
    size: value.size,
    mod_time_ns: value.mod_time_ns,
    device: value.device,
    inode: value.inode,
  };
}

export function supervisorLogRead(
  value: unknown,
  id: string,
  name: string,
  stream: "stdout" | "stderr",
): SupervisorLogRead | undefined {
  if (!isRecord(value)) return;
  const identity = supervisorFileIdentity(value.file_identity);
  if (
    value.id !== id ||
    value.name !== name ||
    value.stream !== stream ||
    typeof value.text !== "string" ||
    value.text.length > 65536 ||
    typeof value.bytes !== "number" ||
    !Number.isSafeInteger(value.bytes) ||
    value.bytes < 0 ||
    value.bytes > 65536 ||
    typeof value.truncated !== "boolean" ||
    !identity
  )
    return;
  return {
    id,
    name,
    stream,
    text: value.text,
    bytes: value.bytes,
    truncated: value.truncated,
    file_identity: identity,
  };
}

export function supervisorLogClearPreview(
  value: unknown,
  id: string,
  name: string,
): SupervisorLogClearPreview | undefined {
  if (!isRecord(value)) return;
  const stdout = supervisorFileIdentity(value.stdout);
  const stderr = supervisorFileIdentity(value.stderr);
  if (
    value.id !== id ||
    value.name !== name ||
    !safeRevision(value.revision) ||
    !stdout ||
    !stderr ||
    !Array.isArray(value.clears_streams) ||
    value.clears_streams.length !== 2 ||
    !value.clears_streams.includes("stdout") ||
    !value.clears_streams.includes("stderr")
  )
    return;
  return {
    id,
    name,
    revision: value.revision,
    stdout,
    stderr,
    clears_streams: ["stdout", "stderr"],
  };
}

export function supervisorLogClearInput(
  preview: SupervisorLogClearPreview,
): Record<string, unknown> {
  if (
    preview.clears_streams.length !== 2 ||
    !preview.clears_streams.includes("stdout") ||
    !preview.clears_streams.includes("stderr")
  )
    throw new Error("清理预览未同时覆盖标准输出和标准错误。");
  return {
    id: preview.id,
    name: preview.name,
    expected_revision: preview.revision,
    expected_stdout: preview.stdout,
    expected_stderr: preview.stderr,
  };
}
