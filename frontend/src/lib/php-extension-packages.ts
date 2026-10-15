import type { Capability } from "./api";

export const phpExtensionModuleIDs = [
  "bcmath",
  "curl",
  "gd",
  "intl",
  "mbstring",
  "mysqli",
  "pdo_mysql",
  "pdo_pgsql",
  "pgsql",
  "redis",
  "soap",
  "sqlite3",
  "xml",
  "zip",
] as const;

export type PHPExtensionOperation = "install" | "uninstall";
export type PHPExtensionPackagePin = {
  name: string;
  version: string;
  architecture: string;
  repository: string;
  sha256: string;
};
export type PHPExtensionPlan = {
  plan_id: string;
  id: string;
  operation: PHPExtensionOperation;
  module_id: (typeof phpExtensionModuleIDs)[number];
  php_version: string;
  php_module_api: string;
  platform: { os: string; arch: string; distro: string; release: string };
  package_pins: PHPExtensionPackagePin[];
  affected_profiles: {
    id: string;
    php_version: string;
    module_enabled: boolean;
    package_reference: boolean;
  }[];
  changes: { kind: string; target: string; description: string }[];
  resources: string[];
  expected_revision: number;
  expires_at: string;
  already_installed: boolean;
  already_enabled: boolean;
  package_removable: false;
  package_retained_reason?: string;
};

const object = (value: unknown): value is Record<string, unknown> =>
  !!value && typeof value === "object" && !Array.isArray(value);
const nonempty = (value: unknown, max = 512): value is string =>
  typeof value === "string" && value.length > 0 && value.length <= max;
const revision = (value: unknown): value is number =>
  typeof value === "number" && Number.isSafeInteger(value) && value >= 1;
const digest = (value: unknown): value is string =>
  typeof value === "string" && /^[a-f0-9]{64}$/.test(value);
const operation = (value: unknown): value is PHPExtensionOperation =>
  value === "install" || value === "uninstall";
const moduleID = (
  value: unknown,
): value is (typeof phpExtensionModuleIDs)[number] =>
  typeof value === "string" &&
  (phpExtensionModuleIDs as readonly string[]).includes(value);

function hasSchema(
  capability: Capability | undefined,
  name: string,
  scope: string,
  required: string[],
  task: boolean,
  readOnly: boolean,
): boolean {
  const schema = capability?.input_schema;
  const properties = schema?.properties;
  const schemaRequired = Array.isArray(schema?.required)
    ? (schema.required as unknown[])
    : undefined;
  return !!(
    capability &&
    capability.name === name &&
    capability.scope === scope &&
    capability.task === task &&
    capability.read_only === readOnly &&
    object(properties) &&
    required.every((field) => Object.hasOwn(properties, field)) &&
    schemaRequired &&
    required.every((field) => schemaRequired.includes(field))
  );
}

export function phpExtensionActions(
  capabilities: Capability[],
): {
  plan?: Capability;
  apply?: Capability;
  modules: (typeof phpExtensionModuleIDs)[number][];
} {
  const plan = capabilities.find((capability) =>
    hasSchema(
      capability,
      "php_extension_plan",
      "php:manage",
      ["id", "operation", "module_id", "expected_revision"],
      false,
      true,
    ),
  );
  const apply = capabilities.find((capability) =>
    hasSchema(
      capability,
      "php_extension_apply",
      "php:manage",
      ["id", "plan_id", "expected_revision", "idempotency_key"],
      true,
      false,
    ),
  );
  const properties = plan?.input_schema?.properties;
  const moduleSchema = object(properties) ? properties.module_id : undefined;
  const enumValues = object(moduleSchema) && Array.isArray(moduleSchema.enum)
    ? moduleSchema.enum
    : [];
  const modules = phpExtensionModuleIDs.filter((module) => enumValues.includes(module));
  return { ...(plan ? { plan } : {}), ...(apply ? { apply } : {}), modules };
}

