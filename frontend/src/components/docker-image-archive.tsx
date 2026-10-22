"use client";
import { t as translate } from "@/lib/i18n";
import { useState } from "react";
import { sha256 } from "@noble/hashes/sha2.js";
import { Button } from "@astryxdesign/core/Button";
import { TextInput } from "@astryxdesign/core/TextInput";
import { action, APIError, bytes, Capability, Task, fullDate } from "@/lib/api";
import {
  DOCKER_ARCHIVE_CHUNK,
  DOCKER_ARCHIVE_MAX_BYTES,
  dockerArchiveCapability,
  dockerArchiveChunkReceipt,
  dockerArchiveExportResultForImage,
  dockerArchiveImportResult,
  dockerArchiveManifest,
  dockerArchiveManifestText,
  dockerArchiveReadChunk,
  dockerArchiveTaskID,
  dockerArchiveUploadBegin,
  dockerArchiveUploadStatus,
  DockerArchiveManifest,
  DockerArchiveUploadStatus,
} from "@/lib/docker-archive";
import { TaskResult } from "./task-result";
import { State, Unavailable } from "./resource-ui";

type SaveFile = {
  createWritable: () => Promise<{
    write: (chunk: Uint8Array) => Promise<void>;
    close: () => Promise<void>;
    abort?: () => Promise<void>;
  }>;
};
type SaveFilePicker = (options: {
  suggestedName: string;
  types?: { description: string; accept: Record<string, string[]> }[];
}) => Promise<SaveFile>;

const hex = (value: Uint8Array) =>
  Array.from(value, (byte) => byte.toString(16).padStart(2, "0")).join("");
const base64 = (value: Uint8Array) => {
  let binary = "";
  for (let offset = 0; offset < value.length; offset += 8192)
    binary += String.fromCharCode(...value.subarray(offset, offset + 8192));
  return btoa(binary);
};
const ambiguous = (error: unknown) =>
  error instanceof APIError &&
  (error.status === 0 ||
    error.status >= 500 ||
    error.code === "invalid_response");
const safeStem = (image: string) =>
  image.replace(/[^A-Za-z0-9._-]+/g, "_").slice(0, 96) || "docker-image";

async function hashFile(file: File, progress: (value: number) => void) {
  if (file.size < 1 || file.size > DOCKER_ARCHIVE_MAX_BYTES)
    throw new Error(translate("归档文件大小必须在 1 字节至 4 GiB 之间。"));
  const hash = sha256.create();
  for (let offset = 0; offset < file.size; offset += DOCKER_ARCHIVE_CHUNK) {
    const chunk = new Uint8Array(
      await file.slice(offset, offset + DOCKER_ARCHIVE_CHUNK).arrayBuffer(),
    );
    hash.update(chunk);
    progress(Math.min(offset + chunk.length, file.size));
  }
  return hex(hash.digest());
}

function downloadText(name: string, text: string) {
  const url = URL.createObjectURL(
    new Blob([text], { type: "application/json;charset=utf-8" }),
  );
  const link = document.createElement("a");
  link.href = url;
  link.download = name;
  link.click();
  window.setTimeout(() => URL.revokeObjectURL(url), 0);
}

function downloadParts(name: string, parts: BlobPart[]) {
  const url = URL.createObjectURL(
    new Blob(parts, { type: "application/x-tar" }),
  );
  const link = document.createElement("a");
  link.href = url;
  link.download = name;
  link.click();
  window.setTimeout(() => URL.revokeObjectURL(url), 0);
}

