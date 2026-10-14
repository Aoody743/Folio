import type { Capability } from "./api";

export const softwareRecoveryCommands = [
  "continue",
  "stop_preserve",
] as const;

export type SoftwareRecoveryCommand =
  (typeof softwareRecoveryCommands)[number];

export type SoftwareRecoveryInspection = {
  id: string;
  state: string;
  checkpoint?: string;
  revision?: number;
  available_actions: SoftwareRecoveryCommand[];
  reason?: string;
  receipt_present: boolean;
};

const record = (value: unknown): Record<string, unknown> | undefined =>
  value && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : undefined;

const properties = (capability?: Capability) =>
  record(capability?.input_schema?.properties);

const required = (capability?: Capability) =>
  Array.isArray(capability?.input_schema?.required)
    ? (capability.input_schema.required as unknown[])
    : [];

export function isSoftwareInstanceID(value: string) {
  return /^[a-z][a-z0-9_-]{0,127}$/.test(value);
}

export function hasSoftwareRecoveryInspectSchema(
  capability?: Capability,
): boolean {
  if (
    capability?.name !== "software_recovery_inspect" ||
    capability.read_only !== true
  )
    return false;
  const id = record(properties(capability)?.id);
  return id?.type === "string" && required(capability).includes("id");
}

export function hasSoftwareRecoveryActionSchema(
  capability?: Capability,
): boolean {
  if (
    capability?.name !== "software_recovery" ||
    capability.task !== true ||
    capability.read_only === true
  )
    return false;
  const fields = properties(capability);
  const id = record(fields?.id);
  const command = record(fields?.command);
  const revision = record(fields?.expected_journal_revision);
  const commandValues = Array.isArray(command?.enum) ? command.enum : [];
  const needed = ["id", "command", "expected_journal_revision"];
  return (
    id?.type === "string" &&
    command?.type === "string" &&
    softwareRecoveryCommands.every((item) => commandValues.includes(item)) &&
    revision?.type === "integer" &&
    typeof revision.minimum === "number" &&
    revision.minimum >= 1 &&
    needed.every((key) => required(capability).includes(key))
  );
}

export function parseSoftwareRecoveryInspection(
  value: unknown,
  expectedID: string,
): SoftwareRecoveryInspection | undefined {
  const candidate = record(value);
  if (
    !candidate ||
    candidate.id !== expectedID ||
    typeof candidate.state !== "string" ||
    typeof candidate.receipt_present !== "boolean" ||
    !Array.isArray(candidate.available_actions) ||
    candidate.available_actions.some((item) => typeof item !== "string") ||
    (candidate.checkpoint !== undefined &&
      typeof candidate.checkpoint !== "string") ||
    (candidate.reason !== undefined && typeof candidate.reason !== "string") ||
    (candidate.revision !== undefined &&
      (!Number.isSafeInteger(candidate.revision) ||
        (candidate.revision as number) < 1))
  )
    return undefined;

  const available = candidate.available_actions.filter(
    (action): action is SoftwareRecoveryCommand =>
      softwareRecoveryCommands.includes(action as SoftwareRecoveryCommand),
  );
  if (available.length && candidate.revision === undefined) return undefined;

  return {
    id: expectedID,
    state: candidate.state,
    ...(candidate.checkpoint ? { checkpoint: candidate.checkpoint } : {}),
    ...(candidate.revision !== undefined
      ? { revision: candidate.revision as number }
      : {}),
    available_actions: available,
    ...(candidate.reason ? { reason: candidate.reason } : {}),
    receipt_present: candidate.receipt_present,
  };
}

export function softwareRecoveryActionInput(
  capability: Capability | undefined,
  inspection: SoftwareRecoveryInspection | undefined,
  selectedID: string,
  command: SoftwareRecoveryCommand,
  inspectionFresh: boolean,
) {
  if (
    !hasSoftwareRecoveryActionSchema(capability) ||
    !inspectionFresh ||
    !inspection ||
    inspection.id !== selectedID ||
    !inspection.available_actions.includes(command) ||
    !Number.isSafeInteger(inspection.revision) ||
    (inspection.revision ?? 0) < 1
  )
    return undefined;
  return {
    id: inspection.id,
    command,
    expected_journal_revision: inspection.revision!,
  };
}
