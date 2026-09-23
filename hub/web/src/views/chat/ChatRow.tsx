import { useEffect, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";

import { REASON_LABEL, type Mark } from "../../lib/attention";
import { indentFor, isLegacy } from "../../lib/chatTree";
import { formatCost, formatTokens, modelLabel, snippetParts } from "./format";
import type { Agent, Chat, SearchHit } from "./types";

const BADGE = "rounded border border-border px-1.5 py-0.5 text-[11px] leading-none text-muted";
export const STATUS_DOT: Record<string, string> = {
  running: "bg-ok",
  waiting: "bg-warn",
  blocked: "bg-warn",
  error: "bg-danger",
  idle: "bg-muted",
  done: "bg-muted",
};

/** How long the row's delete button stays armed before it reverts on its own. */
export const CONFIRM_MS = 4000;

function Hits({ hits }: { hits: SearchHit[] }) {
  return (
    <>
      {hits.slice(0, 2).map((h) => (
        <p key={h.messageId} className="truncate text-xs text-muted">
          {snippetParts(h.snippet).map((p, i) =>
            p.hit ? (
              <mark key={i} className="bg-transparent font-semibold text-text">
                {p.text}
              </mark>
            ) : (
              <span key={i}>{p.text}</span>
            ),
          )}
        </p>
      ))}
    </>
  );
}

const ACTION =
  "flex h-11 w-11 items-center justify-center rounded border bg-surface hover:text-text md:h-8 md:w-8 [@media(hover:none)]:md:h-11 [@media(hover:none)]:md:w-11";
// Shown on hover and keyboard focus-within; always on touch screens, which have no hover.
const REVEAL =
  "opacity-0 focus:opacity-100 group-hover:opacity-100 group-focus-within:opacity-100 [@media(hover:none)]:opacity-100";

/**
 * One chat, and (below it) the chats nested under it. Archive hides it and asks
 * nothing; Delete is permanent, so the button arms into an inline "Delete? ✓ ✗"
 * for a few seconds (no modal) and only the second press deletes. Deleting a
 * chat deletes its child chats too, and the armed text says how many.
 */
export default function ChatRow({
  chat,
  agent,
  depth = 0,
  childCount = 0,
  open = true,
  onToggle,
  nested,
  hits,
  mark,
  active,
  selecting,
  checked,
  onCheck,
  onNavigate,
  onArchive,
  onDelete,
}: {
  chat: Chat;
  agent?: Agent;
  /** nesting level under a root chat; 0 for a root */
  depth?: number;
  /** chats nested directly or indirectly under this one */
  childCount?: number;
  /** the nested chats are shown */
  open?: boolean;
  onToggle?: () => void;
  /** the nested list, rendered below the row */
  nested?: ReactNode;
  hits?: SearchHit[];
  mark?: Mark;
  active: boolean;
  selecting: boolean;
  checked: boolean;
  onCheck: () => void;
  onNavigate: () => void;
  onArchive: () => void;
  onDelete: () => void;
}) {
  const [confirming, setConfirming] = useState(false);
  useEffect(() => {
    if (!confirming) return;
    const id = window.setTimeout(() => setConfirming(false), CONFIRM_MS);
    return () => window.clearTimeout(id);
  }, [confirming]);

  const stop = (fn: () => void) => (e: React.MouseEvent) => {
    e.preventDefault();
    e.stopPropagation();
    fn();
  };
  const name = chat.title || "Untitled chat";

  const legacy = isLegacy(chat);

  return (
    <li role="none" data-testid="chat-node" data-chat={chat.id} data-depth={depth}>
    <div className={`group relative flex items-stretch ${active ? "bg-raised" : ""}`} data-testid="chat-row">
      {selecting ? (
        <label className="flex min-h-[44px] w-11 shrink-0 items-center justify-center">
          <input type="checkbox" checked={checked} onChange={onCheck} aria-label={`select ${name}`} />
        </label>
      ) : null}
      {childCount > 0 ? (
        <button
          type="button"
          aria-expanded={open}
          aria-label={`${open ? "collapse" : "expand"} sub-chats of ${name}`}
          onClick={onToggle}
          className="flex min-h-[44px] w-8 shrink-0 items-center justify-center text-xs text-muted hover:text-text"
        >
          <span aria-hidden="true">{open ? "▾" : "▸"}</span>
        </button>
      ) : null}
      <Link
        to={`/chat/${chat.id}`}
        onClick={onNavigate}
        aria-current={active ? "page" : undefined}
        className={`block min-h-[44px] min-w-0 flex-1 py-2 pr-3 hover:bg-raised [@media(hover:none)]:pr-28 ${childCount > 0 ? "pl-0" : "pl-3"}`}
      >
        <div className="flex items-center gap-2">
          <span
            className={`h-2 w-2 shrink-0 rounded-full ${STATUS_DOT[agent?.status ?? "idle"] ?? "bg-muted"}`}
            title={agent?.status ?? ""}
          />
          <span className="truncate text-sm font-medium">{name}</span>
          {mark ? (
            <span
              data-testid="attention-marker"
              className={`shrink-0 rounded px-1.5 py-0.5 text-[11px] font-semibold leading-none text-bg ${
                mark.reason === "error" ? "bg-danger" : "bg-warn"
              }`}
            >
              {REASON_LABEL[mark.reason]}
            </span>
          ) : null}
          {!open && childCount > 0 ? (
            <span className="shrink-0 text-xs text-muted" data-testid="child-count">
              {childCount} sub-chat{childCount === 1 ? "" : "s"}
            </span>
          ) : null}
        </div>
        <div className="mt-1 flex flex-wrap gap-1">
          {agent?.model?.model ? <span className={BADGE}>{modelLabel(agent.model)}</span> : null}
          {legacy ? <span className={BADGE}>legacy</span> : null}
          <span className={BADGE}>{formatTokens(chat.tokenTotal)} tok</span>
          <span className={BADGE}>{formatCost(chat.costTotalMicros)}</span>
          {chat.tags.map((t) => (
            <span key={t} className={BADGE}>
              #{t}
            </span>
          ))}
          {chat.archivedAt ? <span className={BADGE}>archived</span> : null}
        </div>
        {hits ? <Hits hits={hits} /> : null}
      </Link>
      {selecting ? null : (
        <div className="absolute right-1 top-1 flex items-center gap-1">
          {confirming ? (
            <div
              role="group"
              aria-label={`delete ${name}?`}
              className="flex items-center gap-1 rounded border border-danger bg-surface pl-2"
            >
              <span className="text-xs font-medium text-danger">
                {childCount > 0 ? `Delete + ${childCount} sub-chat${childCount === 1 ? "" : "s"}?` : "Delete?"}
              </span>
              <button
                type="button"
                onClick={stop(() => {
                  setConfirming(false);
                  onDelete();
                })}
                aria-label="Confirm delete"
                title="Confirm delete"
                className="flex h-11 w-11 items-center justify-center text-danger md:h-8 md:w-8 [@media(hover:none)]:md:h-11 [@media(hover:none)]:md:w-11"
              >
                <span aria-hidden="true">✓</span>
              </button>
              <button
                type="button"
                onClick={stop(() => setConfirming(false))}
                aria-label="Cancel delete"
                title="Cancel delete"
                className="flex h-11 w-11 items-center justify-center text-muted md:h-8 md:w-8 [@media(hover:none)]:md:h-11 [@media(hover:none)]:md:w-11"
              >
                <span aria-hidden="true">✗</span>
              </button>
            </div>
          ) : (
            <>
              <button
                type="button"
                onClick={stop(onArchive)}
                aria-label={chat.archivedAt ? "Unarchive" : "Archive"}
                title={chat.archivedAt ? "Unarchive" : "Archive"}
                className={`${ACTION} ${REVEAL} border-border text-muted`}
              >
                <span aria-hidden="true">{chat.archivedAt ? "↩" : "▤"}</span>
              </button>
              <button
                type="button"
                onClick={stop(() => setConfirming(true))}
                aria-label="Delete"
                title="Delete"
                className={`${ACTION} ${REVEAL} border-border text-muted hover:border-danger hover:text-danger`}
              >
                <span aria-hidden="true">🗑</span>
              </button>
            </>
          )}
        </div>
      )}
    </div>
    {open && nested ? (
      <ul
        className="divide-y divide-border border-l border-border"
        style={{ marginLeft: `${indentFor(depth) < 3 ? 12 : 4}px` }}
      >
        {nested}
      </ul>
    ) : null}
    </li>
  );
}
