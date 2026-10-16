const taskReadDeniedCodes = new Set([
  "forbidden",
  "unauthorized",
  "permission_denied",
  "insufficient_scope",
  "resource_denied",
  "task_access_denied",
]);

export type TaskObservationNotificationState = {
  unavailable: boolean;
  finished: boolean;
};

export function claimTaskObservationNotification(
  state: TaskObservationNotificationState,
  kind: keyof TaskObservationNotificationState,
): { state: TaskObservationNotificationState; notify: boolean } {
  if (state[kind]) return { state, notify: false };
  return { state: { ...state, [kind]: true }, notify: true };
}

export function isTaskObservationDenied(error: unknown): boolean {
  if (!error || typeof error !== "object") return false;
  const candidate = error as { code?: unknown; status?: unknown };
  return (
    candidate.status === 401 ||
    candidate.status === 403 ||
    (typeof candidate.code === "string" &&
      taskReadDeniedCodes.has(candidate.code))
  );
}
