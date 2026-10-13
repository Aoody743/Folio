import type { Capability } from "./api";

export type SystemChangeAction =
  | "system_change_apply"
  | "system_change_confirm"
  | "system_change_rollback";

export type SystemChangePlan = {
  id: string;
  state: string;
  change: Record<string, unknown>;
  created_at?: string;
  expires_at?: string;
  confirmation_deadline?: string;
  error?: string;
};

export type SystemChangeRecovery = {
  planId: string;
  taskId?: string;
  pendingAction?: SystemChangeAction;
  intentKey?: string;
};

export type KeyValueStorage = Pick<
  globalThis.Storage,
  "getItem" | "setItem" | "removeItem"
>;

const actionNames: readonly SystemChangeAction[] = [
  "system_change_apply",
  "system_change_confirm",
  "system_change_rollback",
];

export function systemChangeRecoveryKey(origin: string, identity: string) {
  return `powerlinux:system-change:v1:${encodeURIComponent(origin)}:${encodeURIComponent(identity)}`;
}

export function readSystemChangeRecovery(
  storage: KeyValueStorage,
  key: string,
): SystemChangeRecovery | undefined {
  try {
    const raw = storage.getItem(key);
    if (!raw) return undefined;
    const value = JSON.parse(raw) as Record<string, unknown>;
    if (
      typeof value.planId !== "string" ||
      value.planId.length === 0 ||
      value.planId.length > 96
    )
      return undefined;
    const recovery: SystemChangeRecovery = { planId: value.planId };
    if (
      typeof value.taskId === "string" &&
      value.taskId.length > 0 &&
      value.taskId.length <= 160
    )
      recovery.taskId = value.taskId;
    if (
      typeof value.pendingAction === "string" &&
      actionNames.includes(value.pendingAction as SystemChangeAction)
    )
      recovery.pendingAction = value.pendingAction as SystemChangeAction;
    if (
      typeof value.intentKey === "string" &&
      value.intentKey.length > 0 &&
      value.intentKey.length <= 160
    )
      recovery.intentKey = value.intentKey;
    return recovery;
  } catch {
    return undefined;
  }
}

export function writeSystemChangeRecovery(
  storage: KeyValueStorage,
  key: string,
  recovery: SystemChangeRecovery | undefined,
) {
  if (!recovery) {
    storage.removeItem(key);
    return;
  }
  const stored: SystemChangeRecovery = { planId: recovery.planId };
  if (recovery.taskId) stored.taskId = recovery.taskId;
  if (recovery.pendingAction) stored.pendingAction = recovery.pendingAction;
  if (recovery.intentKey) stored.intentKey = recovery.intentKey;
  storage.setItem(key, JSON.stringify(stored));
}

export function canUseSystemChangeStorage(storage: KeyValueStorage, key: string) {
  const probe = `${key}:probe`;
  try {
    storage.setItem(probe, "ready");
    storage.removeItem(probe);
    return true;
  } catch {
    try {
      storage.removeItem(probe);
    } catch {
      // Storage remains unavailable; do not make a mutation without recovery.
    }
    return false;
  }
}

export function hasSystemChangePlanSchema(capability?: Capability) {
  if (capability?.name !== "system_change_plan") return false;
  const input = capability.input_schema;
  const properties = input?.properties as Record<string, unknown> | undefined;
  const required = input?.required;
  const change = properties?.change as Record<string, unknown> | undefined;
  return (
    Array.isArray(required) &&
    required.includes("change") &&
    change?.type === "object" &&
    !!change.properties &&
    typeof change.properties === "object"
  );
}

export function hasSystemChangeIDSchema(
  capability: Capability | undefined,
  expectedName: string,
) {
  if (
    !capability ||
    capability.name !== expectedName ||
    !actionNames.includes(expectedName as SystemChangeAction)
  )
    return false;
  const properties = capability.input_schema?.properties as
    | Record<string, unknown>
    | undefined;
  const id = properties?.id as Record<string, unknown> | undefined;
  const required = capability.input_schema?.required;
  return (
    id?.type === "string" &&
    Array.isArray(required) &&
    required.includes("id")
  );
}

