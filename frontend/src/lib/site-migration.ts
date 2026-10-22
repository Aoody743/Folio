export type SiteMigrationPlan = {
  id: string;
  site_id: string;
  site_name: string;
  expected_revision: number;
  source_web_instance: string;
  target_web_instance: string;
  php_instance: string;
  tls_certificate_id: string;
  tls_certificate_revision: number;
  domains: string[];
  default: boolean;
  tls_enabled: boolean;
  configuration_mode: "managed" | "advanced";
  reset_advanced_config: boolean;
  expires_at: string;
};

export type SiteMigrationGateReason =
  | "invalid_plan"
  | "foreign_site"
  | "site_changed"
  | "expired";

export function isDedicatedSiteMigrationAction(name: string): boolean {
  return [
    "sites_migration_plan",
    "sites_migration_get",
    "sites_migration_apply",
  ].includes(name);
}

const isRecord = (value: unknown): value is Record<string, unknown> =>
  !!value && typeof value === "object" && !Array.isArray(value);
const validResourceID = (value: string) =>
  /^[a-z][a-z0-9_-]{0,95}$/.test(value);

export function parseSiteMigrationPlan(
  value: unknown,
): SiteMigrationPlan | undefined {
  if (!isRecord(value)) return undefined;
  const plan = value;
  const phpInstance = plan.php_instance === undefined ? "" : plan.php_instance;
  const tlsCertificateID =
    plan.tls_certificate_id === undefined ? "" : plan.tls_certificate_id;
  const tlsCertificateRevision =
    plan.tls_certificate_revision === undefined
      ? 0
      : plan.tls_certificate_revision;
  if (
    typeof plan.id !== "string" ||
    !/^m-[a-f0-9]{32}$/.test(plan.id) ||
    typeof plan.site_id !== "string" ||
    !validResourceID(plan.site_id) ||
    typeof plan.site_name !== "string" ||
    !plan.site_name ||
    typeof plan.expected_revision !== "number" ||
    !Number.isSafeInteger(plan.expected_revision) ||
    plan.expected_revision < 1 ||
    typeof plan.source_web_instance !== "string" ||
    !validResourceID(plan.source_web_instance) ||
    typeof plan.target_web_instance !== "string" ||
    !validResourceID(plan.target_web_instance) ||
    plan.source_web_instance === plan.target_web_instance ||
    typeof phpInstance !== "string" ||
    (phpInstance !== "" && !validResourceID(phpInstance)) ||
    typeof tlsCertificateID !== "string" ||
    (tlsCertificateID !== "" && !validResourceID(tlsCertificateID)) ||
    typeof tlsCertificateRevision !== "number" ||
    !Number.isSafeInteger(tlsCertificateRevision) ||
    tlsCertificateRevision < 0 ||
    !Array.isArray(plan.domains) ||
    !plan.domains.every((domain) => typeof domain === "string") ||
    typeof plan.default !== "boolean" ||
    typeof plan.tls_enabled !== "boolean" ||
    (plan.configuration_mode !== "managed" &&
      plan.configuration_mode !== "advanced") ||
    typeof plan.reset_advanced_config !== "boolean" ||
    (plan.tls_enabled && (!tlsCertificateID || tlsCertificateRevision < 1)) ||
    (plan.configuration_mode === "advanced" && !plan.reset_advanced_config) ||
    (plan.configuration_mode === "managed" && plan.reset_advanced_config) ||
    typeof plan.expires_at !== "string" ||
    !Number.isFinite(Date.parse(plan.expires_at))
  )
    return undefined;

  return {
    id: plan.id,
    site_id: plan.site_id,
    site_name: plan.site_name,
    expected_revision: plan.expected_revision,
    source_web_instance: plan.source_web_instance,
    target_web_instance: plan.target_web_instance,
    php_instance: phpInstance,
    tls_certificate_id: tlsCertificateID,
    tls_certificate_revision: tlsCertificateRevision,
    domains: [...plan.domains] as string[],
    default: plan.default,
    tls_enabled: plan.tls_enabled,
    configuration_mode: plan.configuration_mode,
    reset_advanced_config: plan.reset_advanced_config,
    expires_at: plan.expires_at,
  };
}

export function siteMigrationGate(
  plan: SiteMigrationPlan | undefined,
  site: { id: string; revision: number; web_instance?: string },
  now = Date.now(),
): SiteMigrationGateReason | undefined {
  if (!plan) return "invalid_plan";
  if (plan.site_id !== site.id) return "foreign_site";
  if (
    plan.expected_revision !== site.revision ||
    plan.source_web_instance !== site.web_instance
  )
    return "site_changed";
  const expiresAt = Date.parse(plan.expires_at);
  if (!Number.isFinite(expiresAt) || expiresAt <= now) return "expired";
  return undefined;
}

export function sameSiteMigrationPlan(
  reviewed: SiteMigrationPlan,
  current: SiteMigrationPlan,
): boolean {
  const scalarKeys: (keyof SiteMigrationPlan)[] = [
    "id",
    "site_id",
    "site_name",
    "expected_revision",
    "source_web_instance",
    "target_web_instance",
    "php_instance",
    "tls_certificate_id",
    "tls_certificate_revision",
    "default",
    "tls_enabled",
    "configuration_mode",
    "reset_advanced_config",
    "expires_at",
  ];
  return (
    scalarKeys.every((key) => reviewed[key] === current[key]) &&
    reviewed.domains.length === current.domains.length &&
    reviewed.domains.every((domain, index) => domain === current.domains[index])
  );
}
