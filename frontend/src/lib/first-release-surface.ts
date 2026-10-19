type NamedCapability = { name: string };

// Features explicitly deferred by the RC15 launch-readiness scope stay out of
// the first-release panel even when an agent advertises their capabilities.
const deferredCapabilityPatterns = [
  /^hosted_apps_/,
  /^sites_migration_/,
  /^certificates_acme_/,
  /^certificates_dns_/,
  /^backup_ftp_/,
  /^ftp_/,
  /^mongodb_/,
  /^nosql_mongodb\./,
  /^(?:ssh|sshd)_(?:config|server|service)_/,
];

export const deferredCatalogPluginIDs = new Set([
  "acme_pandominassl_apply",
  "backup_ftp",
  "data_query",
  "git",
  "golang",
  "mongodb",
  "nodejs",
  "php-guard",
  "php-yum",
  "phpmyadmin",
  "pgadmin",
  "pureftp",
  "webssh",
]);

export function isFirstReleaseCapability(capability: NamedCapability): boolean {
  return !deferredCapabilityPatterns.some((pattern) =>
    pattern.test(capability.name),
  );
}

export function firstReleaseCapabilities<T extends NamedCapability>(
  capabilities: T[],
): T[] {
  return capabilities.filter(isFirstReleaseCapability);
}

export function isFirstReleaseCatalogPlugin(id: string): boolean {
  return !deferredCatalogPluginIDs.has(id);
}
