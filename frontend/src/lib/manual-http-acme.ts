export type ManualHTTPChallenge = {
  domain: string;
  url: string;
  path: string;
  body: string;
};

export type ManualHTTPOrder = {
  certificate_id: string;
  state: "pending_http";
  expires_at: string;
  order_ref: string;
  confirmation_action: "certificates_acme_confirm";
  http_challenges: ManualHTTPChallenge[];
};

type Row = Record<string, unknown>;
const row = (value: unknown): value is Row =>
  !!value && typeof value === "object" && !Array.isArray(value);

const certificateID = (value: unknown): value is string =>
  typeof value === "string" && /^[A-Za-z0-9][A-Za-z0-9_.-]{0,95}$/.test(value);

export function manualHTTPOrder(
  value: unknown,
  expectedID?: string,
): ManualHTTPOrder | undefined {
  if (!row(value) || !Array.isArray(value.http_challenges)) return undefined;
  const id = value.certificate_id;
  const expires = value.expires_at;
  if (
    value.state !== "pending_http" ||
    !certificateID(id) ||
    (expectedID !== undefined && expectedID !== id) ||
    typeof expires !== "string" ||
    !Number.isFinite(Date.parse(expires)) ||
    typeof value.order_ref !== "string" ||
    !/^[a-f0-9]{64}$/.test(value.order_ref) ||
    value.confirmation_action !== "certificates_acme_confirm" ||
    value.http_challenges.length < 1 ||
    value.http_challenges.length > 100
  )
    return undefined;

  const seen = new Set<string>();
  const challenges: ManualHTTPChallenge[] = [];
  for (const entry of value.http_challenges) {
    if (!row(entry)) return undefined;
    const { domain, url, path, body } = entry;
    if (
      typeof domain !== "string" ||
      !/^(?=.{1,253}$)(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(domain) ||
      seen.has(domain) ||
      typeof path !== "string" ||
      path.length > 512 ||
      !path.startsWith("/.well-known/acme-challenge/") ||
      path.includes("..") ||
      /[?#\\\x00\r\n]/.test(path) ||
      typeof url !== "string" ||
      url !== `http://${domain}${path}` ||
      typeof body !== "string" ||
      body.length < 1 ||
      body.length > 2048 ||
      /[\x00\r\n]/.test(body)
    )
      return undefined;
    seen.add(domain);
    challenges.push({ domain, url, path, body });
  }

  return {
    certificate_id: id,
    state: "pending_http",
    expires_at: expires,
    order_ref: value.order_ref,
    confirmation_action: "certificates_acme_confirm",
    http_challenges: challenges,
  };
}
