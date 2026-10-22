import { t as translate } from "./i18n";
export type ManagedDatabaseReceipt = {
  instance_id: string;
  database: string;
  username: string;
  engine: string;
  permission: string;
  state: string;
  revision: number;
};

type RecordValue = Record<string, unknown>;

const isRecord = (value: unknown): value is RecordValue =>
  typeof value === "object" && value !== null && !Array.isArray(value);

export const isActiveManagedDatabaseState = (
  state: unknown,
): state is "active" => state === "active";

export const isDeletedManagedDatabaseState = (
  state: unknown,
): state is "deleted" => state === "deleted";

export function canRecreateDeletedManagedDatabase(
  receipt: Pick<ManagedDatabaseReceipt, "engine" | "state"> | undefined,
): boolean {
  return receipt?.state === "deleted" && receipt.engine !== "mongodb";
}

export function parseManagedDatabaseReceipt(
  value: unknown,
  instanceID: string,
  database: string,
): ManagedDatabaseReceipt {
  if (
    !isRecord(value) ||
    value.instance_id !== instanceID ||
    value.database !== database ||
    typeof value.username !== "string" ||
    typeof value.engine !== "string" ||
    typeof value.permission !== "string" ||
    typeof value.state !== "string" ||
    !Number.isSafeInteger(value.revision) ||
    Number(value.revision) < 1
  ) {
    throw new Error(translate("服务返回的数据库管理记录无效，请重新查询。"));
  }

  return {
    instance_id: instanceID,
    database,
    username: value.username,
    engine: value.engine,
    permission: value.permission,
    state: value.state,
    revision: Number(value.revision),
  };
}

export function isMissingManagedDatabaseReceipt(error: unknown): boolean {
  return (
    isRecord(error) &&
    error.status === 404 &&
    error.code === "database_receipt_not_found"
  );
}

export function managedDatabaseCreatePreset(
  instanceID: string,
  database: string,
  receipt?: ManagedDatabaseReceipt,
): Record<string, unknown> {
  if (!receipt)
    return { instance_id: instanceID, database, expected_revision: 0 };
  if (receipt.engine === "mongodb" && receipt.state === "deleted")
    throw new Error(translate("MongoDB 暂不支持重建已删除数据库。"));
  if (
    receipt.instance_id !== instanceID ||
    receipt.database !== database ||
    !canRecreateDeletedManagedDatabase(receipt) ||
    !Number.isSafeInteger(receipt.revision) ||
    receipt.revision < 1
  ) {
    throw new Error(translate("只能按该数据库当前的已删除记录版本重建。"));
  }
  return {
    instance_id: instanceID,
    database,
    username: receipt.username,
    permission: receipt.permission,
    expected_revision: receipt.revision,
  };
}

export function managedDatabaseCreateErrorMessage(
  code: string,
  status: number,
  message: string,
): string | undefined {
  if (status === 409 && code === "ownership_conflict")
    return translate(
      "发现同名数据库或账户不属于当前面板，服务已拒绝创建（{0}）：{1}",
      [code, message],
    );
  return undefined;
}
