import { t as translate } from "./i18n";
import type { Capability } from "./api";
export type DockerImage = {
  image: string;
  id: string;
  size_bytes: number;
  restored_from_archive?: boolean;
};
export type RegistryCredential = {
  id: string;
  name: string;
  registry: string;
  username: string;
  revision: number;
  credential_configured: boolean;
};
export const exactDockerImage = (value: unknown): value is string =>
  typeof value === "string" &&
  /^[a-z0-9][a-z0-9._:/-]*@sha256:[a-f0-9]{64}$/.test(value);
const record = (v: unknown): v is Record<string, unknown> =>
  !!v && typeof v === "object" && !Array.isArray(v);
export function dockerImages(value: unknown): DockerImage[] {
  if (!record(value) || !Array.isArray(value.items)) return [];
  const read = (entries: unknown[], restored: boolean) =>
    entries
      .filter(
        (v): v is Record<string, unknown> =>
          record(v) &&
          exactDockerImage(v.image) &&
          typeof v.id === "string" &&
          typeof v.size_bytes === "number" &&
          Number.isSafeInteger(v.size_bytes) &&
          v.size_bytes >= 0,
      )
      .map((v) => ({
        image: v.image as string,
        id: v.id as string,
        size_bytes: v.size_bytes as number,
        ...(restored || v.restored_from_archive === true
          ? { restored_from_archive: true }
          : {}),
      }));
  const items = read(value.items, false);
  const restored = Array.isArray(value.restored_from_archive)
    ? read(value.restored_from_archive, true)
    : [];
  const seen = new Set<string>();
  return [...items, ...restored].filter((item) => {
    if (seen.has(item.image)) return false;
    seen.add(item.image);
    return true;
  });
}
export function registryCredential(value: unknown): RegistryCredential {
  if (
    !record(value) ||
    typeof value.id !== "string" ||
    !value.id ||
    typeof value.name !== "string" ||
    typeof value.registry !== "string" ||
    typeof value.username !== "string" ||
    !Number.isSafeInteger(value.revision) ||
    Number(value.revision) < 1 ||
    typeof value.credential_configured !== "boolean"
  )
    throw new Error(translate("凭据元数据无效，请重新读取。"));
  return {
    id: value.id,
    name: value.name,
    registry: value.registry,
    username: value.username,
    revision: Number(value.revision),
    credential_configured: value.credential_configured,
  };
}
export function registryCredentials(value: unknown): RegistryCredential[] {
  if (!record(value) || !Array.isArray(value.items)) return [];
  return value.items.map(registryCredential);
}
export function dockerCapability(
  capabilities: Capability[],
  name: string,
  fields: string[],
  task?: boolean,
) {
  return capabilities.find((c) => {
    if (
      c.scope !==
      (name.startsWith("docker_")
        ? /_get$|_list$|_inspect$/.test(name)
          ? "containers:read"
          : "containers:manage"
        : "databases:credentials")
    )
      return false;
    if (c.name !== name || (task !== undefined && (c.task === true) !== task))
      return false;
    const schema = c.input_schema;
    if (!schema || !record(schema.properties)) return false;
    const properties = schema.properties;
    if (fields.includes("password")) {
      const secret = properties.password;
      if (!record(secret) || secret.format !== "password" || c.task === true)
        return false;
    }
    if (fields.includes("image")) {
      const image = properties.image;
      if (
        !record(image) ||
        typeof image.pattern !== "string" ||
        !image.pattern.includes("@sha256:") ||
        !image.pattern.includes("64")
      )
        return false;
    }
    return fields.every((f) => f in properties);
  });
}
export function dockerPullInput(
  id: string,
  image: string,
  credential?: RegistryCredential,
) {
  if (!exactDockerImage(image))
    throw new Error(translate("请输入完整 repo@sha256 摘要镜像。"));
  return {
    id,
    image,
    ...(credential
      ? {
          credential_id: credential.id,
          credential_revision: credential.revision,
        }
      : {}),
  };
}
