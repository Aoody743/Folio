import { t as translate } from "./i18n";
export type Schema = {
  type?: string | string[];
  title?: string;
  description?: string;
  properties?: Record<string, Schema>;
  required?: string[];
  items?: Schema;
  enum?: unknown[];
  const?: unknown;
  default?: unknown;
  minimum?: number;
  maximum?: number;
  exclusiveMinimum?: number;
  exclusiveMaximum?: number;
  minLength?: number;
  maxLength?: number;
  minItems?: number;
  maxItems?: number;
  minProperties?: number;
  maxProperties?: number;
  pattern?: string;
  format?: string;
  additionalProperties?: boolean | Schema;
  oneOf?: Schema[];
  anyOf?: Schema[];
  writeOnly?: boolean;
};
export function applyLockedPreset(
  value: unknown,
  preset: Record<string, unknown>,
  fields: string[],
): unknown {
  if (!value || typeof value !== "object" || Array.isArray(value)) return value;
  const result = { ...(value as Record<string, unknown>) };
  for (const field of fields)
    if (Object.prototype.hasOwnProperty.call(preset, field))
      result[field] = preset[field];
  return result;
}
export function isDecimalCursorSchema(schema: Schema): boolean {
  const variants = schema.oneOf ?? schema.anyOf;
  return (
    !!variants?.some(
      (option) =>
        option.type === "string" &&
        option.pattern === "^(0|[1-9][0-9]{0,19})$" &&
        option.maxLength === 20,
    ) &&
    variants.some(
      (option) =>
        option.type === "integer" &&
        option.minimum === 0 &&
        option.maximum === Number.MAX_SAFE_INTEGER,
    )
  );
}
export function normalizeDecimalCursor(value: unknown): string | number {
  if (typeof value === "number" && Number.isSafeInteger(value) && value >= 0)
    return value;
  if (
    typeof value !== "string" ||
    !/^(0|[1-9][0-9]{0,19})$/.test(value) ||
    BigInt(value) > 18446744073709551615n
  )
    throw new Error(
      translate("扫描游标需要完整的无符号十进制字符串；不能使用已舍入的数字。"),
    );
  return value;
}
export function initialValue(schema: Schema, preset?: unknown): unknown {
  if (preset !== undefined) return preset;
  if (schema.default !== undefined) return schema.default;
  if (schema.const !== undefined) return schema.const;
  if (schema.type === "object" || schema.properties) {
    const value: Record<string, unknown> = Object.create(null);
    for (const [key, child] of Object.entries(schema.properties ?? {})) {
      if (schema.required?.includes(key) || child.default !== undefined)
        value[key] = initialValue(child);
    }
    return value;
  }
  if (schema.type === "array") return [];
  if (schema.type === "boolean") return false;
  return "";
}
export function projectInput(schema: Schema, value: unknown): unknown {
  if (schema.type === "object" || schema.properties) {
    if (!value || typeof value !== "object" || Array.isArray(value))
      return value;
    const output: Record<string, unknown> = Object.create(null);
    for (const [key, entry] of Object.entries(value)) {
      const child = schema.properties?.[key];
      if (child) output[key] = projectInput(child, entry);
      else if (schema.additionalProperties !== false)
        output[key] =
          typeof schema.additionalProperties === "object"
            ? projectInput(schema.additionalProperties, entry)
            : entry;
    }
    return output;
  }
  if (schema.type === "array" && Array.isArray(value))
    return value.map((entry) => projectInput(schema.items ?? {}, entry));
  return value;
}
export function normalizeInput(
  schema: Schema,
  value: unknown,
  path = translate("参数"),
): unknown {
  if (isDecimalCursorSchema(schema)) return normalizeDecimalCursor(value);
  if (
    value === null &&
    Array.isArray(schema.type) &&
    schema.type.includes("null")
  )
    return null;
  if (schema.oneOf || schema.anyOf) {
    let candidate = value;
    if (typeof value === "string") {
      try {
        candidate = JSON.parse(value);
      } catch {}
    }
    const accepted: unknown[] = [];
    for (const variant of schema.oneOf ?? schema.anyOf ?? []) {
      try {
        accepted.push(normalizeInput(variant, candidate, path));
      } catch {}
    }
    if (!accepted.length || (schema.oneOf && accepted.length !== 1))
      throw new Error(translate("{0}需要符合其联合类型。", [path]));
    return accepted[0];
  }
  if (
    schema.const !== undefined &&
    JSON.stringify(schema.const) !== JSON.stringify(value)
  )
    throw new Error(translate("{0}需要固定值。", [path]));
  if (
    schema.enum &&
    !schema.enum.some((v) => JSON.stringify(v) === JSON.stringify(value))
  )
    throw new Error(translate("{0}需要选择有效选项。", [path]));
  const kind = Array.isArray(schema.type)
    ? schema.type.find((t) => t !== "null")
    : schema.type;
  if (kind === "object" || schema.properties) {
    const obj = typeof value === "string" ? JSON.parse(value) : value;
    if (!obj || typeof obj !== "object" || Array.isArray(obj))
      throw new Error(translate("{0}需要JSON对象。", [path]));
    const count = Object.keys(obj).length;
    if (
      (schema.minProperties !== undefined && count < schema.minProperties) ||
      (schema.maxProperties !== undefined && count > schema.maxProperties)
    )
      throw new Error(translate("{0}字段数量超出允许范围。", [path]));
    const result: Record<string, unknown> = Object.create(null);
    for (const key of schema.required ?? []) {
      if (!Object.prototype.hasOwnProperty.call(obj, key))
        throw new Error(translate("请填写{0}.{1}。", [path, key]));
    }
    for (const [key, entry] of Object.entries(obj)) {
      const child = schema.properties?.[key];
      if (!child) {
        if (schema.additionalProperties === false)
          throw new Error(translate("{0}.{1}不是有效字段。", [path, key]));
        result[key] =
          typeof schema.additionalProperties === "object"
            ? normalizeInput(
                schema.additionalProperties,
                entry,
                `${path}.${key}`,
              )
            : entry;
      } else
        result[key] = normalizeInput(
          child,
          entry,
          `${path}.${child.title ?? key}`,
        );
    }
    return result;
  }
  if (kind === "array") {
    const entries = typeof value === "string" ? JSON.parse(value) : value;
    if (!Array.isArray(entries))
      throw new Error(translate("{0}需要数组。", [path]));
    if (
      (schema.minItems !== undefined && entries.length < schema.minItems) ||
      (schema.maxItems !== undefined && entries.length > schema.maxItems)
    )
      throw new Error(translate("{0}项数超出允许范围。", [path]));
    return entries.map((v, i) =>
      normalizeInput(schema.items ?? {}, v, `${path}[${i + 1}]`),
    );
  }
  if (kind === "number" || kind === "integer") {
    if (
      value === "" ||
      value === null ||
      !["string", "number"].includes(typeof value)
    )
      throw new Error(translate("{0}需要数字。", [path]));
    const number = Number(value);
    if (
      !Number.isFinite(number) ||
      (kind === "integer" && !Number.isSafeInteger(number))
    )
      throw new Error(
        translate("{0}需要{1}。", [
          path,
          kind === "integer" ? translate("整数") : translate("数字"),
        ]),
      );
    if (
      (schema.minimum !== undefined && number < schema.minimum) ||
      (schema.maximum !== undefined && number > schema.maximum) ||
      (schema.exclusiveMinimum !== undefined &&
        number <= schema.exclusiveMinimum) ||
      (schema.exclusiveMaximum !== undefined &&
        number >= schema.exclusiveMaximum)
    )
      throw new Error(translate("{0}超出允许范围。", [path]));
    return number;
  }
  if (kind === "boolean") {
    if (typeof value !== "boolean")
      throw new Error(translate("{0}需要开关值。", [path]));
    return value;
  }
  if (kind === "string") {
    if (typeof value !== "string")
      throw new Error(translate("{0}需要文本。", [path]));
    const count = Array.from(value).length;
    if (
      (schema.minLength !== undefined && count < schema.minLength) ||
      (schema.maxLength !== undefined && count > schema.maxLength)
    )
      throw new Error(translate("{0}长度超出允许范围。", [path]));
    if (schema.pattern && !new RegExp(schema.pattern).test(value))
      throw new Error(translate("{0}格式不正确。", [path]));
    return value;
  }
  if (kind === "null" && value !== null)
    throw new Error(translate("{0}需要空值。", [path]));
  return value;
}
