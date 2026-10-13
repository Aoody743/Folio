import { sha256 } from "@noble/hashes/sha2.js";
import { action, intentKey, streamRequest, Task, APIError } from "./api";
export type Upload = {
  id: string;
  root: string;
  path: string;
  size: number;
  offset: number;
  sha256: string;
  state: string;
};
const hex = (value: Uint8Array) =>
  Array.from(value, (b) => b.toString(16).padStart(2, "0")).join("");
export async function fileHash(
  file: Blob,
  signal?: AbortSignal,
): Promise<string> {
  const hash = sha256.create();
  for (let offset = 0; offset < file.size; offset += 1024 * 1024) {
    signal?.throwIfAborted();
    hash.update(
      new Uint8Array(
        await file.slice(offset, offset + 1024 * 1024).arrayBuffer(),
      ),
    );
  }
  return hex(hash.digest());
}
export async function waitTask(
  id: string,
  signal?: AbortSignal,
  onTask?: (t: Task) => void,
): Promise<Task> {
  for (;;) {
    signal?.throwIfAborted();
    const task = await action<Task>("tasks_get", { id }, { signal });
    onTask?.(task);
    const state = task.status ?? task.state;
    if (state === "succeeded" || state === "verified" || state === "completed")
      return task;
    if (
      ["failed", "canceled", "cancelled", "interrupted"].includes(state ?? "")
    )
      throw new Error(task.error ?? `任务${state}`);
    await new Promise<void>((resolve, reject) => {
      const t = setTimeout(() => {
        signal?.removeEventListener("abort", abort);
        resolve();
      }, 500);
      const abort = () => {
        clearTimeout(t);
        reject(new DOMException("操作已取消", "AbortError"));
      };
      signal?.addEventListener("abort", abort, { once: true });
    });
  }
}
export async function uploadFile({
  root,
  path,
  file,
  expectedHash,
  overwrite,
  previous,
  signal,
  onProgress,
  onSession,
}: {
  root: string;
  path: string;
  file: Blob;
  expectedHash?: string;
  overwrite?: boolean;
  previous?: Upload;
  signal?: AbortSignal;
  onProgress?: (percent: number, stage: string) => void;
  onSession?: (s: Upload) => void;
}): Promise<{ upload: Upload; task: Task }> {
  onProgress?.(0, "校验本地文件");
  const checksum = await fileHash(file, signal);
  let upload: Upload;
  if (previous) {
    upload = await action<Upload>(
      "fs_upload_status",
      { root, id: previous.id },
      { signal },
    );
    if (
      upload.sha256 !== checksum ||
      upload.size !== file.size ||
      upload.path !== path
    )
      throw new Error("续传文件与原上传不一致。请取消原上传后重新开始。");
  } else
    upload = await action<Upload>(
      "fs_upload_begin",
      {
        root,
        path,
        size: file.size,
        sha256: checksum,
        ...(expectedHash ? { expected_hash: expectedHash } : {}),
        ...(overwrite ? { overwrite: true } : {}),
      },
      { signal, idempotencyKey: intentKey() },
    );
  onSession?.(upload);
  for (let offset = upload.offset; offset < file.size; ) {
    signal?.throwIfAborted();
    const buffer = new Uint8Array(
      await file.slice(offset, offset + 1024 * 1024).arrayBuffer(),
    );
    const query = new URLSearchParams({
      root,
      offset: String(offset),
      sha256: hex(sha256(buffer)),
    });
    await streamRequest(
      `/api/v1/files/uploads/${encodeURIComponent(upload.id)}/chunk?${query}`,
      {
        method: "PATCH",
        headers: { "Content-Type": "application/octet-stream" },
        body: buffer,
        signal,
      },
    );
    offset += buffer.byteLength;
    upload = { ...upload, offset };
    onSession?.(upload);
    onProgress?.(file.size ? (offset / file.size) * 100 : 100, "传输文件");
  }
  onProgress?.(100, "核对文件并提交");
  const submitted = await action<{ id?: string; task_id?: string }>(
    "fs_upload_commit",
    { root, id: upload.id },
    { signal, idempotencyKey: `upload-commit-${upload.id}` },
  );
  const id = submitted.id ?? submitted.task_id;
  if (!id) throw new Error("服务没有返回上传任务标识。");
  const task = await waitTask(id, signal);
  const committed = await action<Upload>(
    "fs_upload_status",
    { root, id: upload.id },
    { signal },
  );
  if (committed.sha256 !== checksum || committed.state !== "completed")
    throw new Error("上传产物尚未通过最终校验。");
  onProgress?.(100, "文件已验证并保存");
  return { upload: committed, task };
}
