"use client";
import { SectionHeader } from "./section-header";
import { t as translate } from "@/lib/i18n";
import { useEffect, useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { action, bytes, Capability, fullDate } from "@/lib/api";
import {
  ftpCapability,
  ftpChildPath,
  ftpDeleteInput,
  ftpDeletePreview,
  ftpDestinations,
  ftpDirectory,
  ftpParentPath,
  FTPDeletePreview,
  FTPDestination,
  FTPEntry,
} from "@/lib/ftp-maintenance";
import { useResource } from "@/lib/resource-hook";
import { ActionForm } from "./actions";
import { State, Unavailable } from "./resource-ui";

const directoryFields = ["id", "path", "expected_revision"];

export function BackupFTPMaintenance({
  capabilities,
  onExecuted,
  active = true,
}: {
  capabilities: Capability[];
  onExecuted: () => void;
  active?: boolean;
}) {
  const destinationsCapability = ftpCapability(
    capabilities,
    "backup_ftp_destinations_list",
    "backup:read",
    [],
    false,
    true,
  );
  const registerCapability = ftpCapability(
    capabilities,
    "backup_ftp_destination_register",
    "backup:manage",
    [
      "id",
      "host",
      "port",
      "username",
      "password",
      "tls_mode",
      "directory",
      "expected_revision",
    ],
    false,
    false,
  );
  const directoryCapability = ftpCapability(
    capabilities,
    "backup_ftp_directory_list",
    "backup:read",
    directoryFields,
    false,
    true,
  );
  const createDirectoryCapability = ftpCapability(
    capabilities,
    "backup_ftp_directory_create",
    "backup:manage",
    ["id", "path", "name", "expected_revision"],
    true,
    false,
  );
  const deletePreviewCapability = ftpCapability(
    capabilities,
    "backup_ftp_delete_preview",
    "backup:manage",
    ["id", "path", "name", "kind", "expected_revision"],
    false,
    false,
  );
  const deleteCapability = ftpCapability(
    capabilities,
    "backup_ftp_delete",
    "backup:manage",
    ["id", "path", "name", "kind", "expected_revision", "preview_id"],
    true,
    false,
  );
  const transferCapability = ftpCapability(
    capabilities,
    "backup_ftp_transfer",
    "backup:manage",
    ["id", "software_id", "filename", "direction"],
    true,
    false,
  );
  const destinationsResource = useResource<unknown>(
    "backup_ftp_destinations_list",
    {},
    active && !!destinationsCapability,
    30000,
  );
  const destinations = ftpDestinations(destinationsResource.data);
  const [selectedID, setSelectedID] = useState("");
  const selected = destinations?.find(
    (destination) => destination.id === selectedID,
  );
  const [currentPath, setCurrentPath] = useState("");
  const [editingDestination, setEditingDestination] = useState<
    "new" | string
  >();
  const [creatingDirectory, setCreatingDirectory] = useState(false);
  const [selectedEntryName, setSelectedEntryName] = useState("");
  const [preview, setPreview] = useState<FTPDeletePreview>();
  const [acknowledged, setAcknowledged] = useState(false);
  const [previewError, setPreviewError] = useState("");
  const [previewBusy, setPreviewBusy] = useState(false);
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    if (!preview) return;
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [preview]);
  const directoryResource = useResource<unknown>(
    "backup_ftp_directory_list",
    selected
      ? {
          id: selected.id,
          path: currentPath,
          expected_revision: selected.revision,
        }
      : {},
    active && !!selected && !!directoryCapability,
    15000,
  );
  const directory = selected
    ? ftpDirectory(
        directoryResource.data,
        selected.id,
        currentPath,
        selected.revision,
      )
    : undefined;
  const selectedEntry = directory?.items.find(
    (entry) => entry.name === selectedEntryName,
  );
  const refresh = () => {
    void destinationsResource.reload();
    void directoryResource.reload();
    onExecuted();
  };
  const enter = (path: string) => {
    setCurrentPath(path);
    setSelectedEntryName("");
    setPreview(undefined);
    setAcknowledged(false);
    setPreviewError("");
    setCreatingDirectory(false);
  };
  const previewDelete = async (entry: FTPEntry) => {
    if (
      !selected ||
      !deletePreviewCapability ||
      !["file", "directory"].includes(entry.kind)
    )
      return;
    setPreviewBusy(true);
    setPreviewError("");
    setPreview(undefined);
    setAcknowledged(false);
    const expected = {
      path: ftpChildPath(currentPath, entry.name),
      kind: entry.kind as "file" | "directory",
      revision: selected.revision,
    };
    try {
      const result = await action<unknown>(deletePreviewCapability.name, {
        id: selected.id,
        path: currentPath,
        name: entry.name,
        kind: expected.kind,
        expected_revision: selected.revision,
      });
      const parsed = ftpDeletePreview(result, expected);
      if (!parsed)
        throw new Error(translate("服务返回的删除预览与所选目录项不一致。"));
      setPreview(parsed);
      setNow(Date.now());
    } catch (error) {
      setPreviewError(
        error instanceof Error ? error.message : translate("创建预览失败。"),
      );
      void directoryResource.reload();
    } finally {
      setPreviewBusy(false);
    }
  };
  let deleteInput: ReturnType<typeof ftpDeleteInput> | undefined;
  let deleteInputError = "";
  if (selected && selectedEntry && preview) {
    try {
      deleteInput = ftpDeleteInput(
        selected.id,
        currentPath,
        selectedEntry.name,
        preview,
        now,
      );
    } catch (error) {
      deleteInputError =
        error instanceof Error ? error.message : translate("预览已失效。");
    }
  }
  const previewStale =
    !!preview &&
    !!selected &&
    (preview.revision !== selected.revision ||
      !selectedEntry ||
      preview.candidate.kind !== selectedEntry.kind ||
      preview.candidate.size !== selectedEntry.size ||
      preview.candidate.modified !== selectedEntry.modified);

  return (
    <Card className="panel-card">
      <SectionHeader icon="backup" className="card-heading">
        <div>
          <h2>{translate("远程备份 FTP")}</h2>
          <p className="muted">
            {translate("受保护的目的地凭据、目录浏览与预览后维护。")}
          </p>
        </div>
        {destinationsCapability && (
          <Button
            label={translate("刷新目的地")}
            variant="ghost"
            isDisabled={destinationsResource.loading}
            onClick={() => void destinationsResource.reload()}
          />
        )}
      </SectionHeader>
      {destinationsResource.error && (
        <div className="notice danger" role="alert">
          {destinationsResource.error}
        </div>
      )}
      {!destinationsCapability ? (
        <Unavailable title={translate("当前账户没有 FTP 目的地读取权限")} />
      ) : destinations ? (
        <div className="instance-list">
          {destinations.map((destination) => (
            <button
              key={destination.id}
              className="instance-row"
              aria-pressed={selectedID === destination.id}
              onClick={() => {
                setSelectedID(destination.id);
                setCurrentPath("");
                setSelectedEntryName("");
                setPreview(undefined);
                setAcknowledged(false);
                setEditingDestination(undefined);
              }}
            >
              <div>
                <strong>{destination.id}</strong>
                <p>
                  {destination.username}@{destination.host}:{destination.port}·{" "}
                  {destination.directory || "/"}
                </p>
                <small>
                  {translate("版本 ")}
                  {destination.revision}· {destination.tls_mode}
                </small>
              </div>
              <State value="managed" />
            </button>
          ))}
          {!destinations.length && !destinationsResource.loading && (
            <Unavailable title={translate("尚未登记 FTP 目的地")} />
          )}
        </div>
      ) : destinationsResource.loading ? (
        <p role="status">{translate("正在读取 FTP 目的地…")}</p>
      ) : destinationsResource.data !== undefined ? (
        <Unavailable title={translate("FTP 目的地响应无效，请重新读取")} />
      ) : null}
      {registerCapability && (
        <div className="stack">
          <div className="button-row">
            <Button
              label={translate("登记新 FTP 目的地")}
              onClick={() => {
                setSelectedID("");
                setEditingDestination("new");
              }}
            />
            {selected && (
              <Button
                label={translate("更新连接并轮换密码")}
                variant="ghost"
                onClick={() => setEditingDestination(selected.id)}
              />
            )}
          </div>
          {editingDestination && (
            <ActionForm
              key={`ftp-register:${editingDestination}:${selected?.revision ?? 0}`}
              capability={registerCapability}
              preset={
                editingDestination === "new"
                  ? { expected_revision: 0 }
                  : {
                      id: editingDestination,
                      expected_revision: selected?.revision ?? 0,
                    }
              }
              lockedPresetFields={
                editingDestination === "new"
                  ? ["expected_revision"]
                  : ["id", "expected_revision"]
              }
              cacheKeySuffix={String(selected?.revision ?? 0)}
              onBack={() => setEditingDestination(undefined)}
              onExecuted={() => {
                setEditingDestination(undefined);
                refresh();
              }}
            />
          )}
          <p className="muted">
            {translate(
              "密码仅在登记或轮换时提交；服务端验证连接后加密保存，不会回显。",
            )}
          </p>
        </div>
      )}
      {selected && (
        <section className="stack" aria-label={translate("FTP 远程目录维护")}>
          <div className="resource-summary">
            <div>
              <strong>
                {selected.host}:{selected.port}
              </strong>
              <p>
                {selected.directory || "/"}
                {translate(" · 目的地版本 ")}
                {selected.revision}
              </p>
            </div>
          </div>
          {directoryResource.error && (
            <div className="notice danger" role="alert">
              {directoryResource.error}
            </div>
          )}
          {!directoryCapability ? (
            <Unavailable title={translate("当前账户没有远程目录读取权限")} />
          ) : (
            <>
              <SectionHeader icon="backup" className="card-heading">
                <div>
                  <h3>{translate("远程目录")}</h3>
                  <p className="muted">
                    {translate("相对配置目录：/")}
                    {currentPath}
                  </p>
                </div>
                <div className="button-row">
                  {currentPath && (
                    <Button
                      label={translate("上一级目录")}
                      variant="ghost"
                      onClick={() => enter(ftpParentPath(currentPath))}
                    />
                  )}
                  <Button
                    label={translate("刷新目录")}
                    variant="ghost"
                    isDisabled={directoryResource.loading}
                    onClick={() => void directoryResource.reload()}
                  />
                </div>
              </SectionHeader>
              {directoryResource.loading && !directory && (
                <p role="status">{translate("正在读取远程目录…")}</p>
              )}
              {directory && (
                <div className="instance-list">
                  {directory.items.map((entry) => {
                    const canDelete = ["file", "directory"].includes(
                      entry.kind,
                    );
                    const child = entry.kind === "directory";
                    return (
                      <button
                        key={entry.name}
                        className="instance-row"
                        aria-pressed={selectedEntryName === entry.name}
                        onClick={() => {
                          if (child) {
                            enter(ftpChildPath(currentPath, entry.name));
                            return;
                          }
                          setSelectedEntryName(entry.name);
                          setPreview(undefined);
                          setAcknowledged(false);
                        }}
                      >
                        <div>
                          <strong>{entry.name}</strong>
                          <p>
                            {entry.kind}· {bytes(entry.size)}·{" "}
                            {entry.modified
                              ? fullDate(entry.modified)
                              : translate("修改时间未提供")}
                          </p>
                        </div>
                        {entry.kind === "link" ? (
                          <State value="unsupported" />
                        ) : child ? (
                          <State value="active" />
                        ) : null}
                      </button>
                    );
                  })}
                  {!directory.items.length && (
                    <Unavailable title={translate("此远程目录为空")} />
                  )}
                </div>
              )}
              {createDirectoryCapability && directory && (
                <Button
                  label={
                    creatingDirectory
                      ? translate("取消新建目录")
                      : translate("在此处新建空目录")
                  }
                  variant="ghost"
                  onClick={() => setCreatingDirectory(!creatingDirectory)}
                />
              )}
              {creatingDirectory && selected && createDirectoryCapability && (
                <ActionForm
                  key={`ftp-create:${selected.id}:${currentPath}:${selected.revision}`}
                  capability={createDirectoryCapability}
                  preset={{
                    id: selected.id,
                    path: currentPath,
                    expected_revision: selected.revision,
                  }}
                  lockedPresetFields={["id", "path", "expected_revision"]}
                  onBack={() => setCreatingDirectory(false)}
                  onExecuted={refresh}
                />
              )}
              {selectedEntry &&
                !["file", "directory"].includes(selectedEntry.kind) && (
                  <p className="muted">
                    {translate(
                      "只支持维护普通文件和空目录；链接或不支持的项目保持不可操作。",
                    )}
                  </p>
                )}
              {selectedEntry &&
                ["file", "directory"].includes(selectedEntry.kind) && (
                  <div className="stack">
                    <p className="muted">
                      {selectedEntry.kind === "directory"
                        ? translate(
                            "目录删除仅接受空目录，服务不会递归删除内容。",
                          )
                        : translate(
                            "文件删除前会核对预览中的名称、大小与修改时间。",
                          )}
                    </p>
                    {!deletePreviewCapability || !deleteCapability ? (
                      <Unavailable
                        title={translate(
                          "当前账户没有预览并删除此远程项目的权限",
                        )}
                      />
                    ) : (
                      <Button
                        label={
                          previewBusy
                            ? translate("正在读取删除预览…")
                            : translate("预览远程删除")
                        }
                        isDisabled={previewBusy}
                        onClick={() => void previewDelete(selectedEntry)}
                      />
                    )}
                    {previewError && (
                      <div className="notice danger" role="alert">
                        {previewError}
                      </div>
                    )}
                    {preview && deleteInput && (
                      <div className="stack notice warning">
                        <strong>
                          {translate("删除预览 · ")}
                          {preview.candidate.path}
                        </strong>
                        <p>
                          {translate("类型：")}
                          {preview.candidate.kind}
                          {translate(" · 大小：")}
                          {bytes(preview.candidate.size)}
                          {translate(" · 修改时间：")}
                          {preview.candidate.modified
                            ? fullDate(preview.candidate.modified)
                            : translate("未提供")}
                        </p>
                        <p>
                          {translate("预览有效至 ")}
                          {fullDate(preview.expires_at)}
                          {translate("，且只能提交一次。")}
                        </p>
                        <p>
                          {translate(
                            "FTP 服务端不提供按文件标识的原子删除；外部写入者可能在最后一次核对与删除之间替换远程项目。",
                          )}
                        </p>
                        {previewStale && (
                          <p role="alert">
                            {translate(
                              "目的地配置版本已变化，请重新读取并预览。",
                            )}
                          </p>
                        )}
                        {deleteInputError && (
                          <p role="alert">{deleteInputError}</p>
                        )}
                        <label className="check-label">
                          <input
                            type="checkbox"
                            checked={acknowledged}
                            onChange={(event) =>
                              setAcknowledged(event.target.checked)
                            }
                          />
                          <span>
                            {translate(
                              "我已核对目标路径，并确认提交这个一次性删除预览。",
                            )}
                          </span>
                        </label>
                        {acknowledged && !previewStale && deleteCapability && (
                          <ActionForm
                            key={`ftp-delete:${selected.id}:${preview.preview_id}`}
                            capability={deleteCapability}
                            preset={deleteInput}
                            lockedPresetFields={Object.keys(deleteInput)}
                            onBack={() => setAcknowledged(false)}
                            onExecuted={refresh}
                          />
                        )}
                      </div>
                    )}
                  </div>
                )}
            </>
          )}
        </section>
      )}
      {transferCapability && (
        <Unavailable
          title={translate("备份文件传输等待安全文件清单")}
          detail={translate(
            "服务提供传输任务，但目前没有列出实例本地已拥有备份文件的读取能力。为避免猜测文件名，此页暂不提交上传或下载任务。",
          )}
        />
      )}
    </Card>
  );
}
