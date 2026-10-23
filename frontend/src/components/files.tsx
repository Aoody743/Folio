"use client";
import { t as translate } from "@/lib/i18n";
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
  ResourceError,
  ResourceActions,
  ResourceDrawer,
  Unavailable,
} from "./resource-ui";
import { addNavigationGuard, usePageState } from "@/lib/navigation";
import { usePageSize } from "@/lib/collection";
import { FileBatch } from "./file-batch";
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
  const [root, setRoot] = usePageState("files", "root", "");
  const [path, setPath] = usePageState("files", "path", "");
  const [offset, setOffset] = usePageState("files", "offset", 0);
  const { ref: listRef, limit } = usePageSize(72);
  const [sortBy,setSortBy]=usePageState("files","sort","name");
  const [sortOrder,setSortOrder]=usePageState("files","order","asc");
  const [checked,setChecked]=useState<string[]>([]);
  const [batchOpen,setBatchOpen]=useState(false);
  const [batchSelection,setBatchSelection]=useState<FileEntry[]>([]);
  useEffect(()=>{setChecked([]);setBatchOpen(false);},[root,path,offset,sortBy,sortOrder]);
  const [query, setQuery] = useState("");
  const [search, setSearch] = usePageState("files", "q", "");
  useEffect(() => {
    setQuery(search);
  }, [search]);
  const [selectedPath, setSelectedPath] = usePageState(
    "files",
    "file",
    "",
    true,
  );
  const [selectedLocal, setSelectedLocal] = useState<FileEntry>();
  const selected =
    selectedLocal?.path === selectedPath
      ? selectedLocal
      : selectedPath
        ? ({
            name: selectedPath.split("/").at(-1) ?? selectedPath,
            path: selectedPath,
            type: "file",
            size: 0,
            modified_at: "",
          } as FileEntry)
        : undefined;
  const setSelected = (value?: FileEntry) => {
    setSelectedLocal(value);
    setSelectedPath(value?.path ?? "");
  };
  const [view, setView] = usePageState("files", "view", "");
  const editorOpen = !!selectedPath && view === "editor";
  const setEditorOpen = (open: boolean) => setView(open ? "editor" : "");
  const [dirty, setDirty] = useState(false);
  const [error, setError] = useState("");
  const [operation, setOperation] = useState<string>();
  const [createOpen, setCreateOpen] = usePageState(
    "files",
    "create",
    false,
    true,
  );
  const [createName, setCreateName] = useState("fs_create");
  const [uploadOpen, setUploadOpen] = usePageState("files", "new", false, true);
  const [trashOpen, setTrashOpen] = usePageState(
    "files",
    "filter",
    false,
    true,
  );
  const [other, setOther] = useState<string>();
  useEffect(() => {
    if (!dirty) return;
    const blocked = () => {
      setError(fileDraftGuard);
      setEditorOpen(true);
    };
    window.addEventListener("folio:navigation-blocked", blocked);
    const stop = addNavigationGuard(
      (next, current) =>
        next.section !== "files" ||
        current.section !== "files" ||
        ["root", "path", "file"].every(
          (key) => (next.params[key] ?? "") === (current.params[key] ?? ""),
        ),
    );
    return () => {
      stop();
      window.removeEventListener("folio:navigation-blocked", blocked);
    };
  }, [dirty]);
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
      setError(
        translate("该资源目录当前不可访问，请检查目录是否仍存在及访问权限。"),
      );
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
      limit,
      offset,
      sort: ["name","size","modified_at"].includes(sortBy)?sortBy:"name",
      order: sortOrder==="desc"?"desc":"asc",
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
  const batchItems=rows.filter(row=>checked.includes(row.path));
  useEffect(()=>{if(!listing.loading && listing.data)setChecked(old=>old.filter(p=>list(listing.data).some(i=>i.path===p)));},[listing.data,listing.loading]);
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
      } else
        setError(
          translate("符号链接与特殊文件不能直接编辑，请使用受控文件操作。"),
        );
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
          <h2>{translate("文件管理")}</h2>
          <div className="button-row">
            {!currentRoot?.read_only &&
              (has("fs_create") || has("fs_mkdir")) && (
                <details className="named-menu">
                  <summary>{translate("新建")}</summary>
                  <div>
                    {has("fs_create") && (
                      <Button
                        label={translate("新建文件")}
                        onClick={() => {
                          setCreateName("fs_create");
                          setCreateOpen(true);
                        }}
                      />
                    )}
                    {has("fs_mkdir") && (
                      <Button
                        label={translate("新建目录")}
                        onClick={() => {
                          setCreateName("fs_mkdir");
                          setCreateOpen(true);
                        }}
                      />
                    )}
                  </div>
                </details>
              )}
            {has("fs_upload_begin") && !currentRoot?.read_only && (
              <Button
                label={translate("上传文件")}
                variant="primary"
                onClick={() => setUploadOpen(true)}
              />
            )}
          </div>
        </div>
        {!has("fs_roots") ? (
          <Unavailable title={translate("文件资源服务暂不可用")} icon="files" />
        ) : (
          <>
            <div className="file-toolbar">
              <label className="native-field file-root-field">
                {translate("资源目录")}
                <select
                  aria-label={translate("文件根目录")}
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
                      {r.read_only ? translate(" · 只读") : ""}
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
                  label={translate("按文件名筛选")}
                  isLabelHidden
                  value={query}
                  onChange={setQuery}
                  placeholder={translate("当前目录文件名")}
                />
                <Button
                  label={translate("筛选文件")}
                  icon={<Icon name="search" />}
                  isIconOnly
                  type="submit"
                />
              </form>
              <label className="native-field file-sort-field">{translate("排序")}<select aria-label={translate("文件排序")} value={sortBy+":"+sortOrder} onChange={e=>{const [sort,order]=e.target.value.split(":");setSortBy(sort);setSortOrder(order);setOffset(0);}}>
                <option value="name:asc">{translate("按名称升序")}</option><option value="name:desc">{translate("按名称降序")}</option>
                <option value="size:asc">{translate("按大小升序")}</option><option value="size:desc">{translate("按大小降序")}</option>
                <option value="modified_at:desc">{translate("最近修改优先")}</option><option value="modified_at:asc">{translate("最早修改优先")}</option>
              </select></label>
              {has("fs_batch") && has("fs_preflight") && !currentRoot?.read_only && batchItems.length>0 && <Button label={translate("批量操作 ({0})",[batchItems.length])} onClick={()=>guard(()=>{setBatchSelection(batchItems);setBatchOpen(true);} )}/>}
              {has("fs_trash_list") && (
                <Button
                  label={translate("回收站")}
                  variant="ghost"
                  onClick={() => setTrashOpen(true)}
                />
              )}
            </div>
            <nav className="breadcrumbs" aria-label={translate("文件路径")}>
              <button
                onClick={() =>
                  guard(() => {
                    setPath("");
                    setOffset(0);
                  })
                }
              >
                {currentRoot?.label || root || translate("选择目录")}
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
              <ResourceError error={translate(e)} key={i} retry={refresh} />
            ))}
            {dirty && !editorOpen && (
              <div className="notice warning">
                {translate("文件草稿已保留。")}
                <Button
                  label={translate("继续编辑文件草稿")}
                  variant="ghost"
                  onClick={() => setEditorOpen(true)}
                />
              </div>
            )}
            {listing.loading && !listing.data && (
              <p className="muted" role="status">
                {translate("正在读取目录…")}
              </p>
            )}
            <div ref={listRef} className="resource-table-wrap">
              <table className="resource-table">
                <thead>
                  <tr>
                    <th>{has("fs_batch") && has("fs_preflight") && !currentRoot?.read_only && <input type="checkbox" aria-label={translate("选择本页文件")} checked={rows.filter(r=>r.type!=="symlink").length>0 && rows.filter(r=>r.type!=="symlink").every(r=>checked.includes(r.path))} onChange={e=>setChecked(e.target.checked?rows.filter(r=>r.type!=="symlink").map(r=>r.path):[])}/>} {translate("名称")}</th>
                    <th>{translate("大小")}</th>
                    <th>{translate("权限 / 所有者")}</th>
                    <th>{translate("修改时间")}</th>
                    <th>{translate("操作")}</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((entry) => (
                    <tr key={entry.path}>
                      <td>
                        {has("fs_batch") && has("fs_preflight") && !currentRoot?.read_only && entry.type!=="symlink" && <input type="checkbox" aria-label={translate("选择 {0}",[entry.name])} checked={checked.includes(entry.path)} onChange={e=>setChecked(old=>e.target.checked?[...old,entry.path]:old.filter(p=>p!==entry.path))}/>}
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
                            ? translate("目录")
                            : entry.type === "symlink"
                              ? translate("符号链接")
                              : translate("文件")}
                        </small>
                      </td>
                      <td>
                        {entry.type === "directory" ? "—" : bytes(entry.size)}
                      </td>
                      <td>
                        <code>{permissions(entry.mode)}</code>
                        <small>
                          UID {entry.owner ?? "—"}/ GID {entry.group ?? "—"}
                        </small>
                      </td>
                      <td>{fullDate(entry.modified_at)}</td>
                      <td>
                        <div className="button-row">
                          {entry.type === "file" && (
                            <Button
                              label={translate("下载")}
                              aria-label={translate("下载 {0}", [entry.name])}
                              href={downloadURL(root, entry.path)}
                              variant="ghost"
                            />
                          )}
                          <Button
                            label={translate("操作")}
                            aria-label={translate("操作 {0}", [entry.name])}
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
                title={translate("目录为空")}
                detail={translate("创建文件、目录或上传文件。")}
                icon="files"
              />
            )}
            <Pager
              offset={offset}
              limit={limit}
              total={listing.data?.total}
              count={rows.length}
              loading={listing.loading}
              onOffset={setOffset}
            />
          </>
        )}
      </Card>
      {batchOpen && batchSelection.length>0 && <FileBatch root={root} path={path} items={batchSelection} roots={availableRoots} capabilities={capabilities} onClose={()=>setBatchOpen(false)} onSaved={refresh}/>}
      {create && (
        <ResourceDrawer
          open={createOpen}
          title={
            createName === "fs_mkdir"
              ? translate("新建目录")
              : translate("新建文件")
          }
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
          title={translate("文件操作 · {0}", [selected.name])}
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
        title={translate("文件回收站")}
        onClose={() => setTrashOpen(false)}
      >
        {trash.error && <ResourceError error={trash.error} state={trash.state} retry={() => void trash.reload()} />}
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
                    label={translate("恢复 {0}", [item.path])}
                    onClick={() => void trashAction("fs_restore", item)}
                  />
                )}
                <details>
                  <summary>{translate("永久删除")}</summary>
                  {has("fs_purge") && (
                    <Button
                      label={translate("确认永久删除 {0}", [item.path])}
                      variant="destructive"
                      onClick={() => void trashAction("fs_purge", item)}
                    />
                  )}
                </details>
              </div>
            </div>
          ))}
        {!list(trash.data).filter((t) => t.state === "trashed").length &&
          !trash.loading && !trash.error && (
            <Unavailable title={translate("回收站为空")} icon="files" />
          )}
      </ResourceDrawer>
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
    setStatus(translate("正在保存"));
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
            translate("服务没有返回已验证文件摘要，草稿已保留。"),
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
      setStatus(translate("已保存"));
      request.current = undefined;
      onSaved();
    } catch (e) {
      setError((e as Error).message);
      setStatus(translate("保存失败，草稿已保留"));
      if (e instanceof APIError && e.code === "operation_recovery_required") {
        setRecovery(true);
        setStatus(translate("操作状态待核对，草稿已保留"));
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
      title={translate("编辑文件 · {0}", [entry.name])}
      onClose={onClose}
    >
      <div className="file-editor-toolbar">
        <label className="native-field">
          {translate("编码")}
          <select
            aria-label={translate("文件编码")}
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
          label={translate("按所选编码重新读取")}
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
          label={translate("下载原文件")}
          href={downloadURL(root, entry.path)}
          variant="ghost"
        />
        {capabilities.some((c) => c.name === "fs_history") && (
          <Button
            label={translate("文件修改历史")}
            icon={<Icon name="history" />}
            onClick={() => void loadHistory()}
          />
        )}
      </div>
      {(error || source.error) && (
        <div className="notice danger" role="alert">
          {error || source.error}
          <Button
            label={
              error ? translate("重试保存文件") : translate("重新读取文件")
            }
            variant="ghost"
            isDisabled={!!error && (!writable || recovery || !!latest)}
            onClick={() => (error ? void save() : void source.reload())}
          />
          {recovery && (
            <p>
              {translate(
                "上次保存的状态尚未确认。请核对关联任务、审计与磁盘内容；当前草稿暂停写入，避免重复执行。",
              )}
            </p>
          )}
        </div>
      )}
      {latest && !recovery && (
        <section
          className="notice warning"
          aria-label={translate("文件版本冲突")}
        >
          <strong>{translate("文件已有新版本，草稿不会被覆盖。")}</strong>
          <details>
            <summary>{translate("查看最新文件")}</summary>
            <pre>{latest.content}</pre>
          </details>
          <Button
            label={translate("采用最新基线并手动合并")}
            onClick={() => {
              setBase(latest);
              setLatest(undefined);
              setError("");
              setStatus(translate("请手动合并后保存"));
            }}
          />
        </section>
      )}
      {source.loading && !base ? (
        <p role="status">{translate("正在读取文件…")}</p>
      ) : base ? (
        <>
          <p className="muted">
            {root}/ {entry.path}
            {" · "}
            {typeof base.size === "number"
              ? bytes(base.size)
              : translate("大小读取中")}
            {" · SHA-256 "}
            {base.sha256.slice(0, 16)}…
          </p>
          {base.truncated && (
            <div className="notice warning">
              {translate(
                "文件超过在线编辑上限，仅显示预览。请下载后编辑并上传，不能保存截断内容。",
              )}
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
        <section className="history" aria-label={translate("文件历史版本")}>
          <div className="split-label">
            <h3>{translate("历史版本")}</h3>
            <Button
              label={translate("关闭文件历史")}
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
                {fullDate(rev.created_at)}· {rev.actor_id}· {bytes(rev.size)}
              </button>
            </div>
          ))}
          {historyText && (
            <div className="preview">
              <pre>{historyText.content}</pre>
              {writable && (
                <Button
                  label={translate("采用此历史内容")}
                  onClick={() => {
                    setDraft(historyText.content);
                    setEncoding(historyText.encoding);
                    setHistoryText(undefined);
                    setStatus(translate("历史内容已载入草稿，请保存为新版本"));
                  }}
                />
              )}
            </div>
          )}
        </section>
      )}
      <div className="dialog-footer">
        <span role="status" className="save-status">
          {saving
            ? translate("正在保存")
            : dirty
              ? translate("文件草稿未保存")
              : status || translate("已读取")}
        </span>
        <div className="button-row">
          {dirty && (
            <Button
              label={translate("放弃文件草稿")}
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
              label={translate("保存文件")}
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
            label={translate("完成文件编辑")}
            onClick={() => {
              if (dirty) {
                setError(translate("草稿尚未保存。请保存或明确放弃后完成。"));
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
type UploadEntry={file:File;path:string;session?:Upload;status:"pending"|"running"|"paused"|"completed"|"skipped";percent:number;error?:string};
function Uploader({open,root,directory,onClose,onSaved}:{open:boolean;root:string;directory:string;onClose:()=>void;onSaved:()=>void}) {
  const [entries,setEntries]=useState<UploadEntry[]>([]);
  const [uploadRoot,setUploadRoot]=useState(root);
  const [overwrite,setOverwrite]=useState(false);
  const [error,setError]=useState("");
  const [busy,setBusy]=useState(false);
  const [stage,setStage]=useState("");
  const input=useRef<HTMLInputElement>(null);
  const abort=useRef<AbortController | undefined>(undefined);
  const busyRef=useRef(false);
  const queueRef=useRef(entries);
  queueRef.current=entries;
  const patch=(index:number,value:Partial<UploadEntry>)=>{
    queueRef.current=queueRef.current.map((entry,i)=>i===index?{...entry,...value}:entry);
    setEntries(queueRef.current);
  };
  useEffect(()=>()=>abort.current?.abort(),[]);
  useEffect(()=>{
    const warn=(event:BeforeUnloadEvent)=>{if(busyRef.current || queueRef.current.some(e=>!!e.session && e.status!=="completed" && e.status!=="skipped")){event.preventDefault();event.returnValue="";}};
    window.addEventListener("beforeunload",warn);return()=>window.removeEventListener("beforeunload",warn);
  },[]);
  const pending=entries.filter(e=>e.status!=="completed" && e.status!=="skipped");
  const hasSession=entries.some(e=>!!e.session);
  const run=async()=>{
    if(busyRef.current || !pending.length)return;
    if(entries.some(e=>!e.path.trim()) || new Set(entries.map(e=>e.path)).size!==entries.length){setError(translate("目标路径不能为空或重复。"));return;}
    busyRef.current=true;setBusy(true);setError("");
    abort.current=new AbortController();
    try{
      for(let i=0;i<queueRef.current.length;i++){
        const entry=queueRef.current[i];
        if(entry.status==="completed" || entry.status==="skipped")continue;
        abort.current.signal.throwIfAborted();
        patch(i,{status:"running",error:undefined});
        try{
          await uploadFile({root:uploadRoot,path:entry.path,file:entry.file,previous:entry.session,overwrite,signal:abort.current.signal,onSession:session=>patch(i,{session}),onProgress:(percent,label)=>{patch(i,{percent});setStage(label);}});
          patch(i,{status:"completed",session:undefined,percent:100});onSaved();
        }catch(e){patch(i,{status:"paused",error:abort.current.signal.aborted?translate("上传已暂停"):(e as Error).message});throw e;}
      }
      setStage(translate("已校验并保存"));
    }catch(e){setStage(translate("上传暂停，可续传"));setError(abort.current.signal.aborted?translate("上传已暂停，可继续队列。"):(e as Error).message);}
    finally{busyRef.current=false;setBusy(false);}
  };
  const skip=async(index:number)=>{
    if(busyRef.current)return;
    busyRef.current=true;setBusy(true);setError("");
    try{const entry=queueRef.current[index];if(entry.session) await action("fs_upload_cancel",{root:uploadRoot,id:entry.session.id},{idempotencyKey:`upload-cancel-${entry.session.id}`});patch(index,{status:"skipped",session:undefined,error:undefined});}
    catch(e){setError((e as Error).message);}finally{busyRef.current=false;setBusy(false);}
  };
  const reset=async()=>{
    if(busyRef.current)return;
    busyRef.current=true;setBusy(true);setError("");
    try{
      for(let i=0;i<queueRef.current.length;i++)if(queueRef.current[i].session){const session=queueRef.current[i].session!;await action("fs_upload_cancel",{root:uploadRoot,id:session.id},{idempotencyKey:`upload-cancel-${session.id}`});patch(i,{session:undefined,status:"skipped"});}
      setEntries([]);queueRef.current=[];setOverwrite(false);setStage(translate("已取消"));if(input.current)input.current.value="";
    }catch(e){setError((e as Error).message);}finally{busyRef.current=false;setBusy(false);}
  };
  return <ResourceDrawer open={open} title={translate("上传文件")} onClose={onClose}>
    <div className="form-stack">
      <div className="file-picker"><Button label={translate("选择本地文件")} onClick={()=>input.current?.click()} isDisabled={busy || pending.length>0}/><span role="status">{entries.length===1?entries[0].file.name:entries.length?translate("已选择 {0} 个文件",[entries.length]):translate("尚未选择文件")}</span>
        <input ref={input} type="file" multiple className="file-picker-input" tabIndex={-1} aria-label={translate("本地上传文件")} disabled={busy || pending.length>0} onChange={event=>{
          const chosen=Array.from(event.target.files??[]);if(!chosen.length)return;
          if(chosen.length>100){setError(translate("每批最多选择 100 个文件。"));event.target.value="";return;}
          if(new Set(chosen.map(f=>f.name)).size!==chosen.length){setError(translate("选中的文件有同名项，请分批上传。"));event.target.value="";return;}
          setEntries(chosen.map(file=>({file,path:join(directory,file.name),status:"pending",percent:0})));setUploadRoot(root);setOverwrite(false);setError("");setStage("");
        }}/>
      </div>
      {entries.length===1 && <TextInput label={translate("上传目标路径")} value={entries[0].path} onChange={path=>patch(0,{path})} isDisabled={busy || hasSession || !pending.length}/>}
      <p className="muted">{translate("目标根目录")} {uploadRoot}{entries.length>1?` / ${directory || "."}`:""}</p>
      <label className="check-label"><input type="checkbox" checked={overwrite} disabled={busy || hasSession || !pending.length} onChange={e=>setOverwrite(e.target.checked)}/>{translate("覆盖同路径已有文件")}</label>
      {entries.length>0 && <ol className="upload-queue" aria-label={translate("上传队列")}>{entries.map((entry,index)=><li key={index}>
        <div><strong>{entry.file.name}</strong><span>{bytes(entry.file.size)} · {entry.status==="completed"?translate("已校验并保存"):entry.status==="skipped"?translate("已跳过"):entry.status==="running"?translate("上传中"):entry.status==="paused"?translate("上传已暂停"):translate("等待上传")}</span></div>
        {entry.path!==entry.file.name && <small>{entry.path}</small>}<progress max={100} value={entry.percent} aria-label={translate("文件上传进度")}/>
        {!busy && entry.status!=="completed" && entry.status!=="skipped" && <Button label={translate("跳过此文件")} variant="ghost" onClick={()=>void skip(index)}/>}
      </li>)}</ol>}
      {error && <ResourceError error={error}/>}
      {stage && <p role="status">{stage}</p>}
      <div className="button-row"><Button label={entries.some(e=>e.status==="paused")?translate("继续上传"):translate("开始上传")} variant="primary" isLoading={busy} isDisabled={!pending.length} onClick={()=>void run()}/>
        {busy && <Button label={translate("暂停上传")} onClick={()=>abort.current?.abort()}/>}
        {!busy && entries.length>0 && <Button label={translate("清空上传队列")} onClick={()=>void reset()}/>}
      </div>
      <p className="muted">{translate("逐个上传并校验 SHA-256；失败后暂停，继续时跳过已完成项。队列仅保留在本页面，刷新或退出会丢失本地文件选择。")}</p>
    </div>
  </ResourceDrawer>;
}
