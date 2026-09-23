import { useMemo, useState } from "react";

import { useChatTools } from "./api";
import type { ChatTool, ToolAnnotations } from "./types";

const CHIP = "rounded border border-border px-1.5 py-0.5 text-[11px] leading-none text-muted";

const HINTS: [keyof ToolAnnotations, string][] = [
  ["readOnlyHint", "read-only"],
  ["destructiveHint", "destructive"],
  ["idempotentHint", "idempotent"],
  ["openWorldHint", "open-world"],
];

function ToolItem({ tool }: { tool: ChatTool }) {
  const [open, setOpen] = useState(false);
  const chips = HINTS.filter(([k]) => tool.annotations?.[k]);
  return (
    <li className="px-3 py-2" data-testid="chat-tool">
      <div className="flex flex-wrap items-center gap-1">
        <code className="break-all text-sm font-medium">{tool.name}</code>
        {chips.map(([k, label]) => (
          <span key={k} className={CHIP}>
            {label}
          </span>
        ))}
      </div>
      {tool.description ? <p className="mt-1 text-xs text-muted">{tool.description}</p> : null}
      {tool.inputSchema ? (
        <>
          <button
            type="button"
            aria-expanded={open}
            onClick={() => setOpen((v) => !v)}
            className="mt-1 text-xs underline"
          >
            {open ? "Hide" : "Show"} input schema
          </button>
          {open ? (
            <pre className="mt-1 max-h-60 overflow-auto rounded bg-raised p-2 text-xs">
              {JSON.stringify(tool.inputSchema, null, 2)}
            </pre>
          ) : null}
        </>
      ) : null}
    </li>
  );
}

/** Bottom sheet below md, right-hand drawer from md up. */
export default function ToolsPanel({ chatId, onClose }: { chatId: string; onClose: () => void }) {
  const query = useChatTools(chatId);
  const data = query.data;

  const groups = useMemo(() => {
    const map = new Map<string, ChatTool[]>();
    const hub: ChatTool[] = [];
    for (const t of data?.tools ?? []) {
      if (t.origin === "hub" || !t.server) {
        hub.push(t);
        continue;
      }
      const key = [t.server.label, t.server.project, t.server.server].filter(Boolean).join("/");
      map.set(key, [...(map.get(key) ?? []), t]);
    }
    return { servers: [...map], hub };
  }, [data]);

  return (
    <div className="fixed inset-0 z-40 flex items-end md:items-stretch md:justify-end" role="presentation">
      <div className="absolute inset-0 bg-text/40" onClick={onClose} aria-hidden="true" />
      <aside
        role="dialog"
        aria-label="tools this chat can use"
        className="relative flex max-h-[80dvh] w-full flex-col rounded-t-lg border border-border bg-surface md:max-h-none md:w-[28rem] md:rounded-none"
      >
        <header className="flex items-center gap-2 border-b border-border px-3 py-2">
          <h2 className="flex-1 text-sm font-semibold">Tools this chat can use</h2>
          <button
            type="button"
            onClick={onClose}
            className="min-h-[44px] rounded border border-border px-3 text-sm md:min-h-0 md:py-1"
          >
            Close
          </button>
        </header>
        <div className="min-h-0 flex-1 overflow-y-auto">
          {query.isError ? (
            <p className="p-3 text-sm text-danger">{(query.error as Error).message}</p>
          ) : null}
          {query.isLoading ? <p className="p-3 text-sm text-muted">Loading…</p> : null}
          {data && !data.clientConnected ? (
            <p role="alert" className="border-b border-border bg-raised p-3 text-sm text-warn">
              Client offline — only hub tools available
              {data.clientLabel ? ` (${data.clientLabel} is not connected)` : ""}.
            </p>
          ) : null}
          {data && data.tools.length === 0 ? (
            <p className="p-3 text-sm text-muted">No tools available.</p>
          ) : null}
          {groups.servers.map(([key, tools]) => (
            <section key={key} aria-label={key}>
              <h3 className="sticky top-0 border-b border-border bg-raised px-3 py-1 text-xs font-semibold text-muted">
                {key}
              </h3>
              <ul className="divide-y divide-border">
                {tools.map((t) => (
                  <ToolItem key={t.name} tool={t} />
                ))}
              </ul>
            </section>
          ))}
          {groups.hub.length > 0 ? (
            <section aria-label="Hub tools">
              <h3 className="sticky top-0 border-b border-border bg-raised px-3 py-1 text-xs font-semibold text-muted">
                Hub tools
              </h3>
              <ul className="divide-y divide-border">
                {groups.hub.map((t) => (
                  <ToolItem key={t.name} tool={t} />
                ))}
              </ul>
            </section>
          ) : null}
        </div>
      </aside>
    </div>
  );
}
