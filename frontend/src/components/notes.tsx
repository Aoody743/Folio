"use client";
import { useCallback, useEffect, useRef, useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { TextInput } from "@astryxdesign/core/TextInput";
import { TextArea } from "@astryxdesign/core/TextArea";
import { Dialog } from "@astryxdesign/core/Dialog";
import { Markdown } from "@astryxdesign/core/Markdown";
import {
  api,
  APIError,
  fullDate,
  list,
  Note,
  Revision,
  source,
  intentKey,
} from "@/lib/api";
import { Icon } from "./icon";

export function NoteBody({ content }: { content: string }) {
  return (
    <Markdown
      className="note-prose"
      density="compact"
      headingLevelStart={3}
      components={{
        image: ({ alt }) => <span className="muted">[图片：{alt}]</span>,
        link: ({ href, children }) => (
          <a href={href} target="_blank" rel="noopener noreferrer">
            {children}
          </a>
        ),
      }}
    >
      {content}
    </Markdown>
  );
}
type Draft = {
  title: string;
  content: string;
  pinned: boolean;
  target_id: string;
};
const draftOf = (note?: Note): Draft => ({
  title: note?.title ?? "",
  content: note?.content ?? "",
  pinned: note?.pinned ?? false,
  target_id: note?.target_id ?? "host:local",
});
const same = (a: Draft, b: Draft) =>
  a.title === b.title &&
  a.content === b.content &&
  a.pinned === b.pinned &&
  a.target_id === b.target_id;
const validate = (d: Draft) =>
  !d.title.trim()
    ? "请填写便签标题。"
    : Array.from(d.title).length > 80
      ? "标题不能超过 80 个字符。"
      : Array.from(d.content).length > 10000
        ? "正文不能超过 10,000 个字符。"
        : new TextEncoder().encode(JSON.stringify(d)).length > 64000
          ? "便签请求过大，请缩短内容。"
          : "";

export function NoteEditor({
  note,
  open,
  onClose,
  onSaved,
  onDraftChange,
  canWrite = true,
  canDelete = false,
}: {
  note?: Note;
  open: boolean;
  onClose: () => void;
  onSaved: (note?: Note) => void;
  onDraftChange?: (dirty: boolean) => void;
  canWrite?: boolean;
  canDelete?: boolean;
}) {
  const [base, setBase] = useState(note);
  const [draft, setDraft] = useState(draftOf(note));
  const [editing, setEditing] = useState(!note);
  const [preview, setPreview] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [latest, setLatest] = useState<Note>();
  const [history, setHistory] = useState<Revision[]>();
  const [selected, setSelected] = useState<Revision>();
  const [deleting, setDeleting] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [status, setStatus] = useState("");
  const [manualMerge, setManualMerge] = useState(false);
  const inFlight = useRef(false);
  const writeIntent = useRef<{ body: string; key: string } | undefined>(
    undefined,
  );
  const draftRef = useRef(draft);
  draftRef.current = draft;
  const baseRef = useRef(base);
  baseRef.current = base;
  const dirty = !same(draft, draftOf(base));
  useEffect(() => {
    onDraftChange?.(dirty);
  }, [dirty, onDraftChange]);
  useEffect(() => {
    if (!note || !base || note.id !== base.id || note.revision <= base.revision)
      return;
    if (editing) setLatest(note);
    else {
      setBase(note);
      setDraft(draftOf(note));
    }
  }, [note, base, editing]);
  useEffect(() => {
    if (!dirty) return;
    const h = (e: BeforeUnloadEvent) => {
      e.preventDefault();
    };
    window.addEventListener("beforeunload", h);
    return () => window.removeEventListener("beforeunload", h);
  }, [dirty]);
  const save = useCallback(
    async (asNew = false): Promise<boolean> => {
      if (inFlight.current) return false;
      const current = { ...draftRef.current };
      const invalid = validate(current);
      if (invalid) {
        setError(invalid);
        return false;
      }
      inFlight.current = true;
      setSaving(true);
      setError("");
      setStatus("正在保存");
      try {
        const b = baseRef.current;
        const intentBody = JSON.stringify({
          id: b?.id,
          asNew,
          ...current,
          expected_revision: b?.revision,
        });
        if (!writeIntent.current || writeIntent.current.body !== intentBody)
          writeIntent.current = { body: intentBody, key: intentKey() };
        const next = await api<Note>(
          b && !asNew ? `/notes/${encodeURIComponent(b.id)}` : "/notes",
          {
            method: b && !asNew ? "PATCH" : "POST",
            headers: { "Idempotency-Key": writeIntent.current.key },
            body: {
              ...current,
              ...(b && !asNew ? { expected_revision: b.revision } : {}),
            },
          },
        );
        setBase(next);
        baseRef.current = next;
        setLatest(undefined);
        // Manual reconciliation pauses autosave only until that draft is saved.
        // The same mounted editor is reused on reopen; retaining this flag would
        // silently disable autosave for every subsequent edit of this note.
        setManualMerge(false);
        setStatus("已保存");
        writeIntent.current = undefined;
        onSaved(next);
        return true;
      } catch (e) {
        if (e instanceof APIError && e.status === 412) {
          const currentNote = (e.details as { current?: Note } | undefined)
            ?.current;
          if (currentNote) setLatest(currentNote);
          else if (baseRef.current) {
            try {
              setLatest(await api<Note>(`/notes/${baseRef.current.id}`));
            } catch {}
          }
          setError("内容已有新版本。草稿已保留，请查看最新版本并合并。");
        } else setError(e instanceof Error ? e.message : "保存失败，请重试。");
        setStatus("保存失败");
        return false;
      } finally {
        inFlight.current = false;
        setSaving(false);
      }
    },
    [onSaved],
  );
  useEffect(() => {
    if (
      !base ||
      !editing ||
      !dirty ||
      saving ||
      error ||
      latest ||
      manualMerge ||
      !open
    )
      return;
    const t = setTimeout(() => {
      void save();
    }, 800);
    return () => clearTimeout(t);
  }, [base, editing, dirty, saving, error, latest, manualMerge, open, save]);
  const loadHistory = async () => {
    setError("");
    try {
      setHistory(list(await api<Revision[]>(`/notes/${base?.id}/history`)));
    } catch (e) {
      setError((e as Error).message);
    }
  };
  const archive = async () => {
    if (!base) return;
    if (dirty && !(await save())) return;
    try {
      const current = baseRef.current!;
      const next = await api<Note>(
        `/notes/${current.id}/${current.archived ? "restore" : "archive"}`,
        { method: "POST", body: { expected_revision: current.revision } },
      );
      setBase(next);
      setDraft(draftOf(next));
      onSaved();
      onClose();
    } catch (e) {
      setError((e as Error).message);
    }
  };
  const restore = async (rev: number) => {
    if (!base) return;
    if (dirty && !(await save())) return;
    try {
      const next = await api<Note>(`/notes/${base.id}/restore`, {
        method: "POST",
        body: { expected_revision: baseRef.current!.revision, revision: rev },
      });
      setBase(next);
      setDraft(draftOf(next));
      setHistory(undefined);
      setSelected(undefined);
      setStatus("已恢复为新版本");
      onSaved();
    } catch (e) {
      setError((e as Error).message);
    }
  };
  const remove = async () => {
    if (!base) return;
    setDeleting(true);
    try {
      await api(`/notes/${base.id}`, {
        method: "DELETE",
        body: { expected_revision: base.revision },
      });
      onSaved();
      onClose();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setDeleting(false);
    }
  };
  const finish = async () => {
    if ((!base || dirty) && !(await save())) return;
    setEditing(false);
    onClose();
  };
  return (
    <Dialog
      isOpen={open}
      onOpenChange={(v) => {
        if (!v) onClose();
      }}
      purpose="form"
      width={640}
      maxHeight="90dvh"
      className="note-dialog"
      aria-label={base ? "便签详情" : "新建便签"}
      padding={6}
    >
      <div className="dialog-title">
        <div>
          <span className="eyebrow">共享信息</span>
          <h2>{base ? "便签详情" : "新建便签"}</h2>
        </div>
        <Button
          label="关闭便签"
          icon={<Icon name="close" />}
          isIconOnly
          variant="ghost"
          onClick={onClose}
        />
      </div>
      {error && (
        <div role="alert" className="notice danger">
          {error}
          {!latest && (
            <Button
              label="重试保存"
              variant="ghost"
              onClick={() => void save()}
            />
          )}
        </div>
      )}
      {latest && (
        <section className="conflict-box" aria-label="版本冲突">
          <strong>内容已有新版本 · v{latest.revision}</strong>
          <p>
            {source(latest.updated_by)} · {fullDate(latest.updated_at)}
          </p>
          <details>
            <summary>查看最新内容与我的草稿</summary>
            <div className="merge-compare">
              <div>
                <h3>最新内容</h3>
                <h4>{latest.title}</h4>
                <NoteBody content={latest.content} />
              </div>
              <div>
                <h3>我的草稿</h3>
                <h4>{draft.title}</h4>
                <NoteBody content={draft.content} />
              </div>
            </div>
          </details>
          <div className="button-row">
            <Button
              label="以最新版本为基础合并"
              onClick={() => {
                setBase(latest);
                baseRef.current = latest;
                setLatest(undefined);
                setError("");
                setStatus("已切换版本，请手动合并后完成");
                setManualMerge(true);
              }}
            />
            <Button
              label="保存为新便签"
              onClick={() => void save(true)}
              isLoading={saving}
            />
          </div>
          <small>切换版本会保留你的草稿。请先核对并合并，再保存。</small>
        </section>
      )}
      {editing && canWrite ? (
        <div className="form-stack">
          <TextInput
            label="标题"
            value={draft.title}
            onChange={(title) => {
              setDraft((d) => ({ ...d, title }));
              if (!latest) setError("");
            }}
            hasAutoFocus
            isRequired
            htmlName="title"
          />
          <div className="split-label">
            <span>正文</span>
            <button
              type="button"
              onClick={() => setPreview(!preview)}
              className="text-button"
            >
              {preview ? "继续编辑" : "Markdown 预览"}
            </button>
          </div>
          {preview ? (
            <div className="preview">
              <NoteBody content={draft.content} />
            </div>
          ) : (
            <TextArea
              label="正文"
              isLabelHidden
              value={draft.content}
              onChange={(content) => {
                setDraft((d) => ({ ...d, content }));
                if (!latest) setError("");
              }}
              rows={9}
              maxLength={10000}
              placeholder="记录待办、配置说明或交接信息…"
            />
          )}
          <TextInput
            label="关联资源"
            value={draft.target_id}
            onChange={(target_id) => setDraft((d) => ({ ...d, target_id }))}
            description="默认当前主机 host:local；填写已授权资源 ID。"
          />
          <label className="check-label">
            <input
              type="checkbox"
              checked={draft.pinned}
              onChange={(e) =>
                setDraft((d) => ({ ...d, pinned: e.target.checked }))
              }
            />
            置顶便签
          </label>
        </div>
      ) : (
        <div className="note-read">
          <h3>{base?.title}</h3>
          <NoteBody content={base?.content ?? ""} />
          {base?.target_id && (
            <p className="muted">关联资源 · {base.target_id}</p>
          )}
        </div>
      )}
      {base && (
        <p className="note-meta">
          {source(base.updated_by)} ·{" "}
          <time dateTime={base.updated_at}>{fullDate(base.updated_at)}</time> ·
          v{base.revision}
          {base.archived ? " · 已归档" : ""}
        </p>
      )}
      {history && (
        <section className="history" aria-label="修改历史">
          <div className="split-label">
            <h3>修改历史</h3>
            <Button
              label="关闭历史"
              variant="ghost"
              onClick={() => {
                setHistory(undefined);
                setSelected(undefined);
              }}
            />
          </div>
          {history.length ? (
            history.map((rev) => (
              <div key={rev.revision} className="history-row">
                <button
                  className="text-button"
                  onClick={() => setSelected(rev)}
                >
                  v{rev.revision} · {source(rev.updated_by)} ·{" "}
                  {fullDate(rev.updated_at)}
                </button>
                {canWrite && (
                  <Button
                    label={`恢复版本 ${rev.revision}`}
                    variant="ghost"
                    onClick={() => void restore(rev.revision)}
                  />
                )}
              </div>
            ))
          ) : (
            <p className="muted">暂无历史版本。</p>
          )}
          {selected && (
            <div className="preview">
              <strong>
                版本 {selected.revision} · {selected.title}
              </strong>
              <NoteBody content={selected.content} />
              <details>
                <summary>与当前版本比较</summary>
                <p className="muted">历史正文</p>
                <pre>{selected.content}</pre>
                <p className="muted">当前正文</p>
                <pre>{base?.content}</pre>
              </details>
            </div>
          )}
        </section>
      )}
      {confirmDelete && (
        <div className="notice danger">
          <p>永久删除此便签及其历史？此操作无法撤销。</p>
          <div className="button-row">
            <Button
              label="确认永久删除"
              variant="destructive"
              isLoading={deleting}
              onClick={() => void remove()}
            />
            <Button label="取消删除" onClick={() => setConfirmDelete(false)} />
          </div>
        </div>
      )}
      <div className="dialog-footer">
        <span className="save-status" role="status">
          {saving
            ? "正在保存"
            : dirty
              ? "草稿未保存"
              : status || (base ? "已保存" : "填写后创建")}
        </span>
        <div className="button-row">
          {editing && dirty && (
            <Button
              label="放弃当前草稿"
              variant="ghost"
              onClick={() => {
                setDraft(draftOf(base));
                setLatest(undefined);
                setError("");
                setManualMerge(false);
                setEditing(!base);
              }}
            />
          )}
          {base && (
            <Button
              label="修改历史"
              icon={<Icon name="history" />}
              variant="ghost"
              onClick={() => void loadHistory()}
            />
          )}
          {base && canWrite && !editing && (
            <Button
              label={base.archived ? "取消归档" : "归档"}
              variant="ghost"
              onClick={() => void archive()}
            />
          )}
          {base && canDelete && !editing && (
            <Button
              label="永久删除"
              variant="ghost"
              onClick={() => setConfirmDelete(true)}
            />
          )}
          {canWrite &&
            (editing ? (
              <Button
                label={base ? "完成" : "创建便签"}
                variant="primary"
                isLoading={saving}
                isDisabled={!!latest}
                onClick={() => void finish()}
              />
            ) : (
              <Button
                label="编辑"
                variant="primary"
                onClick={() => setEditing(true)}
              />
            ))}
        </div>
      </div>
      {!open && dirty && <span className="sr-only">草稿已保留</span>}
    </Dialog>
  );
}
