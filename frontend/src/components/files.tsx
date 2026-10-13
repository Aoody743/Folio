"use client";
import { useEffect, useRef, useState } from "react";
import dynamic from "next/dynamic";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { TextInput } from "@astryxdesign/core/TextInput";
import {
  action,
  APIError,
  bytes,
  Capability,
  fullDate,
  intentKey,
  list,
  streamRequest,
} from "@/lib/api";
import {
  FileContent,
  FileEntry,
  FilePage,
  FileRoot,
  Page,
} from "@/lib/resources";
import { useResource } from "@/lib/resource-hook";
import { uploadFile, Upload, waitTask } from "@/lib/upload";
import { ActionBrowser, ActionForm } from "./actions";
import { Icon } from "./icon";
import {
  Pager,
  ResourceActions,
  ResourceDrawer,
  Unavailable,
} from "./resource-ui";
const CodeEditor = dynamic(() => import("./code-editor"), { ssr: false });
const join = (path: string, name: string) =>
  path && path !== "." ? `${path}/${name}` : name;
const downloadURL = (root: string, path: string) =>
  `/api/v1/files/download?${new URLSearchParams({ root, path })}`;
const permissions = (mode?: number | string) =>
  typeof mode === "number" ? mode.toString(8).padStart(3, "0") : (mode ?? "—");
