export type ApplicationState = { status?: string; running?: boolean };
export function applicationState(
  app: ApplicationState,
): "running" | "stopped" | "unknown" {
  if (app.running === true && ["running", "built"].includes(app.status ?? ""))
    return "running";
  if (
    app.running === false &&
    ["stopped", "created", "built"].includes(app.status ?? "")
  )
    return "stopped";
  return "unknown";
}
export function applicationCounts(items: ApplicationState[]) {
  const counts = { running: 0, stopped: 0, unknown: 0 };
  for (const item of items) counts[applicationState(item)]++;
  return counts;
}
