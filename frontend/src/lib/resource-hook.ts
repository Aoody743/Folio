"use client";
import { t as translate } from "./i18n";
import { useCallback, useEffect, useRef, useState } from "react";
import { action, APIError } from "./api";
import type { PageState } from "./navigation";
export function useResource<T>(
  name: string,
  input: unknown,
  enabled: boolean,
  poll = 15000,
) {
  const [data, setData] = useState<T>();
  const [dataKey, setDataKey] = useState<string>();
  const [error, setError] = useState("");
  const [failureState, setFailureState] = useState<PageState>("error");
  const [loading, setLoading] = useState(false);
  const [loadedAt, setLoadedAt] = useState<string>();
  const generation = useRef(0);
  const serialized = JSON.stringify(input);
  const queryKey = name + ":" + serialized;
  const reload = useCallback(async () => {
    if (!enabled) return;
    const request = ++generation.current;
    setLoading(true);
    try {
      const value = await action<T>(name, JSON.parse(serialized));
      if (request !== generation.current) return;
      setData(value);
      setDataKey(name + ":" + serialized);
      setLoadedAt(new Date().toISOString());
      setError("");
    } catch (e) {
      if (request === generation.current) {
        setError(e instanceof Error ? e.message : translate("读取失败"));
        setFailureState(
          e instanceof APIError && [401, 403].includes(e.status)
            ? "forbidden"
            : e instanceof Error &&
                /privileged|agent_not_configured|特权执行器/i.test(e.message)
              ? "unconfigured"
              : "error",
        );
      }
    } finally {
      if (request === generation.current) setLoading(false);
    }
  }, [name, serialized, enabled]);
  useEffect(() => {
    setData(undefined);
    setError("");
  }, [name, serialized]);
  useEffect(() => {
    void reload();
    return () => {
      ++generation.current;
      setLoading(false);
    };
  }, [reload]);
  useEffect(() => {
    if (!enabled) return;
    let scheduled: ReturnType<typeof setTimeout> | undefined;
    const invalidate = () => {
      clearTimeout(scheduled);
      scheduled = setTimeout(() => void reload(), 200);
    };
    window.addEventListener("powerlinux:invalidate", invalidate);
    const interval = poll
      ? setInterval(() => {
          if (document.visibilityState === "visible") void reload();
        }, poll)
      : undefined;
    return () => {
      window.removeEventListener("powerlinux:invalidate", invalidate);
      clearTimeout(scheduled);
      if (interval) clearInterval(interval);
    };
  }, [reload, enabled, poll]);
  const current = dataKey === queryKey ? data : undefined;
  const collection = current as
    | { items?: unknown[]; availability?: string }
    | undefined;
  const state: PageState = !enabled
    ? "unknown"
    : error
      ? failureState
      : loading && current === undefined
        ? "loading"
        : collection?.availability === "agent_not_configured"
          ? "unconfigured"
          : current === undefined
            ? "unknown"
            : (Array.isArray(current) && !current.length) ||
                (Array.isArray(collection?.items) && !collection.items.length)
              ? "empty"
              : "ready";
  return {
    state,
    data: dataKey === queryKey ? data : undefined,
    error,
    loading,
    loadedAt,
    reload,
  };
}
