import type { ReactNode } from "react";

import type { AgentStatus, GrantSource } from "../../api/types";

const STATUS_DOT: Record<AgentStatus, string> = {
  idle: "bg-muted",
  running: "bg-ok animate-pulse",
  waiting: "bg-warn",
  blocked: "bg-danger",
  done: "bg-accent",
  error: "bg-danger",
};

export function StatusDot({ status }: { status: AgentStatus }) {
  return (
    <span
      role="img"
      className={`inline-block h-2.5 w-2.5 shrink-0 rounded-full ${STATUS_DOT[status] ?? "bg-muted"}`}
      title={status}
      aria-label={`status ${status}`}
    />
  );
}

export function Chip({ children, title }: { children: ReactNode; title?: string }) {
  return (
    <span
      title={title}
      className="inline-block max-w-full truncate rounded bg-raised px-1.5 py-0.5 text-xs text-muted"
    >
      {children}
    </span>
  );
}

const SOURCE_STYLE: Record<GrantSource, string> = {
  inherited: "bg-raised text-muted",
  explicit: "bg-raised text-text",
  human: "bg-accent/15 text-accent",
};

export function SourceBadge({ source }: { source: GrantSource }) {
  return (
    <span
      className={`rounded px-1.5 py-0.5 text-xs ${SOURCE_STYLE[source]}`}
      title={
        source === "human"
          ? "Edited by a person; may exceed the parent's grants (A13)"
          : source === "inherited"
            ? "Copied from the parent; follows its revocations (A14)"
            : "Set explicitly for this chat"
      }
    >
      {source}
    </span>
  );
}

export function fieldClass(): string {
  return "w-full rounded border border-border bg-surface px-2 py-1.5 text-sm";
}

export function buttonClass(kind: "default" | "primary" | "danger" = "default"): string {
  const base = "min-h-[36px] rounded border px-3 py-1.5 text-sm disabled:opacity-50 ";
  if (kind === "primary") return base + "border-accent bg-accent text-bg font-medium";
  if (kind === "danger") return base + "border-danger text-danger hover:bg-danger/10";
  return base + "border-border bg-surface hover:bg-raised";
}
