/** Friendly creation defaults only; the server remains the identity authority. */
export function resourceIdentity(value: string, prefix: string, maxLength = 64): string {
  const text = value.trim();
  if (!text) return "";
  const slug = text.normalize("NFKD").toLowerCase().replace(/[\u0300-\u036f]/g, "")
    .replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "");
  let hash = 2166136261;
  for (const point of Array.from(text)) hash = Math.imul(hash ^ point.codePointAt(0)!, 16777619);
  return `${prefix}-${slug || (hash >>> 0).toString(36)}`.slice(0, maxLength).replace(/-+$/, "");
}
export function websiteIdentity(domain: string): string {
  let host = domain.replace(/^\*\./, "");
  try { host = new URL(`http://${host}`).hostname; } catch { /* Server validates domains. */ }
  return resourceIdentity((domain.startsWith("*.") ? "wildcard-" : "") + host, "site");
}
export function domainLines(text: string): string[] {
  return [...new Set(text.split(/[\s,，]+/u).map(value => value.trim()).filter(Boolean))];
}
