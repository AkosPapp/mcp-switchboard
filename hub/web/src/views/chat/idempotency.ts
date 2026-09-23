/** A fresh id per user action; crypto.randomUUID needs a secure context, and
 * the console is often reached over plain HTTP through a tunnel. */
export function newKey(): string {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
    return crypto.randomUUID();
  }
  return `k-${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;
}
