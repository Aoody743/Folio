"use client";
import { t as translate } from "@/lib/i18n";
import { useState } from "react";
import dynamic from "next/dynamic";
const DockerImageArchiveManagement = dynamic(
  () =>
    import("./docker-image-archive").then(
      (m) => m.DockerImageArchiveManagement,
    ),
  { ssr: false },
);
import { Button } from "@astryxdesign/core/Button";
import { TextInput } from "@astryxdesign/core/TextInput";
import { action, apiErrorMessage, Capability } from "@/lib/api";
import {
  dockerCapability,
  dockerImages,
  dockerPullInput,
  registryCredential,
  registryCredentials,
  RegistryCredential,
} from "@/lib/docker-management";
import { useResource } from "@/lib/resource-hook";
import { TaskResult } from "./task-result";
import { Unavailable } from "./resource-ui";
export function DockerManagement({
  instanceID,
  capabilities,
  onExecuted,
  active = true,
}: {
  instanceID: string;
  capabilities: Capability[];
  onExecuted: () => void;
  active?: boolean;
}) {
  const cap = (suffix: string, fields: string[], task?: boolean) =>
    dockerCapability(capabilities, `docker_${suffix}`, fields, task);
  const images = useResource<unknown>(
    "docker_images_list",
    { id: instanceID },
    active && !!cap("images_list", ["id"]),
  );
  const credentials = useResource<unknown>(
    "docker_registry_credential_list",
    {},
    active && !!cap("registry_credential_list", []),
  );
  const [selected, setSelected] = useState("");
  const [credential, setCredential] = useState<RegistryCredential>();
  const [credentialID, setCredentialID] = useState("");
  const [name, setName] = useState("");
  const [registry, setRegistry] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [image, setImage] = useState("");
  const [confirm, setConfirm] = useState(false);
  const [isTask, setIsTask] = useState(false);
  const [archiveOpen, setArchiveOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState<unknown>();
  const rows = dockerImages(images.data);
  let entries: RegistryCredential[] = [];
  let invalid = "";
  try {
    entries = registryCredentials(credentials.data);
  } catch (e) {
    invalid = apiErrorMessage(e);
  }
  const refresh = () => {
    void images.reload();
    void credentials.reload();
    onExecuted();
  };
  const run = async (
    name: string,
    input: unknown,
    after?: (v: unknown) => void,
  ) => {
    setBusy(true);
    setError("");
    setResult(undefined);
    setIsTask(name === "docker_image_pull" || name === "docker_image_remove");
    try {
      const v = await action<unknown>(name, input);
      after?.(v);
      setResult(
        name.startsWith("docker_registry_credential_") &&
          !name.endsWith("_delete")
          ? registryCredential(v)
          : v,
      );
      refresh();
    } catch (e) {
      setError(apiErrorMessage(e));
      setCredential(undefined);
      refresh();
    } finally {
      setBusy(false);
    }
  };
  const choose = (v: RegistryCredential) => {
    setCredential(v);
    setCredentialID(v.id);
    setName(v.name);
    setRegistry(v.registry);
    setUsername(v.username);
    setPassword("");
  };
  const save = (rotate: boolean) => {
    const secret = password;
    setPassword("");
    void run(
      `docker_registry_credential_${rotate ? "rotate" : "create"}`,
      {
        name,
        registry,
        username,
        password: secret,
        ...(rotate && credential
          ? { id: credential.id, expected_revision: credential.revision }
          : {}),
      },
      (v) => choose(registryCredential(v)),
    );
  };
  return (
    <section className="stack" aria-label={translate("Docker镜像与私仓凭据")}>
      <h3>{translate("精确摘要镜像")}</h3>
      {(error || images.error || credentials.error || invalid) && (
        <div className="notice danger" role="alert">
          {error || images.error || credentials.error || invalid}
        </div>
      )}
      {!cap("images_list", ["id"]) && (
        <Unavailable title={translate("当前没有镜像读取权限或接口不兼容")} />
      )}
      <div className="instance-list">
        {rows.map((v) => (
          <button
            key={v.image}
            className="instance-row"
            aria-pressed={selected === v.image}
            disabled={busy}
            onClick={() => {
              setSelected(v.image);
              setConfirm(false);
              setResult(undefined);
              setArchiveOpen(false);
            }}
          >
            <div>
              <strong>{v.image}</strong>
              <p>
                {v.id}· {v.size_bytes}B
                {v.restored_from_archive ? translate(" · 由受管归档恢复") : ""}
              </p>
            </div>
          </button>
        ))}
      </div>
      {selected && rows.some((v) => v.image === selected) && (
        <div className="stack">
          <code>{selected}</code>
          <div className="button-row">
            {cap("image_inspect", ["id", "image"]) && (
              <Button
                label={translate("读取镜像详情")}
                isDisabled={busy}
                onClick={() =>
                  void run("docker_image_inspect", {
                    id: instanceID,
                    image: selected,
                  })
                }
              />
            )}
            {cap("image_remove", ["id", "image"], true) && (
              <Button
                label={translate("删除此摘要镜像")}
                isDisabled={busy}
                onClick={() => setConfirm(true)}
              />
            )}
            <Button
              label={
                archiveOpen
                  ? translate("收起归档管理")
                  : translate("保存或导入镜像归档")
              }
              variant="ghost"
              onClick={() => setArchiveOpen(!archiveOpen)}
            />
          </div>
          {confirm && (
            <div className="notice warning">
              <p>
                {translate("确认删除 ")}
                {selected}
                {translate("？服务端仅执行非强制删除，使用中的镜像会被拒绝。")}
              </p>
              <Button
                label={translate("确认删除镜像")}
                isDisabled={busy}
                onClick={() => {
                  setConfirm(false);
                  void run("docker_image_remove", {
                    id: instanceID,
                    image: selected,
                  });
                }}
              />
              <Button
                label={translate("取消")}
                onClick={() => setConfirm(false)}
              />
            </div>
          )}
        </div>
      )}
      {archiveOpen && selected && rows.some((v) => v.image === selected) && (
        <DockerImageArchiveManagement
          key={`${instanceID}:${selected}`}
          instanceID={instanceID}
          image={selected}
          capabilities={capabilities}
          onExecuted={refresh}
        />
      )}
      <h3>{translate("私仓凭据")}</h3>
      <p className="muted">
        {translate(
          "密码只用于加密保存，不会回显。拉取任务只提交凭据编号和版本。",
        )}
      </p>
      <div className="instance-list">
        {entries.map((v) => (
          <button
            key={v.id}
            className="instance-row"
            disabled={busy}
            onClick={() => {
              setCredential(undefined);
              setPassword("");
              setCredentialID(v.id);
              if (cap("registry_credential_get", ["id"]))
                void run(
                  "docker_registry_credential_get",
                  { id: v.id },
                  (value) => choose(registryCredential(value)),
                );
            }}
          >
            <div>
              <strong>
                {v.name}· {v.registry}
              </strong>
              <p>
                {v.username}
                {translate(" · 版本 ")}
                {v.revision}
              </p>
            </div>
          </button>
        ))}
      </div>
      <Button
        label={translate("新建凭据")}
        isDisabled={busy}
        onClick={() => {
          setCredential(undefined);
          setCredentialID("");
          setName("");
          setRegistry("");
          setUsername("");
          setPassword("");
        }}
      />
      <TextInput
        label={translate("凭据编号（读取已有凭据）")}
        value={credentialID}
        onChange={(v) => {
          setCredentialID(v);
          setCredential(undefined);
          setPassword("");
        }}
        autoComplete="off"
      />
      {cap("registry_credential_get", ["id"]) && (
        <Button
          label={translate("读取凭据元数据")}
          isDisabled={busy || !credentialID}
          onClick={() =>
            void run(
              "docker_registry_credential_get",
              { id: credentialID },
              (v) => choose(registryCredential(v)),
            )
          }
        />
      )}
      {credential && (
        <p>
          {translate("已读取 ")}
          {credential.id}
          {translate(" · 版本 ")}
          {credential.revision}·{" "}
          {credential.credential_configured
            ? translate("密码已配置")
            : translate("密码未配置")}
        </p>
      )}
      <TextInput
        label={translate("凭据名称")}
        value={name}
        onChange={setName}
        autoComplete="off"
      />
      <TextInput
        label={translate("私仓地址（host 或 host:port）")}
        value={registry}
        onChange={setRegistry}
        autoComplete="off"
      />
      <TextInput
        label={translate("用户名")}
        value={username}
        onChange={setUsername}
        autoComplete="off"
      />
      <TextInput
        label={translate("新密码")}
        type="password"
        value={password}
        onChange={setPassword}
        autoComplete="new-password"
      />
      <div className="button-row">
        {!credential &&
          cap(
            "registry_credential_create",
            ["name", "registry", "username", "password"],
            false,
          ) && (
            <Button
              label={translate("加密保存新凭据")}
              isDisabled={busy || !name || !registry || !username || !password}
              onClick={() => save(false)}
            />
          )}
        {credential &&
          cap(
            "registry_credential_rotate",
            [
              "id",
              "expected_revision",
              "name",
              "registry",
              "username",
              "password",
            ],
            false,
          ) && (
            <Button
              label={translate("按读取版本轮换凭据")}
              isDisabled={busy || !name || !registry || !username || !password}
              onClick={() => save(true)}
            />
          )}
        {credential &&
          cap(
            "registry_credential_delete",
            ["id", "expected_revision"],
            false,
          ) && (
            <Button
              label={translate("删除凭据")}
              isDisabled={busy}
              onClick={() => {
                if (
                  window.confirm(
                    translate("确认删除凭据 {0}（版本 {1}）？", [
                      credential.name,
                      credential.revision,
                    ]),
                  )
                ) {
                  setPassword("");
                  void run(
                    "docker_registry_credential_delete",
                    {
                      id: credential.id,
                      expected_revision: credential.revision,
                    },
                    () => setCredential(undefined),
                  );
                }
              }}
            />
          )}
      </div>
      <h3>{translate("拉取精确摘要镜像")}</h3>
      <TextInput
        label={translate("镜像 repo@sha256:…")}
        value={image}
        onChange={setImage}
        autoComplete="off"
      />
      <p>
        {credential
          ? translate("使用已读取凭据 {0} · 版本 {1}", [
              credential.name,
              credential.revision,
            ])
          : translate("匿名拉取")}
      </p>
      {credential && (
        <Button
          label={translate("改为匿名拉取")}
          onClick={() => {
            setCredential(undefined);
            setPassword("");
          }}
        />
      )}
      {cap(
        "image_pull",
        ["id", "image", "credential_id", "credential_revision"],
        true,
      ) && (
        <Button
          label={translate("提交拉取任务")}
          isDisabled={busy || !image}
          onClick={() => {
            try {
              const input = dockerPullInput(instanceID, image, credential);
              void run("docker_image_pull", input);
            } catch (e) {
              setError(apiErrorMessage(e));
            }
          }}
        />
      )}
      {result !== undefined &&
        (isTask ? (
          <TaskResult
            key={JSON.stringify(result)}
            submitted={result}
            capabilities={capabilities}
            onFinished={refresh}
          />
        ) : (
          <pre className="json-result">{JSON.stringify(result, null, 2)}</pre>
        ))}
    </section>
  );
}
