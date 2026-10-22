"use client";
import { ClientTable } from "./client-table";
import { t as translate } from "@/lib/i18n";
import { useRef, useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { TextInput } from "@astryxdesign/core/TextInput";
import {
  action,
  APIError,
  apiErrorMessage,
  Capability,
  intentKey,
} from "@/lib/api";
import {
  newSiteCategoryID,
  siteCategoryCapabilities,
  siteCategoryErrorMessage,
  isSiteCategoryRevisionConflict,
  siteCategoryMutationInput,
  shouldReloadSiteCategoriesAfterError,
  SiteCategory,
} from "@/lib/site-categories";
import { Icon } from "./icon";
import { ResourceDrawer } from "./resource-ui";

type Draft = {
  id: string;
  name: string;
  expected_revision: number;
  mode: "create" | "edit";
};

export function SiteCategoriesManager({
  capabilities,
  categories,
  loading,
  listError,
  onReload,
  onChanged,
}: {
  capabilities: Capability[];
  categories: SiteCategory[];
  loading: boolean;
  listError?: string;
  onReload: () => Promise<void>;
  onChanged: (categoryID?: string) => void;
}) {
  const available = siteCategoryCapabilities(capabilities);
  const [open, setOpen] = useState(false);
  const [draft, setDraft] = useState<Draft>();
  const [confirmDelete, setConfirmDelete] = useState<SiteCategory>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [stale, setStale] = useState(false);
  const lastIntent = useRef<{ body: string; key: string } | undefined>(
    undefined,
  );

  if (!available.list) return null;

  const beginCreate = () => {
    setError("");
    setStale(false);
    setConfirmDelete(undefined);
    setDraft({
      id: newSiteCategoryID(crypto.randomUUID()),
      name: "",
      expected_revision: 0,
      mode: "create",
    });
  };
  const beginEdit = (category: SiteCategory) => {
    setError("");
    setStale(false);
    setConfirmDelete(undefined);
    setDraft({
      id: category.id,
      name: category.name,
      expected_revision: category.revision,
      mode: "edit",
    });
  };
  const refreshAfterConflict = async (cause: unknown) => {
    if (
      cause instanceof APIError &&
      shouldReloadSiteCategoriesAfterError(cause)
    ) {
      if (isSiteCategoryRevisionConflict(cause)) setStale(true);
      await onReload();
    }
  };
  const save = async () => {
    if (!draft || !available.save || busy) return;
    let input: Record<string, unknown>;
    try {
      input = siteCategoryMutationInput(available.save, {
        id: draft.id,
        name: draft.name,
        expected_revision: draft.expected_revision,
      });
    } catch (cause) {
      setError(apiErrorMessage(cause));
      return;
    }
    const body = JSON.stringify(input);
    if (!lastIntent.current || lastIntent.current.body !== body)
      lastIntent.current = { body, key: intentKey() };
    setBusy(true);
    setError("");
    try {
      await action(available.save.name, input, {
        idempotencyKey: lastIntent.current.key,
      });
      lastIntent.current = undefined;
      setDraft(undefined);
      setStale(false);
      setConfirmDelete(undefined);
      onChanged();
    } catch (cause) {
      setError(
        cause instanceof APIError
          ? siteCategoryErrorMessage(cause)
          : apiErrorMessage(cause),
      );
      await refreshAfterConflict(cause);
    } finally {
      setBusy(false);
    }
  };
  const remove = async () => {
    if (!confirmDelete || !available.delete || busy) return;
    let input: Record<string, unknown>;
    try {
      input = siteCategoryMutationInput(available.delete, {
        id: confirmDelete.id,
        expected_revision: confirmDelete.revision,
      });
    } catch (cause) {
      setError(apiErrorMessage(cause));
      return;
    }
    const body = JSON.stringify(input);
    if (!lastIntent.current || lastIntent.current.body !== body)
      lastIntent.current = { body, key: intentKey() };
    setBusy(true);
    setError("");
    try {
      await action(available.delete.name, input, {
        idempotencyKey: lastIntent.current.key,
      });
      lastIntent.current = undefined;
      setConfirmDelete(undefined);
      onChanged(confirmDelete.id);
    } catch (cause) {
      setError(
        cause instanceof APIError
          ? siteCategoryErrorMessage(cause)
          : apiErrorMessage(cause),
      );
      await refreshAfterConflict(cause);
    } finally {
      setBusy(false);
    }
  };
  const rebaseDraft = () => {
    if (!draft) return;
    const latest = categories.find((item) => item.id === draft.id);
    if (!latest) return;
    setDraft({ ...draft, expected_revision: latest.revision });
    setStale(false);
    setError("");
    lastIntent.current = undefined;
  };
  const replaceConflictingID = () => {
    if (!draft || draft.mode !== "create") return;
    setDraft({ ...draft, id: newSiteCategoryID(crypto.randomUUID()) });
    setError("");
    setStale(false);
    lastIntent.current = undefined;
  };

  return (
    <>
      <Button
        label={translate("管理分类")}
        variant="secondary"
        icon={<Icon name="settings" />}
        onClick={() => setOpen(true)}
      />
      <ResourceDrawer
        open={open}
        title={translate("网站分类")}
        onClose={() => {
          setOpen(false);
          setDraft(undefined);
          setConfirmDelete(undefined);
          setError("");
          setStale(false);
        }}
      >
        {listError && (
          <div className="notice danger" role="alert">
            {listError}
            <Button
              label={translate("重新读取分类")}
              variant="ghost"
              isDisabled={loading}
              onClick={() => void onReload()}
            />
          </div>
        )}
        {error && (
          <div className="notice danger" role="alert">
            {error}
          </div>
        )}
        <p className="muted">
          {translate(
            "网站分类按当前账户可读范围列出。创建和修改使用服务器提供的参数结构及当前版本；删除失败时分类会保留。",
          )}
        </p>
        <div className="button-row">
          {available.save && (
            <Button
              label={translate("新建分类")}
              variant="primary"
              icon={<Icon name="plus" />}
              isDisabled={busy}
              onClick={beginCreate}
            />
          )}
          <Button
            label={translate("刷新分类")}
            variant="ghost"
            isDisabled={loading || busy}
            onClick={() => void onReload()}
          />
        </div>
        {loading && (
          <p className="muted" role="status">
            {translate("正在读取分类…")}
          </p>
        )}
        {categories.length > 0 ? (
          <ClientTable>
            <table className="resource-table">
              <thead>
                <tr>
                  <th>{translate("分类")}</th>
                  <th>{translate("版本")}</th>
                  <th>{translate("操作")}</th>
                </tr>
              </thead>
              <tbody>
                {categories.map((category) => (
                  <tr key={category.id}>
                    <td>
                      <strong>{category.name}</strong>
                      <small>{category.id}</small>
                    </td>
                    <td>{category.revision}</td>
                    <td>
                      <div className="button-row">
                        {available.save && (
                          <Button
                            label={translate("编辑 {0}", [category.name])}
                            variant="ghost"
                            isDisabled={busy}
                            onClick={() => beginEdit(category)}
                          />
                        )}
                        {available.delete && (
                          <Button
                            label={translate("删除 {0}", [category.name])}
                            variant="ghost"
                            isDisabled={busy}
                            onClick={() => {
                              setDraft(undefined);
                              setError("");
                              setStale(false);
                              setConfirmDelete(category);
                            }}
                          />
                        )}
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </ClientTable>
        ) : (
          !loading &&
          !listError && <p className="muted">{translate("暂无可见分类。")}</p>
        )}
        {draft && (
          <section className="stack" aria-label={translate("分类编辑")}>
            <h3>
              {draft.mode === "create"
                ? translate("新建分类")
                : translate("修改分类")}
            </h3>
            <p className="muted">
              {translate("标识：")}
              <code>{draft.id}</code>
              {translate(" · 提交版本：")}
              {draft.expected_revision}
            </p>
            <TextInput
              label={translate("分类名称")}
              value={draft.name}
              onChange={(name) => setDraft({ ...draft, name })}
              isRequired
              autoComplete="off"
            />
            {stale && (
              <div className="notice warning" role="status">
                {translate("当前版本已变化，草稿仍保留。")}
                {draft.mode === "edit" &&
                categories.some((item) => item.id === draft.id) ? (
                  <Button
                    label={translate("载入最新版本并保留名称")}
                    variant="ghost"
                    isDisabled={busy}
                    onClick={rebaseDraft}
                  />
                ) : draft.mode === "create" ? (
                  <Button
                    label={translate("换一个新标识")}
                    variant="ghost"
                    isDisabled={busy}
                    onClick={replaceConflictingID}
                  />
                ) : null}
              </div>
            )}
            <div className="button-row">
              <Button
                label={
                  draft.mode === "create"
                    ? translate("创建分类")
                    : translate("保存分类")
                }
                variant="primary"
                isDisabled={busy || !draft.name.trim()}
                onClick={() => void save()}
              />
              <Button
                label={translate("取消编辑")}
                variant="ghost"
                isDisabled={busy}
                onClick={() => {
                  setDraft(undefined);
                  setStale(false);
                  setError("");
                }}
              />
            </div>
          </section>
        )}
        {confirmDelete && (
          <section
            className="notice warning"
            aria-label={translate("确认删除分类")}
          >
            <strong>
              {translate("确认删除“")}
              {confirmDelete.name}”？
            </strong>
            <p>
              {translate("删除会以版本")}
              {confirmDelete.revision}
              {translate(
                "提交。若网站仍在使用此分类，服务器会拒绝删除；失败时分类不会从列表消失。",
              )}
            </p>
            {stale && (
              <div className="notice warning" role="status">
                {translate("分类版本已变化，尚未再次删除。")}
                {categories.find((item) => item.id === confirmDelete.id) ? (
                  <Button
                    label={translate("核对最新版本并重新确认")}
                    variant="ghost"
                    isDisabled={busy}
                    onClick={() => {
                      const latest = categories.find(
                        (item) => item.id === confirmDelete.id,
                      );
                      if (latest) setConfirmDelete(latest);
                      setStale(false);
                    }}
                  />
                ) : (
                  <span>
                    {translate(
                      "目标分类当前不可见；请重新读取分类列表后再操作。",
                    )}
                  </span>
                )}
              </div>
            )}
            <div className="button-row">
              <Button
                label={translate("确认删除分类")}
                variant="primary"
                isDisabled={busy || stale}
                onClick={() => void remove()}
              />
              <Button
                label={translate("取消删除")}
                variant="ghost"
                isDisabled={busy}
                onClick={() => setConfirmDelete(undefined)}
              />
            </div>
          </section>
        )}
      </ResourceDrawer>
    </>
  );
}
