import { t as translate } from "./i18n";
type ExpectedBackup = {
  siteID: string;
  backupID: string;
  size: number;
  sha256: string;
};

type RecordValue = Record<string, unknown>;

const isRecord = (value: unknown): value is RecordValue =>
  typeof value === "object" && value !== null && !Array.isArray(value);

export function verifiedSiteBackupDownloadHref(
  value: unknown,
  expected: ExpectedBackup,
  origin: string,
): string {
  const invalid = () =>
    new Error(translate("服务返回的网站备份信息无效，请刷新后重试。"));
  if (!isRecord(value) || !isRecord(value.backup)) throw invalid();
  const backup = value.backup;
  if (
    backup.id !== expected.backupID ||
    backup.site_id !== expected.siteID ||
    backup.includes_tls !== false ||
    backup.size !== expected.size ||
    !Number.isSafeInteger(backup.size) ||
    Number(backup.size) < 1 ||
    backup.sha256 !== expected.sha256 ||
    typeof backup.sha256 !== "string" ||
    !/^[a-f\d]{64}$/i.test(backup.sha256) ||
    typeof value.download_url !== "string"
  ) {
    throw invalid();
  }

  let target: URL;
  let currentOrigin: URL;
  try {
    target = new URL(value.download_url, origin);
    currentOrigin = new URL(origin);
  } catch {
    throw invalid();
  }
  const parameters = [...target.searchParams.entries()];
  if (
    target.origin !== currentOrigin.origin ||
    target.username !== "" ||
    target.password !== "" ||
    target.pathname !== "/api/v1/sites/backups/download" ||
    target.hash !== "" ||
    parameters.length !== 2 ||
    parameters.some(([key]) => key !== "id" && key !== "backup_id") ||
    target.searchParams.getAll("id").length !== 1 ||
    target.searchParams.getAll("backup_id").length !== 1 ||
    target.searchParams.get("id") !== expected.siteID ||
    target.searchParams.get("backup_id") !== expected.backupID
  ) {
    throw invalid();
  }

  return `/api/v1/sites/backups/download?${new URLSearchParams({
    id: expected.siteID,
    backup_id: expected.backupID,
  }).toString()}`;
}
