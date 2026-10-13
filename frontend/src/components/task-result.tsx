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
}: {
  submitted: unknown;
  capabilities?: Capability[];
  onFinished?: (task: Task) => void;
}) {
  const original = submitted as Partial<Task> & { task_id?: string };
  const id = original?.id ?? original?.task_id;
  const [task, setTask] = useState<Partial<Task>>(original ?? {});
  const [error, setError] = useState("");
  const [logs, setLogs] = useState<unknown>();
  const callback = useRef(onFinished);
  callback.current = onFinished;
  const notified = useRef(false);
  useEffect(() => {
    if (!id) return;
    notified.current = false;
    setTask(original ?? {});
    setLogs(undefined);
    let stopped = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const read = async () => {
      try {
        const next = await action<Task>("tasks_get", { id });
        if (stopped) return;
        setTask(next);
        setError("");
        if (finished(next.status ?? next.state)) {
          if (!notified.current) {
            notified.current = true;
            callback.current?.(next);
          }
          return;
        }
      } catch (e) {
        if (stopped) return;
        setError((e as Error).message);
      }
      timer = setTimeout(read, 1000);
    };
    void read();
    return () => {
      stopped = true;
      clearTimeout(timer);
    };
  }, [id]);
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
      {(error || task.error) && (
        <div className="notice danger" role="alert">
          {error || task.error}
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
