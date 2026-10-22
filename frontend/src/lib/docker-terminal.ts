import { t as translate } from "./i18n";
import type { Capability } from "./api";

export type DockerTerminalSession = {
  id: string;
  name: string;
  session_id: string;
  target_id: string;
  type: "docker";
  rows: number;
  cols: number;
  created_at: string;
  expires_at: string;
  execution_state: string;
  attached: boolean;
  stream_started: boolean;
  output_available: boolean;
  close_requested?: boolean;
  exit_code?: number;
};

export type DockerTerminalRead = DockerTerminalSession & {
  frames: { sequence: number; data: string }[];
  truncated: boolean;
  next: number;
  output_unavailable?: boolean;
};

const isRecord = (value: unknown): value is Record<string, unknown> =>
  !!value && typeof value === "object" && !Array.isArray(value);
const safeInt = (value: unknown, min: number, max: number): value is number =>
  typeof value === "number" &&
  Number.isSafeInteger(value) &&
  value >= min &&
  value <= max;
const validDate = (value: unknown) =>
  typeof value === "string" && Number.isFinite(Date.parse(value));

export function dockerTerminalCapability(
  capabilities: Capability[],
  name: string,
  scope: "containers:read" | "containers:manage",
  required: string[],
  readOnly: boolean,
  allowedFields: string[] = required,
): Capability | undefined {
  return capabilities.find((capability) => {
    if (
      capability.name !== name ||
      capability.scope !== scope ||
      capability.read_only !== readOnly ||
      capability.task === true
    )
      return false;
    const schema = capability.input_schema;
    const properties = schema?.properties;
    const schemaRequired = schema?.required;
    return (
      isRecord(properties) &&
      allowedFields.every((field) =>
        Object.prototype.hasOwnProperty.call(properties, field),
      ) &&
      Array.isArray(schemaRequired) &&
      required.every((field) => schemaRequired.includes(field)) &&
      schemaRequired.every((field) => allowedFields.includes(String(field)))
    );
  });
}

export function dockerTerminalSession(
  value: unknown,
  instanceID: string,
  logicalName: string,
): DockerTerminalSession | undefined {
  if (!isRecord(value)) return;
  const sessionID = value.session_id;
  if (
    typeof sessionID !== "string" ||
    !/^dct_[a-f0-9]{32}$/.test(sessionID) ||
    value.id !== sessionID ||
    value.name !== logicalName ||
    value.target_id !== `docker:${instanceID}:${logicalName}` ||
    value.type !== "docker" ||
    !safeInt(value.rows, 1, 500) ||
    !safeInt(value.cols, 1, 1000) ||
    !validDate(value.created_at) ||
    !validDate(value.expires_at) ||
    typeof value.execution_state !== "string" ||
    !["created", "starting", "running", "exited", "unknown"].includes(
      value.execution_state,
    ) ||
    typeof value.attached !== "boolean" ||
    typeof value.stream_started !== "boolean" ||
    typeof value.output_available !== "boolean" ||
    (value.close_requested !== undefined &&
      typeof value.close_requested !== "boolean") ||
    (value.exit_code !== undefined && !Number.isSafeInteger(value.exit_code))
  )
    return;
  return {
    id: sessionID,
    name: logicalName,
    session_id: sessionID,
    target_id: `docker:${instanceID}:${logicalName}`,
    type: "docker",
    rows: value.rows,
    cols: value.cols,
    created_at: value.created_at as string,
    expires_at: value.expires_at as string,
    execution_state: value.execution_state,
    attached: value.attached,
    stream_started: value.stream_started,
    output_available: value.output_available,
    ...(value.close_requested === undefined
      ? {}
      : { close_requested: value.close_requested }),
    ...(value.exit_code === undefined
      ? {}
      : { exit_code: value.exit_code as number }),
  };
}

const validBase64 = (value: unknown, maxLength = 5500) =>
  typeof value === "string" &&
  value.length <= maxLength &&
  (value === "" ||
    (/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(
      value,
    ) &&
      value.length % 4 === 0));

export function dockerTerminalRead(
  value: unknown,
  instanceID: string,
  logicalName: string,
  expectedSessionID: string,
  after = 0,
): DockerTerminalRead | undefined {
  if (!isRecord(value)) return;
  const session = dockerTerminalSession(value, instanceID, logicalName);
  const frames = value.frames;
  if (
    !session ||
    session.session_id !== expectedSessionID ||
    !Array.isArray(frames) ||
    frames.length > 32 ||
    !safeInt(value.next, after, Number.MAX_SAFE_INTEGER) ||
    typeof value.truncated !== "boolean" ||
    (value.output_unavailable !== undefined &&
      typeof value.output_unavailable !== "boolean")
  )
    return;
  const parsed: { sequence: number; data: string }[] = [];
  let previous = after;
  for (const frame of frames) {
    if (
      !isRecord(frame) ||
      !safeInt(frame.sequence, 1, Number.MAX_SAFE_INTEGER) ||
      frame.sequence <= previous ||
      !validBase64(frame.data)
    )
      return;
    previous = frame.sequence;
    parsed.push({ sequence: frame.sequence, data: frame.data as string });
  }
  if (parsed.length && value.next < parsed[parsed.length - 1].sequence) return;
  return {
    ...session,
    frames: parsed,
    truncated: value.truncated,
    next: value.next,
    ...(value.output_unavailable === undefined
      ? {}
      : { output_unavailable: value.output_unavailable }),
  };
}

export function dockerTerminalInputFrame(
  sessionID: string,
  inputID: string,
  data: string,
): { session_id: string; input_id: string; data: string } {
  if (
    !/^dct_[a-f0-9]{32}$/.test(sessionID) ||
    !/^[A-Za-z0-9_.:-]{8,128}$/.test(inputID) ||
    !validBase64(data, 21848) ||
    data.length === 0 ||
    data.length > 21848
  )
    throw new Error(translate("终端输入帧无效，请重新连接后重试。"));
  const decoded = atob(data);
  if (decoded.length < 1 || decoded.length > 16 * 1024)
    throw new Error(translate("单个终端输入帧不能超过 16 KiB。"));
  return { session_id: sessionID, input_id: inputID, data };
}

export function dockerTerminalStateLabel(state: string): string {
  const labels: Record<string, string> = {
    created: translate("已创建"),
    starting: translate("正在启动"),
    running: translate("Engine进程仍在运行"),
    exited: translate("Engine进程已退出"),
    unknown: translate("Engine状态未知"),
  };
  return labels[state] ?? translate("状态未提供");
}
