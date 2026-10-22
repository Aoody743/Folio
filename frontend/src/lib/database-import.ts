export type DatabaseImport = {
  id: string;
  instance_id: string;
  database: string;
  engine: string;
  state: string;
  bytes: number;
  bytes_received: number;
  sha256?: string;
  database_revision: number;
  revision: number;
  created_at: string;
};

export function verifiedDatabaseImport(
  value: unknown,
  target: { instance: string; database: string; revision: number },
): DatabaseImport | undefined {
  if (!value || typeof value !== "object") return;
  const row = value as Partial<DatabaseImport>;
  if (
    typeof row.id !== "string" ||
    !/^dbimport_[a-f0-9]{32}$/.test(row.id) ||
    row.instance_id !== target.instance ||
    row.database !== target.database ||
    row.database_revision !== target.revision ||
    !Number.isSafeInteger(row.revision) ||
    (row.revision ?? 0) < 1 ||
    !Number.isSafeInteger(row.bytes) ||
    (row.bytes ?? 0) < 1 ||
    (row.bytes ?? 0) > 4 * 1024 ** 3 ||
    !Number.isSafeInteger(row.bytes_received) ||
    row.bytes_received !== row.bytes ||
    typeof row.sha256 !== "string" ||
    !/^[a-f0-9]{64}$/.test(row.sha256) ||
    row.state !== "uploaded" ||
    typeof row.engine !== "string" ||
    typeof row.created_at !== "string"
  )
    return;
  return row as DatabaseImport;
}
