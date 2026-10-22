"use client";
import { t as translate } from "@/lib/i18n";
import { useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { TextArea } from "@astryxdesign/core/TextArea";
import { TextInput } from "@astryxdesign/core/TextInput";
import { action, APIError, Capability, Task } from "@/lib/api";
import {
  dockerExecCapability,
  dockerExecInput,
  dockerExecOutput,
  dockerExecRecovery,
  dockerExecStatus,
  dockerExecTaskID,
  DockerExecInput,
  DockerExecOutput,
  DockerExecRecovery,
  DockerExecStatus,
} from "@/lib/docker-exec";
import { TaskResult } from "./task-result";
import { State, Unavailable } from "./resource-ui";

type Pending = { id: string; name: string; execution_id: string };

export function DockerExecManagement({
  instanceID,
  containerName,
  running,
  capabilities,
  onExecuted,
}: {
  instanceID: string;
  containerName: string;
  running: boolean;
  capabilities: Capability[];
  onExecuted: () => void;
}) {
  const capability = dockerExecCapability(capabilities);
  const statusCapability = capabilities.find((item) => {
    const properties = item.input_schema?.properties as
      | Record<string, unknown>
      | undefined;
    return (
      item.name === "docker_container_exec_status" &&
      item.scope === "containers:read" &&
      item.task !== true &&
      item.read_only === true &&
      !!properties &&
      ["id", "name", "execution_id"].every((key) => key in properties)
    );
  });
  const [argv, setArgv] = useState([""]);
  const [envText, setEnvText] = useState("{}");
  const [workingDir, setWorkingDir] = useState("");
  const [stdin, setStdin] = useState("");
  const [timeout, setTimeout] = useState(30);
  const [outputLimit, setOutputLimit] = useState(16384);
  const [pendingInput, setPendingInput] = useState<DockerExecInput>();
  const [busy, setBusy] = useState(false);
  const [formError, setFormError] = useState("");
  const [submitted, setSubmitted] = useState<unknown>();
  const [finalTask, setFinalTask] = useState<Task>();
  const [output, setOutput] = useState<DockerExecOutput>();
  const [recovery, setRecovery] = useState<DockerExecRecovery>();
  const [status, setStatus] = useState<DockerExecStatus>();
  const [statusError, setStatusError] = useState("");
  const [statusBusy, setStatusBusy] = useState(false);
  const [uncertain, setUncertain] = useState(false);
  const locked = busy || !!submitted || uncertain || !!pendingInput;
  const command = argv[0]?.trim() ?? "";
  const buildInput = () => {
    let env: Record<string, string> = {};
    try {
      const parsed = JSON.parse(envText || "{}") as unknown;
      if (
        !parsed ||
        typeof parsed !== "object" ||
        Array.isArray(parsed) ||
        Object.values(parsed).some((value) => typeof value !== "string")
      )
        throw new Error(
          translate('环境变量需要JSON字符串对象，例如 {"MODE":"safe"}。'),
        );
      env = parsed as Record<string, string>;
    } catch (error) {
      throw new Error(
        error instanceof SyntaxError
          ? translate("环境变量JSON格式无效。")
          : error instanceof Error
            ? error.message
            : translate("环境变量JSON格式无效。"),
      );
    }
    return dockerExecInput(
      {
        argv,
        env,
        working_dir: workingDir,
        stdin,
        timeout_seconds: timeout,
        output_limit_bytes: outputLimit,
      },
      instanceID,
      containerName,
    );
  };
  const review = () => {
    if (!capability || !running) return;
    setFormError("");
    try {
      setPendingInput(buildInput());
    } catch (error) {
      setFormError(
        error instanceof Error ? error.message : translate("命令参数无效。"),
      );
    }
  };
  const submit = async () => {
    if (!capability || !pendingInput) return;
    setBusy(true);
    setFormError("");
    setStatusError("");
    try {
      const task = await action<unknown>(capability.name, pendingInput);
      if (!dockerExecTaskID(task)) {
        setUncertain(true);
        setFormError(
          translate(
            "服务未返回任务编号，提交状态无法确认。请先检查任务列表；页面不会自动重试此命令。",
          ),
        );
      } else {
        setSubmitted(task);
        setPendingInput(undefined);
      }
    } catch (error) {
      const uncertainResult =
        error instanceof APIError &&
        (error.status === 0 ||
          error.status >= 500 ||
          error.code === "invalid_response");
      if (uncertainResult) {
        setUncertain(true);
        setPendingInput(undefined);
        setFormError(
          translate(
            "连接或服务响应中断，命令可能已进入后台。请先检查任务列表；页面不会自动重试此命令。",
          ),
        );
      } else {
        setFormError(
          error instanceof Error ? error.message : translate("命令提交失败。"),
        );
      }
    } finally {
      setBusy(false);
    }
  };
  const finished = (task: Task) => {
    setFinalTask(task);
    const parsed = dockerExecOutput(task.result);
    setOutput(parsed);
    const pending = dockerExecRecovery(task.result);
    setRecovery(pending);
    setStatus(undefined);
    setStatusError("");
    onExecuted();
  };
  const recheck = async () => {
    if (!statusCapability || !recovery) return;
    setStatusBusy(true);
    setStatusError("");
    try {
      const value = await action<unknown>(statusCapability.name, {
        id: instanceID,
        name: containerName,
        execution_id: recovery.execution_id,
      });
      const parsed = dockerExecStatus(value);
      if (!parsed || parsed.execution_id !== recovery.execution_id)
        throw new Error(translate("执行状态与本次记录的引擎执行编号不一致。"));
      setStatus(parsed);
    } catch (error) {
      setStatusError(
        error instanceof Error
          ? error.message
          : translate("无法读取执行状态。"),
      );
    } finally {
      setStatusBusy(false);
    }
  };
  const startNew = () => {
    setSubmitted(undefined);
    setFinalTask(undefined);
    setOutput(undefined);
    setRecovery(undefined);
    setStatus(undefined);
    setStatusError("");
    setFormError("");
    setArgv([""]);
    setEnvText("{}");
    setWorkingDir("");
    setStdin("");
    setTimeout(30);
    setOutputLimit(16384);
  };

  return (
    <section className="stack" aria-label={translate("Docker 一次性容器命令")}>
      <div className="card-heading">
        <h3>{translate("一次性容器命令")}</h3>
        <span className="status-chip neutral">
          {translate("argv · 非交互执行")}
        </span>
      </div>
      <p className="muted">
        {translate(
          "每一行作为独立参数直接传入，不附加 shell 或交互式 TTY；若 argv[0] 指向 shell，shell 本身仍会按传入参数运行。参数会保留在任务记录中，不要把密码放入 argv。连接中断后不会自动重试。",
        )}
      </p>
      {!capability ? (
        <Unavailable title={translate("当前账户没有受管容器命令执行权限")} />
      ) : !running ? (
        <Unavailable
          title={translate("容器当前未处于运行状态")}
          detail={translate("仅运行中的受管容器支持一次性命令执行。")}
        />
      ) : (
        <>
          <div className="stack">
            {argv.map((value, index) => (
              <div className="array-field" key={index}>
                <TextInput
                  label={
                    index === 0
                      ? translate("可执行程序 · argv[0]")
                      : translate("参数 {0}", [index + 1])
                  }
                  value={value}
                  onChange={(next) =>
                    setArgv(argv.map((item, i) => (i === index ? next : item)))
                  }
                  autoComplete="off"
                  isDisabled={locked}
                />
                {index > 0 && (
                  <Button
                    label={translate("移除参数 {0}", [index + 1])}
                    variant="ghost"
                    isDisabled={locked}
                    onClick={() => setArgv(argv.filter((_, i) => i !== index))}
                  />
                )}
              </div>
            ))}
            <Button
              label={translate("添加 argv 参数")}
              variant="ghost"
              isDisabled={locked || argv.length >= 16}
              onClick={() => setArgv([...argv, ""])}
            />
            <TextArea
              label={translate("环境变量 JSON")}
              description={translate(
                "可选；最多 16 项。密钥字段通过受保护通道提交，任务记录只保存服务端引用。",
              )}
              value={envText}
              onChange={setEnvText}
              rows={4}
              autoComplete="off"
              isDisabled={locked}
            />
            <TextInput
              label={translate("工作目录")}
              description={translate("可选；容器内的规范绝对路径。")}
              value={workingDir}
              onChange={setWorkingDir}
              autoComplete="off"
              isDisabled={locked}
            />
            <TextArea
              label={translate("标准输入")}
              description={translate(
                "可选；最多 16 KiB。适合传入不应出现在命令参数中的内容。",
              )}
              value={stdin}
              onChange={setStdin}
              rows={4}
              autoComplete="off"
              isDisabled={locked}
            />
            <div className="detail-grid">
              <TextInput
                label={translate("超时秒数")}
                type="text"
                value={String(timeout)}
                onChange={(value) => setTimeout(Number(value))}
                isDisabled={locked}
              />
              <TextInput
                label={translate("输出上限（字节）")}
                type="text"
                value={String(outputLimit)}
                onChange={(value) => setOutputLimit(Number(value))}
                isDisabled={locked}
              />
            </div>
          </div>
          {formError && (
            <div className="notice danger" role="alert">
              {formError}
            </div>
          )}
          {pendingInput ? (
            <div className="notice warning">
              <strong>{translate("确认提交一次性命令")}</strong>
              <p>
                {translate("目标：")}
                {containerName}· argv[0]：{command}
              </p>
              <p>
                {translate(
                  "执行后若连接中断，先检查任务列表再决定是否发起一次新的命令。",
                )}
              </p>
              <div className="button-row">
                <Button
                  label={
                    busy ? translate("正在提交…") : translate("确认并提交命令")
                  }
                  isDisabled={busy}
                  isLoading={busy}
                  onClick={() => void submit()}
                />
                <Button
                  label={translate("返回修改参数")}
                  variant="ghost"
                  isDisabled={busy}
                  onClick={() => setPendingInput(undefined)}
                />
              </div>
            </div>
          ) : uncertain ? (
            <div className="notice warning">
              <p>
                {translate(
                  "提交状态尚未确认，请先在任务列表核对是否已有本次命令。",
                )}
              </p>
              <label className="check-label">
                <input
                  type="checkbox"
                  onChange={(event) => {
                    if (event.target.checked) {
                      setUncertain(false);
                      setFormError("");
                    }
                  }}
                />
                <span>
                  {translate("我已核对任务列表，确认可以发起一次新的执行。")}
                </span>
              </label>
            </div>
          ) : submitted ? (
            <>
              <TaskResult
                submitted={submitted}
                capabilities={capabilities}
                onFinished={finished}
              />
              {finalTask && (
                <>
                  {output && (
                    <div className="stack">
                      <div className="resource-summary">
                        <strong>
                          {translate("退出码 ")}
                          {output.exit_code}
                        </strong>
                        {output.truncated && <State value="truncated" />}
                      </div>
                      {output.stdout && (
                        <details open>
                          <summary>{translate("标准输出")}</summary>
                          <pre className="json-result">{output.stdout}</pre>
                        </details>
                      )}
                      {output.stderr && (
                        <details open>
                          <summary>{translate("标准错误")}</summary>
                          <pre className="json-result">{output.stderr}</pre>
                        </details>
                      )}
                      {!output.stdout && !output.stderr && (
                        <p className="muted">
                          {translate("命令没有返回文本输出。")}
                        </p>
                      )}
                    </div>
                  )}
                  {recovery && (
                    <div className="notice warning">
                      <strong>{translate("执行状态尚未确认")}</strong>
                      <p>
                        {translate("任务结束时容器引擎仍报告 ")}
                        {recovery.execution_state}
                        {translate("。输出不代表命令已经完成。")}
                      </p>
                      {!statusCapability ? (
                        <p>
                          {translate(
                            "当前账户没有读取受管执行状态的权限；请检查任务与容器状态。",
                          )}
                        </p>
                      ) : (
                        <Button
                          label={
                            statusBusy
                              ? translate("正在核对…")
                              : translate("核对这次执行状态")
                          }
                          isDisabled={statusBusy}
                          isLoading={statusBusy}
                          onClick={() => void recheck()}
                        />
                      )}
                      {statusError && <p role="alert">{statusError}</p>}
                      {status && (
                        <p>
                          {translate("引擎状态：")}
                          {status.execution_state}
                          {status.exit_code !== undefined
                            ? translate(" · 实际退出码 {0}", [status.exit_code])
                            : ""}
                        </p>
                      )}
                    </div>
                  )}
                  <Button
                    label={translate("准备一次新的执行")}
                    variant="ghost"
                    onClick={startNew}
                  />
                </>
              )}
            </>
          ) : (
            <Button
              label={translate("检查参数并继续")}
              variant="primary"
              onClick={review}
            />
          )}
        </>
      )}
    </section>
  );
}
