type NamedCapability = { name: string };

// Features explicitly deferred by the RC15 launch-readiness scope stay out of
// the first-release panel even when an agent advertises their capabilities.
const deferredCapabilityPatterns = [
  /^sites_migration_/,
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
  "mongodb",
  "php-guard",
  "php-yum",
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
