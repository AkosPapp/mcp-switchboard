import { Link } from "react-router-dom";

import CopyButton from "../../components/CopyButton";
import { useSystemPrompt } from "./api";
import type { SystemPromptInfo } from "./types";

const CHIP = "rounded border border-border px-1.5 py-0.5 text-[11px] leading-none text-muted";

export function sourceLabel(info: SystemPromptInfo): string {
  if (info.source === "profile") return `from prompt ${info.profileName ?? "(removed)"}`;
  if (info.source === "agent") return "this chat's own prompt";
  return "none — no system prompt is sent";
}

/**
 * The exact system prompt the model receives next turn for this chat
 * (GET /api/chats/{id}/system-prompt), never abbreviated. Bottom sheet below
 * md, right-hand drawer from md up.
 */
export default function SystemPromptPanel({ chatId, onClose }: { chatId: string; onClose: () => void }) {
  const query = useSystemPrompt(chatId);
  const info = query.data;
  const model = info?.model
    ? `${info.model.provider}/${info.model.model}${info.modelIsDefault ? " (default)" : ""}`
    : "no model configured";

  return (
    <div className="fixed inset-0 z-40 flex items-end md:items-stretch md:justify-end" role="presentation">
      <div className="absolute inset-0 bg-text/40" onClick={onClose} aria-hidden="true" />
      <aside
        role="dialog"
        aria-label="system prompt for this chat"
        className="relative flex max-h-[85dvh] w-full flex-col rounded-t-lg border border-border bg-surface md:max-h-none md:w-[32rem] md:rounded-none"
      >
        <header className="flex items-center gap-2 border-b border-border px-3 py-2">
          <h2 className="flex-1 text-sm font-semibold">System prompt</h2>
          {info && info.systemPrompt ? (
            <CopyButton value={info.systemPrompt} label="Copy" className="min-h-[44px] md:min-h-0" />
          ) : null}
          <button
            type="button"
            onClick={onClose}
            className="min-h-[44px] rounded border border-border px-3 text-sm md:min-h-0 md:py-1"
          >
            Close
          </button>
        </header>
        <div className="min-h-0 flex-1 overflow-y-auto">
          {query.isError ? <p className="p-3 text-sm text-danger">{(query.error as Error).message}</p> : null}
          {query.isLoading ? <p className="p-3 text-sm text-muted">Loading…</p> : null}
          {info ? (
            <>
              <div className="flex flex-wrap items-center gap-1 border-b border-border p-3" data-testid="prompt-meta">
                <span className={CHIP} data-testid="prompt-source">
                  {sourceLabel(info)}
                </span>
                <span className={CHIP}>{model}</span>
                <span className={CHIP}>
                  {info.toolCount} tool{info.toolCount === 1 ? "" : "s"}
                </span>
                {info.source === "profile" && info.profileId ? (
                  <Link to={`/prompts/${info.profileId}`} className="ml-1 text-xs underline">
                    Edit prompt
                  </Link>
                ) : null}
              </div>
              {info.systemPrompt ? (
                <pre
                  data-testid="prompt-text"
                  className="whitespace-pre-wrap break-words p-3 font-mono text-xs"
                >
                  {info.systemPrompt}
                </pre>
              ) : (
                <p className="p-3 text-sm text-muted">No system prompt is sent to the model.</p>
              )}
            </>
          ) : null}
        </div>
      </aside>
    </div>
  );
}
