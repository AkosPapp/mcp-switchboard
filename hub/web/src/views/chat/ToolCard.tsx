import { useState } from "react";
import { Link } from "react-router-dom";

import { ApiError } from "../../api/client";
import { useCall } from "../../api/queries";
import CopyButton from "../../components/CopyButton";
import Json, { pretty } from "../../components/Json";
import { formatDuration } from "../../components/Status";

export interface ToolCardData {
  callId: string;
  name: string;
  arguments: unknown;
  /** undefined while the call is still running */
  done: boolean;
  result?: unknown;
  error?: string | null;
  /** id of the row in the call log, from the tool message (R6) */
  recordId?: string;
}

const STATUS_COLOUR: Record<string, string> = {
  ok: "text-ok",
  error: "text-danger",
  denied: "text-warn",
  running: "text-warn",
};

/** A collapsible tool call: name, where it ran, how long, raw JSON (U9). */
export default function ToolCard({ tool }: { tool: ToolCardData }) {
  const [open, setOpen] = useState(false);
  const record = useCall(tool.recordId ?? null);
  const expired = record.error instanceof ApiError && record.error.status === 404;
  const call = record.data;

  const status = !tool.done
    ? "running"
    : (call?.status ?? (tool.error ? "error" : "ok"));
  const where = call ? [call.label, call.server].filter(Boolean).join(" / ") : "";

  return (
    <div className="my-2 rounded border border-border bg-surface text-sm" data-testid="tool-card">
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
        className="flex w-full min-w-0 items-center gap-2 px-2 py-1.5 text-left"
      >
        <span aria-hidden="true" className="text-muted">
          {open ? "▾" : "▸"}
        </span>
        <span className="truncate font-mono text-xs font-medium">{tool.name}</span>
        {where ? <span className="hidden truncate text-xs text-muted sm:inline">{where}</span> : null}
        <span className="ml-auto flex shrink-0 items-center gap-2 text-xs">
          {tool.done ? null : (
            <span
              aria-hidden="true"
              data-testid="tool-spinner"
              className="h-3 w-3 animate-spin rounded-full border-2 border-warn border-t-transparent"
            />
          )}
          {call ? <span className="text-muted">{formatDuration(call.durationMs)}</span> : null}
          <span className={`font-medium ${STATUS_COLOUR[status] ?? "text-muted"}`}>{status}</span>
        </span>
      </button>

      {open ? (
        <div className="space-y-2 border-t border-border p-2">
          {where ? (
            <p className="text-xs text-muted sm:hidden">{where}</p>
          ) : null}
          <div>
            <div className="mb-1 flex items-center justify-between text-xs text-muted">
              <span>arguments</span>
              <CopyButton value={pretty(tool.arguments)} />
            </div>
            <Json value={tool.arguments} className="max-h-64" />
          </div>
          {tool.done ? (
            <div>
              <div className="mb-1 flex items-center justify-between text-xs text-muted">
                <span>{tool.error ? "error" : "result"}</span>
                <CopyButton value={tool.error ?? pretty(tool.result)} />
              </div>
              {tool.error ? (
                <pre className="overflow-auto rounded border border-border bg-raised p-2 font-mono text-xs text-danger">
                  {tool.error}
                </pre>
              ) : (
                <Json value={tool.result} className="max-h-64" />
              )}
            </div>
          ) : null}
          <div className="text-xs">
            {tool.recordId && !expired ? (
              <Link className="text-accent underline" to={`/calls?call=${encodeURIComponent(tool.recordId)}`}>
                open in Calls
              </Link>
            ) : tool.recordId ? (
              <span className="text-muted">call record expired</span>
            ) : null}
          </div>
        </div>
      ) : null}
    </div>
  );
}
