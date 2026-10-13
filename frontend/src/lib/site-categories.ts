import type { APIError, Capability } from "./api";
import { normalizeInput, type Schema } from "./schema";

export type SiteCategory = {
  id: string;
  name: string;
  revision: number;
};

export type SiteCategoryReference = {
  value: string;
  label: string;
};

export function siteCategoryCapabilities(capabilities: Capability[]) {
  return {
    list: capabilities.find((item) => item.name === "sites_categories_list"),
    save: capabilities.find((item) => item.name === "sites_category_save"),
    delete: capabilities.find((item) => item.name === "sites_category_delete"),
  };
}

export function supportsSiteCategoryFilter(
  capability: Capability | undefined,
): boolean {
  const properties = capability?.input_schema?.properties;
  return !!properties && typeof properties === "object" && "category_id" in properties;
}

export function siteListInput(
  capability: Capability | undefined,
  input: { limit: number; offset: number; query: string; categoryID: string },
): Record<string, unknown> {
  const properties = capability?.input_schema?.properties;
  if (!properties || typeof properties !== "object") return {};
  const candidate: Record<string, unknown> = {
    limit: input.limit,
    offset: input.offset,
    ...(input.query ? { query: input.query } : {}),
    // An empty category_id means “all categories” on the server.
    ...(input.categoryID ? { category_id: input.categoryID } : {}),
  };
  return Object.fromEntries(
    Object.entries(candidate).filter(([key]) => key in properties),
  );
}

export function siteCategoryReferences(
  categories: SiteCategory[],
): Record<string, SiteCategoryReference[]> {
  return {
    category_id: [
      { value: "", label: "未分类" },
      ...categories.map((category) => ({
        value: category.id,
        label: `${category.name} · ${category.id}`,
      })),
    ],
  };
}

export function newSiteCategoryID(uuid: string): string {
  const normalized = uuid.toLowerCase();
  if (!/^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(normalized))
    throw new Error("无法生成有效的网站分类标识。");
  return `cat-${normalized}`;
}

export function siteCategoryMutationInput(
  capability: Capability | undefined,
  input: Record<string, unknown>,
): Record<string, unknown> {
  if (
    !capability ||
    !["sites_category_save", "sites_category_delete"].includes(capability.name) ||
    !capability.input_schema
  )
    throw new Error("当前账户没有可用的网站分类写入能力。");
  return normalizeInput(capability.input_schema as Schema, input) as Record<
    string,
    unknown
  >;
}

export function siteCategoryErrorMessage(
  error: Pick<APIError, "code" | "message" | "status">,
): string {
  if (error.code === "category_in_use")
    return "该分类仍被网站使用。请先调整这些网站的分类，再删除；服务器信息：" + error.message;
  if (error.code === "revision_conflict" || error.status === 412)
    return "分类版本已变化，草稿已保留并重新读取。核对最新版本后再保存；服务器信息：" + error.message;
  if (error.status === 403)
    return "当前账户无权完成此分类操作，草稿已保留并重新读取分类列表。";
  if (error.status === 409)
    return "分类操作与当前状态冲突，草稿已保留并重新读取；服务器信息：" + error.message;
  return error.message;
}

export function shouldReloadSiteCategoriesAfterError(
  error: Pick<APIError, "status">,
): boolean {
  return [403, 409, 412].includes(error.status);
}

export function isSiteCategoryRevisionConflict(
  error: Pick<APIError, "code" | "status">,
): boolean {
  return error.status === 412 || error.code === "revision_conflict";
}
