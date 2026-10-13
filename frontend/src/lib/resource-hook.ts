"use client";
import { useCallback, useEffect, useRef, useState } from "react";
import { action } from "./api";
export function useResource<T>(
  name: string,
  input: unknown,
  enabled: boolean,
  poll = 15000,
) {
  const [data, setData] = useState<T>();
  const [dataKey, setDataKey] = useState<string>();
  const [error, setError] = useState("");
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
      if (request === generation.current)
        setError(e instanceof Error ? e.message : "读取失败");
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
  return {
    data: dataKey === queryKey ? data : undefined,
    error,
    loading,
    loadedAt,
    reload,
  };
}
