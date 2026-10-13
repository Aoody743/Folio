export type SQLCell = {
  text: string;
  kind: "integer" | "binary" | "null" | "value" | "unsafe";
  detail?: string;
};
export function sqlCell(value: unknown): SQLCell {
  if (value === null) return { text: "NULL", kind: "null" };
  if (
    typeof value === "number" &&
    Number.isInteger(value) &&
    !Number.isSafeInteger(value)
  )
    return { text: "整数精度未能确认", kind: "unsafe" };
  if (value && typeof value === "object" && !Array.isArray(value)) {
    const encoded = value as {
      encoding?: unknown;
      value?: unknown;
      data?: unknown;
    };
    if (
      encoded.encoding === "integer" &&
      typeof encoded.value === "string" &&
      /^-?(0|[1-9][0-9]{0,19})$/.test(encoded.value)
    ) {
      const number = BigInt(encoded.value);
      if (number >= -9223372036854775808n && number <= 18446744073709551615n)
        return { text: encoded.value, kind: "integer", detail: "精确整数" };
      return { text: "整数超出协议范围", kind: "unsafe" };
    }
    if (encoded.encoding === "integer")
      return { text: "整数格式未能确认", kind: "unsafe" };
    if (
      encoded.encoding === "base64" &&
      typeof encoded.data === "string" &&
      /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(
        encoded.data,
      )
    ) {
      const size =
        (encoded.data.length / 4) * 3 -
        (encoded.data.endsWith("==") ? 2 : encoded.data.endsWith("=") ? 1 : 0);
      const prefix = atob(encoded.data.slice(0, 88));
      const hex = Array.from(prefix.slice(0, 32), (byte) =>
        byte.charCodeAt(0).toString(16).padStart(2, "0"),
      ).join(" ");
      return {
        text: hex + (size > 32 ? " …" : ""),
        kind: "binary",
        detail: `${size} 字节 · 二进制`,
      };
    }
  }
  return {
    text:
      typeof value === "string" ? value : (JSON.stringify(value) ?? "未提供"),
    kind: "value",
  };
}
export function sqlColumns(columns: string[]): string[] {
  const used = new Set<string>();
  return columns.map((original, index) => {
    const key = used.has(original) ? `${original}_${index}` : original;
    used.add(key);
    return key;
  });
}
export function validateSQLParameters(input: unknown): void {
  const parameters = (input as { parameters?: unknown } | null)?.parameters;
  if (!Array.isArray(parameters)) return;
  const visit = (value: unknown) => {
    if (
      typeof value === "number" &&
      Number.isInteger(value) &&
      !Number.isSafeInteger(value)
    )
      throw new Error(
        "大整数参数请使用十进制字符串，或包含encoding:integer与字符串value的对象；当前数字可能已舍入。",
      );
    if (Array.isArray(value)) value.forEach(visit);
    else if (value && typeof value === "object")
      Object.values(value).forEach(visit);
  };
  parameters.forEach(visit);
}
