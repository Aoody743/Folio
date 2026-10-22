import { t as translate } from "./i18n";
export type DatabaseAdminState = {
  current_credential_version: string;
  state: "legacy_unversioned" | "active" | "recovery_required";
  available_actions: string[];
};
export function databaseAdminState(
  value: unknown,
): DatabaseAdminState | undefined {
  if (!value || typeof value !== "object" || Array.isArray(value)) return;
  const v = value as Record<string, unknown>;
  if (
    typeof v.current_credential_version !== "string" ||
    !["legacy_unversioned", "active", "recovery_required"].includes(
      String(v.state),
    ) ||
    !Array.isArray(v.available_actions) ||
    !v.available_actions.every((a) => typeof a === "string")
  )
    return;
  return {
    current_credential_version: v.current_credential_version,
    state: v.state as DatabaseAdminState["state"],
    available_actions: v.available_actions.filter((a) =>
      ["initialize", "rotate", "recover", "apply_pending"].includes(a),
    ),
  };
}
export function databaseAdminTaskInput(
  instanceID: string,
  currentVersion: string,
  passwordVersion?: string,
) {
  if (passwordVersion !== undefined && !/^[a-f0-9]{32}$/.test(passwordVersion))
    throw new Error(translate("密码准备响应无效，请重新读取状态。"));
  return {
    instance_id: instanceID,
    current_credential_version: currentVersion,
    ...(passwordVersion ? { password_version: passwordVersion } : {}),
  };
}
