import type { Capability } from "./api";
import { phase2Capability } from "./database-binlog";

export type FTPAccountInspection = {
  id: string;
  name: string;
  revision: number;
  last_confirmed_enabled?: boolean;
  recovery_required: boolean;
  phase:
    | "active"
    | "pause_pending"
    | "paused"
    | "resume_pending"
    | "unknown"
    | "conflict"
    | "legacy_recovery_required"
    | "legacy_recovery_pending"
    | "legacy_unrecoverable";
  allowed_recovery_commands: string[];
};

const isRecord = (value: unknown): value is Record<string, unknown> =>
  !!value && typeof value === "object" && !Array.isArray(value);

export function ftpAccountStateCapability(
  capabilities: Capability[],
  name: "ftp_account_state_inspect" | "ftp_account_state_legacy_recover",
): Capability | undefined {
  const task = name.endsWith("legacy_recover");
  const scope = task ? "ftp:manage" : "ftp:read";
  const required = task
    ? ["id", "name", "expected_revision", "confirm_clear_legacy_deny_rule"]
    : ["id", "name"];
  const capability = phase2Capability(
    capabilities,
    name,
    scope,
    required,
    task,
    !task,
  );
  if (!capability) return undefined;
  const properties = capability.input_schema?.properties;
  if (task) {
    const confirmation = isRecord(properties)
      ? properties.confirm_clear_legacy_deny_rule
      : undefined;
    if (!isRecord(confirmation) || confirmation.const !== true)
      return undefined;
  }
  return capability;
}

export function parseFTPAccountInspection(
  value: unknown,
  id: string,
  name: string,
): FTPAccountInspection | undefined {
  if (!isRecord(value)) return;
  const allowed = [
    "pause",
    "resume",
    "apply_pending",
    "restore_previous",
    "legacy_recover",
  ];
  const rawCommands = value.allowed_recovery_commands;
  const commandValues =
    rawCommands === null
      ? []
      : Array.isArray(rawCommands)
        ? rawCommands
        : undefined;
  const phases = [
    "active",
    "pause_pending",
    "paused",
    "resume_pending",
    "unknown",
    "conflict",
    "legacy_recovery_required",
    "legacy_recovery_pending",
    "legacy_unrecoverable",
  ];
  if (
    value.id !== id ||
    value.name !== name ||
    typeof value.revision !== "number" ||
    !Number.isSafeInteger(value.revision) ||
    value.revision < 1 ||
    typeof value.recovery_required !== "boolean" ||
    typeof value.phase !== "string" ||
    !phases.includes(value.phase) ||
    !commandValues ||
    commandValues.some(
      (command) => typeof command !== "string" || !allowed.includes(command),
    ) ||
    (value.last_confirmed_enabled !== undefined &&
      typeof value.last_confirmed_enabled !== "boolean")
  )
    return;
  const commands = [...new Set(commandValues as string[])];
  if (
    value.phase === "legacy_recovery_required" &&
    (value.last_confirmed_enabled !== undefined ||
      value.recovery_required !== true ||
      !commands.includes("legacy_recover"))
  )
    return;
  if (
    value.phase === "legacy_recovery_pending" &&
    (value.last_confirmed_enabled !== undefined ||
      value.recovery_required !== true ||
      !commands.includes("legacy_recover"))
  )
    return;
  if (
    value.phase !== "legacy_recovery_required" &&
    value.phase !== "legacy_recovery_pending" &&
    commands.includes("legacy_recover")
  )
    return;
  return {
    id,
    name,
    revision: value.revision,
    ...(typeof value.last_confirmed_enabled === "boolean"
      ? { last_confirmed_enabled: value.last_confirmed_enabled }
      : {}),
    recovery_required: value.recovery_required,
    phase: value.phase as FTPAccountInspection["phase"],
    allowed_recovery_commands: commands,
  };
}

export function ftpLegacyRecoveryInput(
  inspection: FTPAccountInspection | undefined,
  id: string,
  name: string,
):
  | {
      id: string;
      name: string;
      expected_revision: number;
      confirm_clear_legacy_deny_rule: true;
    }
  | undefined {
  if (
    !inspection ||
    inspection.id !== id ||
    inspection.name !== name ||
    !["legacy_recovery_required", "legacy_recovery_pending"].includes(
      inspection.phase,
    ) ||
    !inspection.recovery_required ||
    !inspection.allowed_recovery_commands.includes("legacy_recover") ||
    !Number.isSafeInteger(inspection.revision) ||
    inspection.revision < 1
  )
    return;
  return {
    id,
    name,
    expected_revision: inspection.revision,
    confirm_clear_legacy_deny_rule: true,
  };
}