export function parsePHPExtensionPlan(
  value: unknown,
  expected: {
    id: string;
    operation: PHPExtensionOperation;
    module_id: string;
    expected_revision: number;
  },
): PHPExtensionPlan | undefined {
  if (!object(value)) return;
  const pins: PHPExtensionPackagePin[] = [];
  const profiles: PHPExtensionPlan["affected_profiles"] = [];
  const changes: PHPExtensionPlan["changes"] = [];
  const resources: string[] = [];
  if (
    !Array.isArray(value.package_pins) ||
    value.package_pins.length > 32 ||
    !Array.isArray(value.affected_profiles) ||
    value.affected_profiles.length > 256 ||
    !Array.isArray(value.changes) ||
    value.changes.length > 128 ||
    !Array.isArray(value.resources) ||
    value.resources.length > 64
  )
    return;
  for (const item of value.package_pins) {
    if (
      !object(item) ||
      !nonempty(item.name, 200) ||
      !nonempty(item.version, 200) ||
      !nonempty(item.architecture, 80) ||
      !nonempty(item.repository, 512) ||
      !digest(item.sha256)
    )
      return;
    pins.push({
      name: item.name,
      version: item.version,
      architecture: item.architecture,
      repository: item.repository,
      sha256: item.sha256,
    });
  }
  for (const item of value.affected_profiles) {
    if (
      !object(item) ||
      !nonempty(item.id, 128) ||
      !nonempty(item.php_version, 64) ||
      typeof item.module_enabled !== "boolean" ||
      typeof item.package_reference !== "boolean"
    )
      return;
    profiles.push({
      id: item.id,
      php_version: item.php_version,
      module_enabled: item.module_enabled,
      package_reference: item.package_reference,
    });
  }
  for (const item of value.changes) {
    if (
      !object(item) ||
      !nonempty(item.kind, 64) ||
      !nonempty(item.target, 256) ||
      !nonempty(item.description, 1024)
    )
      return;
    changes.push({ kind: item.kind, target: item.target, description: item.description });
  }
  for (const resource of value.resources) {
    if (typeof resource !== "string" || !resource || resource.length > 256 || resources.includes(resource)) return;
    resources.push(resource);
  }
  const parsedOperation = operation(value.operation) ? value.operation : undefined;
  const platform = object(value.platform) &&
    nonempty(value.platform.os, 64) &&
    nonempty(value.platform.arch, 64) &&
    nonempty(value.platform.distro, 64) &&
    nonempty(value.platform.release, 64)
    ? {
        os: value.platform.os,
        arch: value.platform.arch,
        distro: value.platform.distro,
        release: value.platform.release,
      }
    : undefined;
  if (
    !nonempty(value.plan_id, 128) ||
    value.id !== expected.id ||
    parsedOperation !== expected.operation ||
    !moduleID(value.module_id) ||
    value.module_id !== expected.module_id ||
    !nonempty(value.php_version, 64) ||
    !nonempty(value.php_module_api, 64) ||
    !platform ||
    !revision(value.expected_revision) ||
    value.expected_revision !== expected.expected_revision ||
    !nonempty(value.expires_at, 128) ||
    !Number.isFinite(Date.parse(value.expires_at)) ||
    typeof value.already_installed !== "boolean" ||
    typeof value.already_enabled !== "boolean" ||
    value.package_removable !== false ||
    (parsedOperation === "uninstall" &&
      (typeof value.package_retained_reason !== "string" ||
        value.package_retained_reason.trim().length === 0))
  )
    return;
  if (!resources.includes(`software:${expected.id}`) || !resources.includes("host:packages"))
    return;
  return {
    plan_id: value.plan_id,
    id: expected.id,
    operation: parsedOperation,
    module_id: value.module_id,
    php_version: value.php_version,
    php_module_api: value.php_module_api,
    platform,
    package_pins: pins,
    affected_profiles: profiles,
    changes,
    resources,
    expected_revision: value.expected_revision,
    expires_at: value.expires_at,
    already_installed: value.already_installed,
    already_enabled: value.already_enabled,
    package_removable: false,
    ...(typeof value.package_retained_reason === "string"
      ? { package_retained_reason: value.package_retained_reason.slice(0, 1024) }
      : {}),
  };
}

export function phpExtensionApplyInput(
  plan: PHPExtensionPlan | undefined,
  id: string,
  currentRevision: number,
  now = Date.now(),
  idempotencyKey?: string,
): { id: string; plan_id: string; expected_revision: number; idempotency_key: string } | undefined {
  if (
    !plan ||
    plan.id !== id ||
    plan.expected_revision !== currentRevision ||
    Date.parse(plan.expires_at) <= now ||
    !idempotencyKey ||
    !/^[A-Za-z0-9_.:-]{8,128}$/.test(idempotencyKey)
  )
    return;
  return {
    id,
    plan_id: plan.plan_id,
    expected_revision: plan.expected_revision,
    idempotency_key: idempotencyKey,
  };
}
