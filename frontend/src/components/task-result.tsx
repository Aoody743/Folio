"use client";
import { useEffect, useRef, useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import {
  action,
  Capability,
  fullDate,
  intentKey,
  source,
  Task,
} from "@/lib/api";
import {
  claimTaskObservationNotification,
  isTaskObservationDenied,
  TaskObservationNotificationState,
} from "@/lib/task-observation";
import { State } from "./resource-ui";
const finished = (state?: string) =>
  [
    "succeeded",
    "failed",
    "canceled",
    "cancelled",
    "interrupted",
    "completed",
  ].includes(state ?? "");
export function TaskResult({
  submitted,
  capabilities,
  onFinished,
  onObservationUnavailable,
}: {
  submitted: unknown;
  capabilities?: Capability[];
  onFinished?: (task: Task) => void;
  onObservationUnavailable?: (error: Error) => void;
}) {
  const original = submitted as Partial<Task> & { task_id?: string };
  const id = original?.id ?? original?.task_id;
  const [task, setTask] = useState<Partial<Task>>(original ?? {});
  const [error, setError] = useState("");
  const [logs, setLogs] = useState<unknown>();
  const [observationUnavailable, setObservationUnavailable] = useState(false);
  const [observationRetry, setObservationRetry] = useState(0);
  const callback = useRef(onFinished);
  callback.current = onFinished;
  const unavailableCallback = useRef(onObservationUnavailable);
  unavailableCallback.current = onObservationUnavailable;
  const notifications = useRef<TaskObservationNotificationState>({ unavailable: false, finished: false });
  const canReadTask = capabilities?.some((capability) => capability.name === "tasks_get");
  useEffect(() => {
    notifications.current = { unavailable: false, finished: false };
    setTask(original ?? {});
    setLogs(undefined);
    setObservationUnavailable(false);
    setError("");
  }, [id]);
  useEffect(() => {
    if (!id) return;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const unavailable = (reason: Error) => {
      if (stopped) return;
      setObservationUnavailable(true);
      setError("");
      const notification = claimTaskObservationNotification(notifications.current, "unavailable");
      notifications.current = notification.state;
      if (notification.notify) {
        unavailableCallback.current?.(reason);
      }
    };
    if (canReadTask === false) {
      unavailable(new Error("tasks_get is not available to this identity"));
      return () => {
        stopped = true;
      };
    }
    setObservationUnavailable(false);
    const read = async () => {
      try {
        const next = await action<Task>("tasks_get", { id });
        if (stopped) return;
        setTask(next);
        setError("");
        setObservationUnavailable(false);
        if (finished(next.status ?? next.state)) {
          const notification = claimTaskObservationNotification(notifications.current, "finished");
          notifications.current = notification.state;
          if (notification.notify) {
            callback.current?.(next);
          }
          return;
        }
      } catch (e) {
        if (stopped) return;
        if (isTaskObservationDenied(e)) {
          unavailable(e instanceof Error ? e : new Error(String(e)));
          return;
        }
        setError((e as Error).message);
      }
      timer = setTimeout(read, 1000);
    };
    void read();
    return () => {
      stopped = true;
      clearTimeout(timer);
    };
  }, [id, observationRetry, canReadTask]);
  if (!id)
    return (
      <pre className="json-result">{JSON.stringify(submitted, null, 2)}</pre>
    );
  return (
    <section className="task-detail" aria-label="任务详情">
      <div className="resource-summary">
        <code>{id}</code>
        <State value={task.status ?? task.state} />
      </div>
      {task.actor && (
        <p className="muted">
          {source(task.actor)} · {fullDate(task.updated_at ?? task.created_at)}
        </p>
      )}
      {!observationUnavailable && (error || task.error) && (
        <div className="notice danger" role="alert">
          {error || task.error}
        </div>
      )}
      {observationUnavailable && (
        <div className="notice warning" role="alert">
          任务已提交，但当前身份无法读取任务状态；执行结果未知。不会自动重试。
          <div className="button-row">
            <Button
              label="手动重新读取任务状态"
              variant="ghost"
              onClick={() => {
                setObservationUnavailable(false);
                setObservationRetry((value) => value + 1);
              }}
            />
          </div>
        </div>
      )}
      <div className="button-row">
        {!finished(task.status ?? task.state) &&
          (!capabilities ||
            capabilities.some((c) => c.name === "tasks_cancel")) && (
            <Button
              label="请求取消此任务"
              onClick={() =>
                void action<Task>(
                  "tasks_cancel",
                  { id },
                  { idempotencyKey: intentKey() },
                )
                  .then(setTask)
                  .catch((e) => setError((e as Error).message))
              }
            />
          )}
        <Button
          label="读取任务日志"
          variant="ghost"
          onClick={() =>
            void action("tasks_logs", { id })
              .then(setLogs)
              .catch((e) => setError((e as Error).message))
          }
        />
      </div>
      {task.result !== undefined && (
        <pre className="json-result">
          {JSON.stringify(task.result, null, 2)}
        </pre>
      )}
      {logs !== undefined && (
        <details open>
          <summary>任务日志</summary>
          <pre className="json-result">
            {typeof logs === "string" ? logs : JSON.stringify(logs, null, 2)}
          </pre>
        </details>
      )}
    </section>
  );
}
