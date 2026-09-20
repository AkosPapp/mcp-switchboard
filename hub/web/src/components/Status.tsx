import type { CallStatus, ServerState } from "../api/types";

const CALL_COLOURS: Record<CallStatus, string> = {
  ok: "text-ok",
  error: "text-danger",
  denied: "text-warn",
};

export function CallStatusBadge({ status }: { status: CallStatus }) {
  return (
    <span className={`font-medium ${CALL_COLOURS[status] ?? "text-muted"}`}>{status}</span>
  );
}

const SERVER_COLOURS: Record<ServerState, string> = {
  running: "bg-ok",
  starting: "bg-warn",
  exited: "bg-muted",
  failed: "bg-danger",
};

export function ServerStateDot({ state }: { state: ServerState }) {
  return (
    <span
      className={`inline-block h-2 w-2 shrink-0 rounded-full ${SERVER_COLOURS[state] ?? "bg-muted"}`}
      title={state}
      aria-label={state}
    />
  );
}

/** Durations read better rounded: a call is either sub-second or it is not. */
export function formatDuration(ms: number): string {
  if (ms < 1000) return `${Math.round(ms)} ms`;
  return `${(ms / 1000).toFixed(2)} s`;
}

/** Local time, seconds precision - the call log is read while it happens. */
export function formatTime(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return iso;
  return date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

export function formatDateTime(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return iso;
  return date.toLocaleString();
}