const fileDraftGuard = "文件草稿已保留，请先保存或放弃草稿再切换文件。";
const directoryDraftGuard = "文件草稿已保留，请先保存或放弃草稿再切换目录。";
type Trash = {
  id: string;
  root: string;
  path: string;
  deleted_at: string;
  directory: boolean;
  state: string;
};
type History = {
  id: string;
  path: string;
  sha256: string;
  actor_id: string;
  created_at: string;
  encoding: string;
  size: number;
};
export function FilesView({
  capabilities,
  onExecuted,
  active = true,
  rootRequest,
}: {
  capabilities: Capability[];
  onExecuted: () => void;
  active?: boolean;
  rootRequest?: { root: string; sequence: number };
}) {
  const [root, setRoot] = useState("");
  const [path, setPath] = useState("");
  const [offset, setOffset] = useState(0);
  const [query, setQuery] = useState("");
  const [search, setSearch] = useState("");
  const [selected, setSelected] = useState<FileEntry>();
  const [editorOpen, setEditorOpen] = useState(false);
  const [dirty, setDirty] = useState(false);
  const [error, setError] = useState("");
  const [operation, setOperation] = useState<string>();
  const [createOpen, setCreateOpen] = useState(false);
  const [createName, setCreateName] = useState("fs_create");
  const [uploadOpen, setUploadOpen] = useState(false);
  const [trashOpen, setTrashOpen] = useState(false);
  const [other, setOther] = useState<string>();
  const appliedRootRequest = useRef<number>(undefined);
  const has = (name: string) => capabilities.some((c) => c.name === name);
  const roots = useResource<FileRoot[] | Page<FileRoot>>(
    "fs_roots",
    {},
    active && has("fs_roots"),
  );
  const availableRoots = list(roots.data);
  useEffect(() => {
    if (!dirty)
      setError((previous) =>
        previous === fileDraftGuard || previous === directoryDraftGuard
          ? ""
          : previous,
      );
  }, [dirty]);
  useEffect(() => {
    if (
      !rootRequest ||
      appliedRootRequest.current === rootRequest.sequence ||
      !roots.data
    )
      return;
    if (dirty) {
      setError(directoryDraftGuard);
      setEditorOpen(true);
      return;
    }
    if (!availableRoots.some((item) => item.id === rootRequest.root)) {
      setError("该资源目录当前不可访问，请检查目录是否仍存在及访问权限。");
      return;
    }
    appliedRootRequest.current = rootRequest.sequence;
    setRoot(rootRequest.root);
    setPath("");
    setOffset(0);
    setSelected(undefined);
    setEditorOpen(false);
    setError("");
  }, [rootRequest, roots.data, dirty, availableRoots]);
  useEffect(() => {
    if (!root && availableRoots.length) setRoot(availableRoots[0].id);
  }, [root, availableRoots]);
  const listing = useResource<FilePage>(
    "fs_list",
    {
      root,
      path,
      limit: 50,
      offset,
      sort: "name",
      order: "asc",
      ...(search ? { query: search } : {}),
    },
    active && !!root && has("fs_list"),
  );
  const trash = useResource<Trash[] | Page<Trash>>(
    "fs_trash_list",
    { root },
    active && trashOpen && !!root && has("fs_trash_list"),
  );
  const currentRoot = availableRoots.find((r) => r.id === root);
  const rows = list(listing.data);
  const refresh = () => {
    void listing.reload();
    void trash.reload();
    onExecuted();
  };
  const guard = (next: () => void) => {
    if (dirty) {
      setError(fileDraftGuard);
      setEditorOpen(true);
      return;
    }
    next();
  };
  const enter = (entry: FileEntry) =>
    guard(() => {
      if (entry.type === "directory") {
        setPath(entry.path);
        setOffset(0);
      } else if (entry.type === "file") {
        setSelected(entry);
        setEditorOpen(true);
        setError("");
      } else setError("符号链接与特殊文件不能直接编辑，请使用受控文件操作。");
    });
  const trashAction = async (name: string, item: Trash) => {
    try {
      const submitted = await action<{ id: string }>(
        name,
        { root, id: item.id },
        { idempotencyKey: intentKey() },
      );
      await waitTask(submitted.id);
      refresh();
    } catch (e) {
      setError((e as Error).message);
    }
  };
  const create = capabilities.find((c) => c.name === createName);
  return (
    <div className="stack">
      <Card className="panel-card">
        <div className="card-heading">
          <h2>文件管理</h2>
          <div className="button-row">
            {has("fs_create") && !currentRoot?.read_only && (
              <Button
                label="新建文件"
                icon={<Icon name="plus" />}
                onClick={() => {
                  setCreateName("fs_create");
                  setCreateOpen(true);
                }}
              />
            )}
            {has("fs_mkdir") && !currentRoot?.read_only && (
              <Button
                label="新建目录"
                onClick={() => {
                  setCreateName("fs_mkdir");
                  setCreateOpen(true);
                }}
              />
            )}
            {has("fs_upload_begin") && !currentRoot?.read_only && (
              <Button
                label="上传文件"
                variant="primary"
                onClick={() => setUploadOpen(true)}
              />
            )}
          </div>
        </div>
        {!has("fs_roots") ? (
          <Unavailable title="文件资源服务暂不可用" icon="files" />
        ) : (
          <>
            <div className="file-toolbar">
              <label className="native-field">
                资源目录
                <select
                  aria-label="文件根目录"
                  value={root}
                  onChange={(e) =>
                    guard(() => {
                      setRoot(e.target.value);
                      setPath("");
                      setOffset(0);
                    })
                  }
                >
                  {availableRoots.map((r) => (
                    <option key={r.id} value={r.id}>
                      {r.label || r.id}
                      {r.read_only ? " · 只读" : ""}
                    </option>
                  ))}
                </select>
              </label>
              <form
                onSubmit={(e) => {
                  e.preventDefault();
                  setSearch(query);
                  setOffset(0);
                }}
              >
                <TextInput
                  label="按文件名筛选"
                  isLabelHidden
                  value={query}
                  onChange={setQuery}
                  placeholder="当前目录文件名"
                />
                <Button
                  label="筛选文件"
                  icon={<Icon name="search" />}
                  isIconOnly
                  type="submit"
                />
              </form>
              {has("fs_trash_list") && (
                <Button
                  label="回收站"
                  variant="ghost"
                  onClick={() => setTrashOpen(true)}
                />
              )}
            </div>
            <nav className="breadcrumbs" aria-label="文件路径">
              <button
                onClick={() =>
                  guard(() => {
                    setPath("");
                    setOffset(0);
                  })
                }
              >
                {currentRoot?.label || root || "选择目录"}
              </button>
              {path
                .split("/")
                .filter(Boolean)
                .map((segment, index, array) => (
                  <span key={index}>
                    <Icon name="arrow" size={14} />
                    <button
                      onClick={() =>
                        guard(() => {
                          setPath(array.slice(0, index + 1).join("/"));
                          setOffset(0);
                        })
                      }
                    >
                      {segment}
                    </button>
                  </span>
                ))}
            </nav>
            {[error, roots.error, listing.error].filter(Boolean).map((e, i) => (
              <div className="notice danger" role="alert" key={i}>
                {e}
              </div>
            ))}
            {dirty && !editorOpen && (
              <div className="notice warning">
                文件草稿已保留。
                <Button
                  label="继续编辑文件草稿"
                  variant="ghost"
                  onClick={() => setEditorOpen(true)}
                />
              </div>
            )}
            {listing.loading && (
              <p className="muted" role="status">
                正在读取目录…
              </p>
            )}
            <div className="resource-table-wrap">
              <table className="resource-table">
                <thead>
                  <tr>
                    <th>名称</th>
                    <th>大小</th>
                    <th>权限 / 所有者</th>
                    <th>修改时间</th>
                    <th>操作</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((entry) => (
                    <tr key={entry.path}>
                      <td>
                        <button
                          className="resource-name"
                          onClick={() => enter(entry)}
                        >
                          <Icon
                            name={entry.type === "directory" ? "files" : "note"}
                            size={17}
                          />
                          {entry.name}
                        </button>
                        <small>
                          {entry.type === "directory"
                            ? "目录"
                            : entry.type === "symlink"
                              ? "符号链接"
                              : "文件"}
                        </small>
                      </td>
                      <td>
                        {entry.type === "directory" ? "—" : bytes(entry.size)}
                      </td>
                      <td>
                        <code>{permissions(entry.mode)}</code>
                        <small>
                          UID {entry.owner ?? "—"} / GID {entry.group ?? "—"}
                        </small>
                      </td>
                      <td>{fullDate(entry.modified_at)}</td>
                      <td>
                        <div className="button-row">
                          {entry.type === "file" && (
                            <Button
                              label={`下载 ${entry.name}`}
                              href={downloadURL(root, entry.path)}
                              variant="ghost"
                            />
                          )}
                          <Button
                            label={`操作 ${entry.name}`}
                            variant="ghost"
                            onClick={() =>
                              guard(() => {
                                setSelected(entry);
                                setOperation("operations");
                              })
                            }
                          />
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            {!rows.length && !listing.loading && !listing.error && (
              <Unavailable
                title="目录为空"
                detail="创建文件、目录或上传文件。"
                icon="files"
              />
            )}
            <Pager
              offset={offset}
              limit={50}
              total={listing.data?.total}
              count={rows.length}
              loading={listing.loading}
              onOffset={setOffset}
            />
          </>
        )}
      </Card>
      {create && (
        <ResourceDrawer
          open={createOpen}
          title={createName === "fs_mkdir" ? "新建目录" : "新建文件"}
          onClose={() => setCreateOpen(false)}
        >
          <ActionForm
            key={createName}
            capability={create}
            preset={{
              root,
              path: join(
                path,
                createName === "fs_mkdir" ? "new-folder" : "new-file.txt",
              ),
            }}
            onExecuted={refresh}
          />
        </ResourceDrawer>
      )}
      {selected && selected.type === "file" && (
        <FileEditor
          key={root + ":" + selected.path}
          root={root}
          entry={selected}
          open={editorOpen}
          onClose={() => setEditorOpen(false)}
          onSaved={refresh}
          onDirty={setDirty}
          writable={!currentRoot?.read_only && has("fs_edit")}
          capabilities={capabilities}
        />
      )}
      {selected && (
        <ResourceDrawer
          open={!!operation}
          title={`文件操作 · ${selected.name}`}
          onClose={() => setOperation(undefined)}
        >
          <ResourceActions
            capabilities={capabilities}
            names={(name) =>
              /^fs_/.test(name) &&
              ![
                "fs_roots",
                "fs_list",
                "fs_read",
                "fs_formats",
                "fs_upload_begin",
                "fs_upload_status",
                "fs_upload_commit",
                "fs_upload_cancel",
                "fs_trash_list",
              ].includes(name)
            }
            preset={{ root, path: selected.path }}
            selected={operation === "operations" ? undefined : operation}
            onSelect={(value) => setOperation(value ?? "operations")}
            onExecuted={refresh}
          />
        </ResourceDrawer>
      )}
      {root && (
        <Uploader
          open={uploadOpen}
          root={root}
          directory={path}
          onClose={() => setUploadOpen(false)}
          onSaved={refresh}
        />
      )}
      <ResourceDrawer
        open={trashOpen}
        title="文件回收站"
        onClose={() => setTrashOpen(false)}
      >
        {trash.error && <div className="notice danger">{trash.error}</div>}
        {list(trash.data)
          .filter((t) => t.state === "trashed")
          .map((item) => (
            <div className="trash-row" key={item.id}>
              <div>
                <strong>{item.path}</strong>
                <p>{fullDate(item.deleted_at)}</p>
              </div>
              <div className="button-row">
                {has("fs_restore") && (
                  <Button
                    label={`恢复 ${item.path}`}
                    onClick={() => void trashAction("fs_restore", item)}
                  />
                )}
                <details>
                  <summary>永久删除</summary>
                  {has("fs_purge") && (
                    <Button
                      label={`确认永久删除 ${item.path}`}
                      variant="destructive"
                      onClick={() => void trashAction("fs_purge", item)}
                    />
                  )}
                </details>
              </div>
            </div>
          ))}
        {!list(trash.data).filter((t) => t.state === "trashed").length &&
          !trash.loading && <Unavailable title="回收站为空" icon="files" />}
      </ResourceDrawer>
      <details className="advanced-actions">
        <summary>其他文件与归档操作</summary>
        <ActionBrowser
          capabilities={capabilities}
          category="files"
          selected={other}
          onSelect={setOther}
          onExecuted={refresh}
          preset={{ root, path }}
        />
      </details>
    </div>
  );
}
function FileEditor({
  root,
  entry,
  open,
  onClose,
  onSaved,
  onDirty,
  writable,
  capabilities,
}: {
  root: string;
  entry: FileEntry;
  open: boolean;
  onClose: () => void;
  onSaved: () => void;
  onDirty: (value: boolean) => void;
  writable: boolean;
  capabilities: Capability[];
}) {
  const [encoding, setEncoding] = useState("utf-8");
  const [readEncoding, setReadEncoding] = useState("utf-8");
  const source = useResource<FileContent>(
    "fs_read",
    { root, path: entry.path, encoding: readEncoding, limit: 2 * 1024 * 1024 },
    open,
    0,
  );
  const [base, setBase] = useState<FileContent>();
  const [draft, setDraft] = useState("");
  const [error, setError] = useState("");
  const [latest, setLatest] = useState<FileContent>();
  const [recovery, setRecovery] = useState(false);
  const [saving, setSaving] = useState(false);
  const [rereading, setRereading] = useState(false);
  const [status, setStatus] = useState("");
  const [history, setHistory] = useState<History[]>();
  const [historyText, setHistoryText] = useState<{
    content: string;
    encoding: string;
    revision: History;
  }>();
  const request = useRef<{ body: string; key: string } | undefined>(undefined);
  const dirty =
    !!base && (draft !== base.content || encoding !== base.encoding);
  const baseline = useRef(base);
  baseline.current = base;
  useEffect(() => {
    onDirty(dirty);
  }, [dirty, onDirty]);
  useEffect(() => {
    if (!source.data) return;
    if (
      base &&
      source.data.sha256 === base.sha256 &&
      source.data.encoding === base.encoding
    ) {
      if (source.data.size !== base.size)
        setBase({ ...base, size: source.data.size });
      setRereading(false);
      return;
    }
    if (dirty && !rereading) {
      setLatest(source.data);
      return;
    }
    setBase(source.data);
    setDraft(source.data.content);
    setEncoding(source.data.encoding);
    setRereading(false);
  }, [source.data]);
  useEffect(() => {
    if (source.error && !source.loading) setRereading(false);
  }, [source.error, source.loading]);
  useEffect(() => {
    if (!dirty) return;
    const before = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener("beforeunload", before);
    return () => window.removeEventListener("beforeunload", before);
  }, [dirty]);
  const save = async () => {
    if (!base || base.truncated || !writable || recovery || latest) return;
    setSaving(true);
    setError("");
    setStatus("正在保存");
    const body = JSON.stringify({
      root,
      path: entry.path,
      content: draft,
      encoding,
      expected_hash: base.sha256,
    });
    if (!request.current || request.current.body !== body)
      request.current = { body, key: intentKey() };
    try {
      let hash: string;
      let savedSize: number | undefined;
      if (new TextEncoder().encode(body).byteLength > 48 * 1024) {
        const query = new URLSearchParams({
          root,
          path: entry.path,
          encoding,
          expected_hash: base.sha256,
        });
        const response = await streamRequest(`/api/v1/files/text?${query}`, {
          method: "PUT",
          headers: {
            "Content-Type": "text/plain;charset=utf-8",
            "Idempotency-Key": request.current.key,
          },
          body: new Blob([draft], { type: "text/plain;charset=utf-8" }),
        });
        const result = await response.json();
        if (
          typeof result?.data?.sha256 !== "string" ||
          !/^[a-f0-9]{64}$/.test(result.data.sha256)
        )
          throw new APIError(
            "invalid_response",
            "服务没有返回已验证文件摘要，草稿已保留。",
            response.status,
          );
        hash = result.data.sha256;
        savedSize = result.data.entry?.size;
      } else {
        const result = await action<{
          sha256: string;
          entry?: { size: number };
        }>("fs_edit", JSON.parse(body), {
          idempotencyKey: request.current.key,
        });
        hash = result.sha256;
        savedSize = result.entry?.size;
      }
      setBase({
        ...base,
        content: draft,
        encoding,
        sha256: hash,
        size: savedSize,
      });
      setReadEncoding(encoding);
      setLatest(undefined);
      setStatus("已保存");
      request.current = undefined;
      onSaved();
    } catch (e) {
      setError((e as Error).message);
      setStatus("保存失败，草稿已保留");
      if (e instanceof APIError && e.code === "operation_recovery_required") {
        setRecovery(true);
        setStatus("操作状态待核对，草稿已保留");
      } else if (
        e instanceof APIError &&
        (e.status === 409 || e.status === 412)
      ) {
        try {
          setLatest(
            await action<FileContent>("fs_read", {
              root,
              path: entry.path,
              encoding: readEncoding,
              limit: 2 * 1024 * 1024,
            }),
          );
        } catch {}
      }
    } finally {
      setSaving(false);
    }
  };
  const loadHistory = async () => {
    try {
      setHistory(
        list(await action<History[]>("fs_history", { root, path: entry.path })),
      );
    } catch (e) {
      setError((e as Error).message);
    }
  };
  return (
    <ResourceDrawer
      open={open}
      title={`编辑文件 · ${entry.name}`}
      onClose={onClose}
    >
      <div className="file-editor-toolbar">
        <label className="native-field">
          编码
          <select
            aria-label="文件编码"
            value={encoding}
            onChange={(e) => setEncoding(e.target.value)}
          >
            {["utf-8", "gb18030", "gbk", "big5"].map((value) => (
              <option key={value} value={value}>
                {value}
              </option>
            ))}
          </select>
        </label>
        <Button
          label="按所选编码重新读取"
          isDisabled={
            (!!base && draft !== base.content) || saving || source.loading
          }
          onClick={() => {
            setRereading(true);
            if (readEncoding === encoding) void source.reload();
            else setReadEncoding(encoding);
          }}
        />
        <Button
          label="下载原文件"
          href={downloadURL(root, entry.path)}
          variant="ghost"
        />
        {capabilities.some((c) => c.name === "fs_history") && (
          <Button
            label="文件修改历史"
            icon={<Icon name="history" />}
            onClick={() => void loadHistory()}
          />
        )}
      </div>
      {(error || source.error) && (
        <div className="notice danger" role="alert">
          {error || source.error}
          <Button
            label={error ? "重试保存文件" : "重新读取文件"}
            variant="ghost"
            isDisabled={!!error && (!writable || recovery || !!latest)}
            onClick={() => (error ? void save() : void source.reload())}
          />
          {recovery && (
            <p>
              上次保存的状态尚未确认。请核对关联任务、审计与磁盘内容；当前草稿暂停写入，避免重复执行。
            </p>
          )}
        </div>
      )}
      {latest && !recovery && (
        <section className="notice warning" aria-label="文件版本冲突">
          <strong>文件已有新版本，草稿不会被覆盖。</strong>
          <details>
            <summary>查看最新文件</summary>
            <pre>{latest.content}</pre>
          </details>
          <Button
            label="采用最新基线并手动合并"
            onClick={() => {
              setBase(latest);
              setLatest(undefined);
              setError("");
              setStatus("请手动合并后保存");
            }}
          />
        </section>
      )}
      {source.loading && !base ? (
        <p role="status">正在读取文件…</p>
      ) : base ? (
        <>
          <p className="muted">
            {root} / {entry.path} ·{" "}
            {typeof base.size === "number" ? bytes(base.size) : "大小读取中"} ·
            SHA-256 {base.sha256.slice(0, 16)}…
          </p>
          {base.truncated && (
            <div className="notice warning">
              文件超过在线编辑上限，仅显示预览。请下载后编辑并上传，不能保存截断内容。
            </div>
          )}
          <CodeEditor
            value={draft}
            onChange={setDraft}
            filename={entry.name}
            readOnly={!writable || base.truncated || saving || rereading}
          />
        </>
      ) : null}
      {history && (
        <section className="history" aria-label="文件历史版本">
          <div className="split-label">
            <h3>历史版本</h3>
            <Button
              label="关闭文件历史"
              variant="ghost"
              onClick={() => {
                setHistory(undefined);
                setHistoryText(undefined);
              }}
            />
          </div>
          {history.map((rev) => (
            <div className="history-row" key={rev.id}>
              <button
                className="text-button"
                onClick={() =>
                  void action<typeof historyText>("fs_history", {
                    root,
                    path: entry.path,
                    id: rev.id,
                  })
                    .then((value) => setHistoryText(value))
                    .catch((e) => setError((e as Error).message))
                }
              >
                {fullDate(rev.created_at)} · {rev.actor_id} · {bytes(rev.size)}
              </button>
            </div>
          ))}
          {historyText && (
            <div className="preview">
              <pre>{historyText.content}</pre>
              {writable && (
                <Button
                  label="采用此历史内容"
                  onClick={() => {
                    setDraft(historyText.content);
                    setEncoding(historyText.encoding);
                    setHistoryText(undefined);
                    setStatus("历史内容已载入草稿，请保存为新版本");
                  }}
                />
              )}
            </div>
          )}
        </section>
      )}
      <div className="dialog-footer">
        <span role="status" className="save-status">
          {saving ? "正在保存" : dirty ? "文件草稿未保存" : status || "已读取"}
        </span>
        <div className="button-row">
          {dirty && (
            <Button
              label="放弃文件草稿"
              variant="ghost"
              onClick={() => {
                if (base) {
                  setDraft(base.content);
                  setEncoding(base.encoding);
                }
                setError("");
                setLatest(undefined);
              }}
            />
          )}
          {writable && (
            <Button
              label="保存文件"
              variant="primary"
              isLoading={saving}
              isDisabled={
                !base ||
                base.truncated ||
                !!latest ||
                source.loading ||
                recovery
              }
              onClick={() => void save()}
            />
          )}
          <Button
            label="完成文件编辑"
            onClick={() => {
              if (dirty) {
                setError("草稿尚未保存。请保存或明确放弃后完成。");
                return;
              }
              onClose();
            }}
          />
        </div>
      </div>
    </ResourceDrawer>
  );
}
function Uploader({
  open,
  root,
  directory,
  onClose,
  onSaved,
}: {
  open: boolean;
  root: string;
  directory: string;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [file, setFile] = useState<File>();
  const [path, setPath] = useState("");
  const [session, setSession] = useState<Upload>();
  const [uploadRoot, setUploadRoot] = useState(root);
  const [overwrite, setOverwrite] = useState(false);
  const input = useRef<HTMLInputElement>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [percent, setPercent] = useState(0);
  const [stage, setStage] = useState("");
  const abort = useRef<AbortController | undefined>(undefined);
  const run = async () => {
    if (!file || !path) {
      setError("请选择文件并填写目标路径。");
      return;
    }
    setBusy(true);
    setError("");
    abort.current = new AbortController();
    try {
      await uploadFile({
        root: uploadRoot,
        path,
        file,
        previous: session,
        overwrite,
        signal: abort.current.signal,
        onSession: setSession,
        onProgress: (value, label) => {
          setPercent(value);
          setStage(label);
        },
      });
      setStage("已校验并保存");
      setSession(undefined);
      setFile(undefined);
      setOverwrite(false);
      if (input.current) input.current.value = "";
      onSaved();
    } catch (e) {
      setError((e as Error).message);
      setStage("上传暂停，可续传");
    } finally {
      setBusy(false);
    }
  };
  return (
    <ResourceDrawer open={open} title="上传文件" onClose={onClose}>
      <div className="form-stack">
        <label className="native-field">
          选择本地文件
          <input
            ref={input}
            type="file"
            aria-label="本地上传文件"
            disabled={busy || !!session}
            onChange={(e) => {
              const chosen = e.target.files?.[0];
              setFile(chosen);
              setOverwrite(false);
              if (chosen) {
                setPath(join(directory, chosen.name));
                setUploadRoot(root);
              }
              setError("");
            }}
          />
        </label>
        <TextInput
          label="上传目标路径"
          value={path}
          onChange={setPath}
          isDisabled={busy || !!session}
        />
        <label className="check-label">
          <input
            type="checkbox"
            checked={overwrite}
            disabled={busy || !!session}
            onChange={(e) => setOverwrite(e.target.checked)}
          />
          覆盖同路径已有文件
        </label>
        {file && (
          <p className="muted">
            目标 {uploadRoot} / {path} · {file.name} · {bytes(file.size)} ·
            分段传输与SHA-256验证
          </p>
        )}
        {error && (
          <div className="notice danger" role="alert">
            {error}
          </div>
        )}
        {stage && (
          <div role="status">
            <progress value={percent} max={100} aria-label="文件上传进度" />
            <p>
              {stage} · {percent.toFixed(0)}%
            </p>
          </div>
        )}
        <div className="button-row">
          <Button
            label={session ? "继续上传" : "开始上传"}
            variant="primary"
            isLoading={busy}
            isDisabled={!file}
            onClick={() => void run()}
          />
          {busy && (
            <Button label="暂停上传" onClick={() => abort.current?.abort()} />
          )}{" "}
          {!busy && session && (
            <Button
              label="取消此上传"
              onClick={() =>
                void action(
                  "fs_upload_cancel",
                  { root: uploadRoot, id: session.id },
                  { idempotencyKey: intentKey() },
                )
                  .then(() => {
                    setSession(undefined);
                    setFile(undefined);
                    setOverwrite(false);
                    if (input.current) input.current.value = "";
                    setStage("已取消");
                  })
                  .catch((e) => setError((e as Error).message))
              }
            />
          )}
        </div>
        <p className="muted">
          失败或断线时保留所选文件与上传会话；重新打开此窗口可续传。服务器仍会核对最终大小、摘要与目标版本。
        </p>
      </div>
    </ResourceDrawer>
  );
}