export function hasSystemChangeGetSchema(capability?: Capability) {
  return hasRequiredIDSchema(capability, "system_change_get");
}

export function hasRequiredIDSchema(
  capability: Capability | undefined,
  expectedName: string,
) {
  if (capability?.name !== expectedName) return false;
  const properties = capability.input_schema?.properties as
    | Record<string, unknown>
    | undefined;
  const id = properties?.id as Record<string, unknown> | undefined;
  const required = capability.input_schema?.required;
  return (
    id?.type === "string" &&
    Array.isArray(required) &&
    required.includes("id")
  );
}

export function systemChangeIDInput(
  capability: Capability | undefined,
  expectedName: string,
  id: string,
) {
  const schemaReady =
    expectedName === "system_change_get"
      ? hasSystemChangeGetSchema(capability)
      : hasSystemChangeIDSchema(capability, expectedName);
  return schemaReady && id
    ? { id }
    : undefined;
}

export function parseSystemChangePlan(
  value: unknown,
): SystemChangePlan | undefined {
  if (!value || typeof value !== "object" || Array.isArray(value))
    return undefined;
  const candidate = value as Record<string, unknown>;
  if (
    typeof candidate.id !== "string" ||
    candidate.id.length === 0 ||
    typeof candidate.state !== "string" ||
    !candidate.change ||
    typeof candidate.change !== "object" ||
    Array.isArray(candidate.change)
  )
    return undefined;
  return {
    id: candidate.id,
    state: candidate.state,
    change: candidate.change as Record<string, unknown>,
    ...(typeof candidate.created_at === "string"
      ? { created_at: candidate.created_at }
      : {}),
    ...(typeof candidate.expires_at === "string"
      ? { expires_at: candidate.expires_at }
      : {}),
    ...(typeof candidate.confirmation_deadline === "string"
      ? { confirmation_deadline: candidate.confirmation_deadline }
      : {}),
    ...(typeof candidate.error === "string" ? { error: candidate.error } : {}),
  };
}

export function systemChangeTaskID(value: unknown): string | undefined {
  if (!value || typeof value !== "object" || Array.isArray(value))
    return undefined;
  const result = value as Record<string, unknown>;
  if (typeof result.task_id === "string" && result.task_id)
    return result.task_id;
  const task = result.task;
  if (!task || typeof task !== "object" || Array.isArray(task))
    return undefined;
  const id = (task as Record<string, unknown>).id;
  return typeof id === "string" && id ? id : undefined;
}

export function systemChangeActionAllowed(
  plan: SystemChangePlan | undefined,
  action: SystemChangeAction,
  options: { fresh: boolean; blocked: boolean; now?: number },
) {
  if (!plan || !options.fresh || options.blocked) return false;
  switch (action) {
    case "system_change_apply": {
      if (plan.state !== "planned" || !plan.expires_at) return false;
      const expires = Date.parse(plan.expires_at);
      return Number.isFinite(expires) && expires > (options.now ?? Date.now());
    }
    case "system_change_confirm": {
      if (plan.state !== "awaiting_confirmation" || !plan.confirmation_deadline)
        return false;
      const deadline = Date.parse(plan.confirmation_deadline);
      return Number.isFinite(deadline) && deadline > (options.now ?? Date.now());
    }
    case "system_change_rollback":
      return [
        "applied",
        "awaiting_confirmation",
        "applying",
        "recovery_required",
      ].includes(plan.state);
  }
}

export function systemChangeTaskFinished(state?: string) {
  return [
    "succeeded",
    "failed",
    "canceled",
    "cancelled",
    "interrupted",
    "completed",
  ].includes(state ?? "");
}

export function systemChangeIntentResolved(
  plan: SystemChangePlan,
  action: SystemChangeAction,
  now = Date.now(),
) {
  switch (action) {
    case "system_change_apply":
      return (
        plan.state !== "planned" ||
        (!!plan.expires_at &&
          Number.isFinite(Date.parse(plan.expires_at)) &&
          Date.parse(plan.expires_at) <= now)
      );
    case "system_change_confirm":
      return plan.state === "applied" || plan.state === "rolled_back";
    case "system_change_rollback":
      return plan.state === "rolled_back";
  }
}
