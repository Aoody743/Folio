// Temporary browser-memory drafts, never the authoritative resource store.
// Authentication changes clear them, including any sensitive form fields.
const drafts = new Map<string, unknown>();
export function readDraft<T>(key: string): T | undefined {
  return drafts.get(key) as T | undefined;
}
export function saveDraft(key: string, value: unknown) {
  drafts.set(key, value);
}
export function forgetDraft(key: string) {
  drafts.delete(key);
}
export function clearDrafts() {
  drafts.clear();
}