export function DockerImageArchiveManagement({
  instanceID,
  image,
  capabilities,
  onExecuted,
}: {
  instanceID: string;
  image: string;
  capabilities: Capability[];
  onExecuted: () => void;
}) {
  const exportCapability = dockerArchiveCapability(
    capabilities,
    "docker_image_archive_export",
    "containers:read",
    ["id", "image"],
    true,
    false,
  );
  const readCapability = dockerArchiveCapability(
    capabilities,
    "docker_image_archive_read",
    "containers:read",
    ["id", "image", "archive_id", "offset", "length"],
    false,
    true,
  );
  const beginCapability = dockerArchiveCapability(
    capabilities,
    "docker_image_archive_upload_begin",
    "containers:manage",
    ["id", "image", "proof"],
    false,
    false,
  );
  const chunkCapability = dockerArchiveCapability(
    capabilities,
    "docker_image_archive_upload_chunk",
    "containers:manage",
    ["id", "image", "archive_id", "offset", "sha256", "data"],
    false,
    false,
  );
  const statusCapability = dockerArchiveCapability(
    capabilities,
    "docker_image_archive_upload_status",
    "containers:manage",
    ["id", "image", "archive_id"],
    false,
    true,
  );
  const importCapability = dockerArchiveCapability(
    capabilities,
    "docker_image_archive_import",
    "containers:manage",
    ["id", "image", "archive_id"],
    true,
    false,
  );

  const [exportTask, setExportTask] = useState<unknown>();
  const [exported, setExported] = useState<DockerArchiveManifest>();
  const [exportBusy, setExportBusy] = useState(false);
  const [exportFinished, setExportFinished] = useState(false);
  const [exportError, setExportError] = useState("");
  const [exportUncertain, setExportUncertain] = useState(false);
  const [exportAck, setExportAck] = useState(false);
  const [downloadBusy, setDownloadBusy] = useState(false);
  const [downloadProgress, setDownloadProgress] = useState(0);

  const [tarFile, setTarFile] = useState<File>();
  const [manifest, setManifest] = useState<DockerArchiveManifest>();
  const [manifestError, setManifestError] = useState("");
  const [archiveID, setArchiveID] = useState("");
  const [uploadStatus, setUploadStatus] = useState<DockerArchiveUploadStatus>();
  const [uploadBusy, setUploadBusy] = useState(false);
  const [uploadProgress, setUploadProgress] = useState(0);
  const [uploadError, setUploadError] = useState("");
  const [beginUncertain, setBeginUncertain] = useState(false);
  const [beginAck, setBeginAck] = useState(false);
  const [importTask, setImportTask] = useState<unknown>();
  const [importResult, setImportResult] =
    useState<ReturnType<typeof dockerArchiveImportResult>>();
  const [importBusy, setImportBusy] = useState(false);
  const [importFinished, setImportFinished] = useState(false);
  const [importError, setImportError] = useState("");
  const [importUncertain, setImportUncertain] = useState(false);
  const [importAck, setImportAck] = useState(false);
  const [confirmedImport, setConfirmedImport] = useState(false);

  const startExport = async () => {
    if (!exportCapability) return;
    setExportBusy(true);
    setExportError("");
    setExported(undefined);
    setExportTask(undefined);
    setExportFinished(false);
    setExportUncertain(false);
    setExportAck(false);
    try {
      const submitted = await action<unknown>(exportCapability.name, {
        id: instanceID,
        image,
      });
      if (!dockerArchiveTaskID(submitted)) {
        setExportUncertain(true);
        setExportError(
          translate(
            "服务未返回任务编号，导出状态无法确认。请先核对任务列表；页面不会自动重试。",
          ),
        );
      } else {
        setExportTask(submitted);
      }
    } catch (error) {
      setExportError(
        error instanceof Error
          ? error.message
          : translate("提交导出任务失败。"),
      );
      if (ambiguous(error)) setExportUncertain(true);
    } finally {
      setExportBusy(false);
    }
  };
  const finishExport = (task: Task) => {
    setExportFinished(true);
    const state = task.status ?? task.state;
    const parsed =
      state === "succeeded" || state === "completed"
        ? dockerArchiveExportResultForImage(task, image)
        : undefined;
    if (parsed) {
      setExported(parsed);
      setExportError("");
    } else if (state === "succeeded" || state === "completed") {
      setExportError(
        translate(
          "任务完成，但返回的归档证明与当前精确镜像不匹配。请检查任务结果。",
        ),
      );
    }
    onExecuted();
  };
  const downloadArchive = async () => {
    if (!readCapability || !exported) return;
    setDownloadBusy(true);
    setDownloadProgress(0);
    setExportError("");
    let writable: Awaited<ReturnType<SaveFile["createWritable"]>> | undefined;
    const parts: BlobPart[] = [];
    try {
      const picker = (
        window as unknown as { showSaveFilePicker?: SaveFilePicker }
      ).showSaveFilePicker;
      if (picker) {
        const handle = await picker({
          suggestedName: `${safeStem(image)}.tar`,
          types: [
            {
              description: "Docker archive",
              accept: { "application/x-tar": [".tar"] },
            },
          ],
        });
        writable = await handle.createWritable();
      } else if (exported.bytes > 512 * 1024 * 1024) {
        throw new Error(
          translate(
            "此浏览器不支持流式保存；为避免占满内存，备用下载方式限制为 512 MiB。请使用支持文件保存权限的浏览器。",
          ),
        );
      }
      const wholeHash = sha256.create();
      let offset = 0;
      while (offset < exported.bytes) {
        const length = Math.min(DOCKER_ARCHIVE_CHUNK, exported.bytes - offset);
        const value = await action<unknown>(readCapability.name, {
          id: instanceID,
          image,
          archive_id: exported.archive_id,
          offset,
          length,
        });
        const chunk = dockerArchiveReadChunk(value, {
          id: exported.archive_id,
          offset,
          bytes: exported.bytes,
          sha256: exported.sha256,
        });
        if (!chunk || chunk.data.length !== length)
          throw new Error(
            translate("服务返回的下载块与偏移、长度或摘要不匹配；下载已停止。"),
          );
        if (hex(sha256(chunk.data)) !== chunk.chunk_sha256)
          throw new Error(translate("下载块 SHA-256 校验失败；下载已停止。"));
        wholeHash.update(chunk.data);
        if (writable) await writable.write(chunk.data);
        else parts.push(new Blob([Uint8Array.from(chunk.data).buffer]));
        offset = chunk.next_offset;
        setDownloadProgress(offset);
      }
      if (hex(wholeHash.digest()) !== exported.sha256)
        throw new Error(
          translate("完整归档 SHA-256 校验失败；未保存下载文件。"),
        );
      if (writable) await writable.close();
      else downloadParts(`${safeStem(image)}.tar`, parts);
      downloadText(
        `${safeStem(image)}.docker-archive.json`,
        JSON.stringify(exported, null, 2),
      );
    } catch (error) {
      if (writable?.abort) await writable.abort().catch(() => undefined);
      setExportError(
        error instanceof Error ? error.message : translate("下载归档失败。"),
      );
    } finally {
      setDownloadBusy(false);
    }
  };

  const acceptManifest = async (file?: File) => {
    setManifest(undefined);
    setManifestError("");
    setUploadStatus(undefined);
    setImportResult(undefined);
    setConfirmedImport(false);
    if (!file) return;
    try {
      const parsed = dockerArchiveManifestText(await file.text());
      if (parsed.image !== image)
        throw new Error(translate("清单绑定的精确镜像与当前目标不一致。"));
      if (parsed.state !== "ready")
        throw new Error(translate("清单不是已完成导出的归档证明。"));
      setManifest(parsed);
      setArchiveID("");
      setBeginUncertain(false);
      setBeginAck(false);
    } catch (error) {
      setManifestError(
        error instanceof Error
          ? error.message
          : translate("无法读取归档清单。"),
      );
    }
  };

  const statusFor = async (
    id: string,
    receipt: DockerArchiveManifest,
  ): Promise<DockerArchiveUploadStatus> => {
    if (!statusCapability)
      throw new Error(translate("当前账户没有读取上传偏移的权限。"));
    const value = await action<unknown>(statusCapability.name, {
      id: instanceID,
      image,
      archive_id: id,
    });
    const parsed = dockerArchiveUploadStatus(value, {
      id,
      image,
      sha256: receipt.sha256,
      bytes: receipt.bytes,
    });
    if (!parsed || parsed.archive.image_id !== receipt.image_id)
      throw new Error(
        translate("上传状态与本地证明清单不匹配；请确认归档编号和源文件。"),
      );
    return parsed;
  };

  const updateUploadStatus = async () => {
    if (!manifest || !archiveID.trim()) return;
    setUploadBusy(true);
    setUploadError("");
    try {
      const current = await statusFor(archiveID.trim(), manifest);
      setUploadStatus(current);
      setUploadProgress(current.next_offset);
    } catch (error) {
      setUploadError(
        error instanceof Error
          ? error.message
          : translate("无法读取上传状态。"),
      );
    } finally {
      setUploadBusy(false);
    }
  };

  const transferChunks = async (
    id: string,
    receipt: DockerArchiveManifest,
    file: File,
    start: number,
  ) => {
    if (!chunkCapability)
      throw new Error(translate("当前账户没有上传归档块的权限。"));
    let offset = start;
    while (offset < file.size) {
      const data = new Uint8Array(
        await file
          .slice(offset, Math.min(offset + DOCKER_ARCHIVE_CHUNK, file.size))
          .arrayBuffer(),
      );
      const hash = hex(sha256(data));
      const nextOffset = offset + data.length;
      const complete = nextOffset === file.size;
      const input = {
        id: instanceID,
        image,
        archive_id: id,
        offset,
        sha256: hash,
        data: base64(data),
      };
      let acknowledged = false;
      for (let attempt = 0; attempt < 2 && !acknowledged; attempt++) {
        try {
          const value = await action<unknown>(chunkCapability.name, input);
          const receiptValue = dockerArchiveChunkReceipt(value, {
            id,
            next_offset: nextOffset,
            complete,
          });
          if (!receiptValue)
            throw new Error(translate("上传块回执与提交的偏移不匹配。"));
          acknowledged = true;
        } catch (error) {
          let observed: DockerArchiveUploadStatus;
          try {
            observed = await statusFor(id, receipt);
          } catch {
            throw error;
          }
          setUploadStatus(observed);
          setUploadProgress(observed.next_offset);
          if (
            attempt === 0 &&
            (observed.next_offset === nextOffset ||
              observed.next_offset === offset)
          ) {
            continue;
          } else {
            throw new Error(
              translate(
                "服务端上传偏移尚未通过同一块的幂等回执核对；为避免跳过或覆盖数据，上传已停止。",
              ),
            );
          }
        }
      }
      if (!acknowledged)
        throw new Error(translate("上传块未得到可核对的提交回执。"));
      offset = nextOffset;
      setUploadProgress(offset);
      setUploadStatus({
        archive: {
          archive_id: id,
          image,
          image_id: receipt.image_id,
          bytes: receipt.bytes,
          sha256: receipt.sha256,
          created_at: receipt.created_at,
          state: complete ? "ready" : "uploading",
        },
        next_offset: offset,
        complete,
      });
    }
    const finalStatus = await statusFor(id, receipt);
    setUploadStatus(finalStatus);
    setUploadProgress(finalStatus.next_offset);
  };

  const prepareUpload = async (resume: boolean) => {
    if (
      !manifest ||
      !tarFile ||
      !statusCapability ||
      !chunkCapability ||
      (!resume && !beginCapability)
    )
      return;
    setUploadBusy(true);
    setUploadError("");
    try {
      if (tarFile.size !== manifest.bytes)
        throw new Error(translate("归档文件大小与服务端证明不匹配。"));
      const fileHash = await hashFile(tarFile, setUploadProgress);
      if (fileHash !== manifest.sha256)
        throw new Error(
          translate("本地归档 SHA-256 与服务端证明不匹配；尚未上传。"),
        );
      if (resume) {
        const id = archiveID.trim();
        if (!id) throw new Error(translate("请输入需要续传的受管归档编号。"));
        const current = await statusFor(id, manifest);
        setUploadStatus(current);
        setUploadProgress(current.next_offset);
        if (!current.complete)
          await transferChunks(id, manifest, tarFile, current.next_offset);
      } else {
        setBeginUncertain(false);
        setBeginAck(false);
        const capability = beginCapability;
        if (!capability)
          throw new Error(
            translate("当前账户没有创建此精确镜像上传归档的权限。"),
          );
        const value = await action<unknown>(capability.name, {
          id: instanceID,
          image,
          proof: manifest.proof,
        });
        const begun = dockerArchiveUploadBegin(value, manifest);
        if (!begun) {
          setBeginUncertain(true);
          throw new Error(
            translate(
              "服务未返回可核对的上传归档编号。请先查看审计记录，页面不会自动重建上传。",
            ),
          );
        }
        setArchiveID(begun.archive.archive_id);
        setUploadStatus(begun);
        await transferChunks(begun.archive.archive_id, manifest, tarFile, 0);
      }
    } catch (error) {
      setUploadError(
        error instanceof Error ? error.message : translate("上传归档失败。"),
      );
      if (!resume && ambiguous(error)) setBeginUncertain(true);
      if (archiveID && manifest && !uploadStatus) void updateUploadStatus();
    } finally {
      setUploadBusy(false);
    }
  };

  const startImport = async () => {
    if (!importCapability || !uploadStatus?.complete || !confirmedImport)
      return;
    setImportBusy(true);
    setImportError("");
    setImportTask(undefined);
    setImportResult(undefined);
    setImportFinished(false);
    setImportUncertain(false);
    setImportAck(false);
    try {
      const submitted = await action<unknown>(importCapability.name, {
        id: instanceID,
        image,
        archive_id: uploadStatus.archive.archive_id,
      });
      if (!dockerArchiveTaskID(submitted)) {
        setImportUncertain(true);
        setImportError(
          translate(
            "服务未返回任务编号，导入状态无法确认。请先核对任务列表；页面不会自动重试。",
          ),
        );
      } else {
        setImportTask(submitted);
      }
    } catch (error) {
      setImportError(
        error instanceof Error
          ? error.message
          : translate("提交镜像加载任务失败。"),
      );
      if (ambiguous(error)) setImportUncertain(true);
    } finally {
      setImportBusy(false);
    }
  };
  const finishImport = (task: Task) => {
    setImportFinished(true);
    const state = task.status ?? task.state;
    const parsed =
      state === "succeeded" || state === "completed"
        ? dockerArchiveImportResult(task)
        : undefined;
    if (parsed?.image === image) {
      setImportResult(parsed);
      setImportError("");
    } else if (state === "succeeded" || state === "completed") {
      setImportError(
        translate("任务完成，但加载回执与当前精确镜像不匹配。请检查任务结果。"),
      );
    }
    onExecuted();
  };

  const canTransfer =
    !!manifest && !!tarFile && tarFile.size === manifest.bytes;

  return (
    <section
      className="stack"
      aria-label={translate("Docker 镜像归档保存与加载")}
    >
      <div className="card-heading">
        <h3>{translate("镜像归档保存与加载")}</h3>
        <span className="status-chip neutral">
          {translate("精确摘要 · 受管证明")}
        </span>
      </div>
      <p className="muted">
        {translate(
          "归档清单中的证明由面板生成并绑定此精确镜像。加载必须使用配套 .tar 和 .json 清单；任意 tar、其他摘要或其他 API 主机的证明都不会被接受。",
        )}
      </p>
      <section className="stack" aria-label={translate("导出镜像归档")}>
        <h4>{translate("导出并保存")}</h4>
        {!exportCapability ? (
          <Unavailable
            title={translate("当前账户没有此精确镜像的归档导出权限")}
          />
        ) : (
          <>
            <p className="muted">
              {translate(
                "服务端任务会生成受管归档；完成后分段读取并逐块、整包核对 SHA-256。",
              )}
            </p>
            {exportUncertain && !exportTask && (
              <div className="notice warning">
                <p>
                  {translate(
                    "导出提交状态无法确认。先检查任务列表，再决定是否开始另一项导出。",
                  )}
                </p>
                <label className="check-label">
                  <input
                    type="checkbox"
                    checked={exportAck}
                    onChange={(event) => setExportAck(event.target.checked)}
                  />
                  <span>
                    {translate("我已检查任务列表，确认可以新建一次导出任务。")}
                  </span>
                </label>
              </div>
            )}
            <Button
              label={
                exportBusy
                  ? translate("正在提交…")
                  : exportUncertain
                    ? translate("确认后重新提交导出")
                    : !!exportTask && !exportFinished
                      ? translate("导出任务处理中")
                      : exported
                        ? translate("新建一次导出任务")
                        : translate("提交镜像导出任务")
              }
              isDisabled={
                exportBusy ||
                (!!exportTask && !exportFinished) ||
                (!!exportUncertain && !exportAck)
              }
              isLoading={exportBusy}
              onClick={() => {
                if (exportUncertain && exportAck) setExportUncertain(false);
                void startExport();
              }}
            />
          </>
        )}
        {exportError && (
          <div className="notice danger" role="alert">
            {exportError}
          </div>
        )}
        {!!exportTask && (
          <TaskResult
            submitted={exportTask}
            capabilities={capabilities}
            onFinished={finishExport}
          />
        )}
        {exported && (
          <div className="stack notice success">
            <div className="resource-summary">
              <div>
                <strong>{translate("已验证归档")}</strong>
                <p>
                  {bytes(exported.bytes)}· SHA-256 {exported.sha256}
                </p>
                <small>
                  {fullDate(exported.created_at)}
                  {translate(" · 受管归档编号 ")}
                  {exported.archive_id}
                </small>
              </div>
              <State value="succeeded" />
            </div>
            <div className="button-row">
              {readCapability ? (
                <Button
                  label={
                    downloadBusy
                      ? translate("正在读取 {0} / {1}", [
                          bytes(downloadProgress),
                          bytes(exported.bytes),
                        ])
                      : translate("校验并下载 .tar 与 .json 清单")
                  }
                  isDisabled={downloadBusy}
                  isLoading={downloadBusy}
                  onClick={() => void downloadArchive()}
                />
              ) : (
                <Unavailable
                  title={translate("当前账户没有读取此受管归档的权限")}
                />
              )}
              <Button
                label={translate("单独下载证明清单 JSON")}
                variant="ghost"
                onClick={() =>
                  downloadText(
                    `${safeStem(image)}.docker-archive.json`,
                    JSON.stringify(exported, null, 2),
                  )
                }
              />
            </div>
            <p className="muted">
              {translate(
                "请把 .tar 与 .json 清单一起交给接收实例；清单包含受管证明，不要编辑其内容。",
              )}
            </p>
          </div>
        )}
      </section>
      <section className="stack" aria-label={translate("上传并加载镜像归档")}>
        <h4>{translate("上传并加载")}</h4>
        <>
          {!beginCapability && (
            <Unavailable
              title={translate("当前账户没有创建此精确镜像上传归档的权限")}
            />
          )}
          {!chunkCapability && (
            <Unavailable title={translate("当前账户没有上传归档块的权限")} />
          )}
          {!statusCapability && (
            <Unavailable
              title={translate(
                "当前账户没有读取上传偏移的权限；安全续传不可用",
              )}
            />
          )}
          <label className="native-field">
            {translate("Docker save 归档文件（.tar）")}
            <input
              type="file"
              accept=".tar,application/x-tar,application/octet-stream"
              onChange={(event) => {
                setTarFile(event.target.files?.[0]);
                setUploadStatus(undefined);
                setUploadError("");
              }}
            />
          </label>
          <label className="native-field">
            {translate("配套受管证明清单（.json）")}
            <input
              type="file"
              accept=".json,application/json"
              onChange={(event) => void acceptManifest(event.target.files?.[0])}
            />
          </label>
          {tarFile && (
            <p className="muted">
              {tarFile.name}· {bytes(tarFile.size)}
            </p>
          )}
          {manifest && (
            <div className="resource-summary">
              <div>
                <strong>{manifest.image}</strong>
                <p>
                  {bytes(manifest.bytes)}· SHA-256 {manifest.sha256}
                </p>
                <small>
                  {translate("源实例 ")}
                  {manifest.archive_id}· Engine ID {manifest.image_id}
                </small>
              </div>
              <State value="verified" />
            </div>
          )}
          {manifestError && (
            <div className="notice danger" role="alert">
              {manifestError}
            </div>
          )}
          {canTransfer && (
            <p className="muted">
              {translate(
                "开始前会先逐块计算本地完整 SHA-256；只有与清单完全一致才会上传。",
              )}
            </p>
          )}
          {beginUncertain && (
            <div className="notice warning">
              <p>
                {translate(
                  "创建上传归档的提交状态无法确认。先查看审计记录，并核对是否已经创建归档。",
                )}
              </p>
              <label className="check-label">
                <input
                  type="checkbox"
                  checked={beginAck}
                  onChange={(event) => setBeginAck(event.target.checked)}
                />
                <span>
                  {translate(
                    "我已核对审计记录，确认可以再创建一个受管上传归档。",
                  )}
                </span>
              </label>
            </div>
          )}
          <div className="button-row">
            <Button
              label={
                uploadBusy
                  ? translate("正在校验或上传 {0} / {1}", [
                      bytes(uploadProgress),
                      bytes(manifest?.bytes ?? 0),
                    ])
                  : translate("校验文件并开始新上传")
              }
              isDisabled={
                !beginCapability ||
                !statusCapability ||
                !chunkCapability ||
                !canTransfer ||
                uploadBusy ||
                !!uploadStatus?.complete ||
                (beginUncertain && !beginAck)
              }
              isLoading={uploadBusy}
              onClick={() => {
                setBeginUncertain(false);
                void prepareUpload(false);
              }}
            />
            <Button
              label={
                uploadBusy
                  ? translate("正在续传…")
                  : translate("核对状态并续传")
              }
              variant="ghost"
              isDisabled={
                !chunkCapability ||
                !statusCapability ||
                !canTransfer ||
                uploadBusy ||
                !archiveID.trim()
              }
              onClick={() => void prepareUpload(true)}
            />
            <Button
              label={translate("只读取服务端偏移")}
              variant="ghost"
              isDisabled={
                !statusCapability ||
                !manifest ||
                !archiveID.trim() ||
                uploadBusy
              }
              onClick={() => void updateUploadStatus()}
            />
          </div>
          <TextInput
            label={translate("受管上传归档编号（续传时使用）")}
            value={archiveID}
            onChange={(value) => {
              setArchiveID(value);
              setUploadStatus(undefined);
            }}
            autoComplete="off"
          />
          {uploadBusy && manifest && (
            <p role="status">
              {translate("本地校验或分段传输：")}
              {bytes(uploadProgress)}/ {bytes(manifest.bytes)}
            </p>
          )}
          {uploadStatus && (
            <div className="resource-summary">
              <div>
                <strong>
                  {uploadStatus.complete
                    ? translate("上传归档已完整接收")
                    : translate("上传可从已提交位置续传")}
                </strong>
                <p>
                  {bytes(uploadStatus.next_offset)}/{" "}
                  {bytes(uploadStatus.archive.bytes)}·{" "}
                  {uploadStatus.archive.archive_id}
                </p>
              </div>
              <State value={uploadStatus.complete ? "ready" : "running"} />
            </div>
          )}
          {uploadError && (
            <div className="notice danger" role="alert">
              {uploadError}
            </div>
          )}
          {uploadStatus?.complete && (
            <>
              {!importCapability ? (
                <Unavailable
                  title={translate("当前账户没有加载此精确镜像的管理权限")}
                />
              ) : (
                <>
                  <label className="check-label">
                    <input
                      type="checkbox"
                      checked={confirmedImport}
                      onChange={(event) =>
                        setConfirmedImport(event.target.checked)
                      }
                    />
                    <span>
                      {translate(
                        "我确认将这个已校验归档加载到当前 Docker 实例的目标镜像摘要。",
                      )}
                    </span>
                  </label>
                  {importUncertain && !importTask && (
                    <div className="notice warning">
                      <p>
                        {translate(
                          "加载提交状态无法确认。先检查任务列表，再决定是否提交新的加载任务。",
                        )}
                      </p>
                      <label className="check-label">
                        <input
                          type="checkbox"
                          checked={importAck}
                          onChange={(event) =>
                            setImportAck(event.target.checked)
                          }
                        />
                        <span>
                          {translate(
                            "我已检查任务列表，确认可以再次提交加载任务。",
                          )}
                        </span>
                      </label>
                    </div>
                  )}
                  <Button
                    label={
                      importBusy
                        ? translate("正在提交加载任务…")
                        : !!importTask && !importFinished
                          ? translate("加载任务处理中")
                          : importUncertain
                            ? translate("确认后重新提交加载")
                            : translate("提交 Docker 加载任务")
                    }
                    isDisabled={
                      !confirmedImport ||
                      importBusy ||
                      (!!importTask && !importFinished) ||
                      (importUncertain && !importAck)
                    }
                    isLoading={importBusy}
                    onClick={() => {
                      if (importUncertain && importAck)
                        setImportUncertain(false);
                      void startImport();
                    }}
                  />
                </>
              )}
            </>
          )}
          {importError && (
            <div className="notice danger" role="alert">
              {importError}
            </div>
          )}
          {!!importTask && (
            <TaskResult
              submitted={importTask}
              capabilities={capabilities}
              onFinished={finishImport}
            />
          )}
          {importResult && (
            <div className="notice success">
              <strong>
                {importResult.already_loaded
                  ? translate("归档已在目标实例中加载")
                  : translate("镜像已从受管归档加载")}
              </strong>
              <p>
                {importResult.image_id}· SHA-256 {importResult.sha256}
              </p>
              {importResult.provenance === "managed_archive" && (
                <p className="muted">
                  {translate(
                    "来源标记为面板受管归档；Docker 本身未报告 RepoDigest。",
                  )}
                </p>
              )}
              {importResult.provenance === "engine_digest" && (
                <p className="muted">
                  {translate("Docker 引擎已确认目标摘要。")}
                </p>
              )}
            </div>
          )}
        </>
      </section>
    </section>
  );
}
