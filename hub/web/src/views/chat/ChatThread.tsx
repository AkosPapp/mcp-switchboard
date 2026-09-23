import { useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";

import { useConnections } from "../../api/queries";
import { useAgent, useProfiles } from "../../api/resources";
import ClientBadge from "../../components/ClientBadge";
import { forgetTab } from "../../lib/routeMemory";
import { useToast } from "../../components/Toast";
import { useChatStream } from "../../hooks/useChatStream";
import { overlayThread } from "../../lib/streamAssembly";
import { runningLabel, runningToolNames } from "../../lib/runningTools";
import ApprovalCard from "./ApprovalCard";
import QuestionCard from "./QuestionCard";
import {
  branchChat,
  cancelRun,
  deleteChat,
  exportUrl,
  patchChat,
  postMessage,
  useChat,
  useDecideApproval,
  useInvalidateChat,
  useMessages,
  useModels,
  useRun,
  useSystemPrompt,
} from "./api";
import ChatDialog from "./ChatDialog";
import Composer, { TruncationWarning } from "./Composer";
import { modelLabel } from "./format";
import { newKey } from "./idempotency";
import MessageView, { DraftView, type MessageActions } from "./MessageView";
import SystemPromptPanel from "./SystemPromptPanel";
import ToolsPanel from "./ToolsPanel";
import ToolCard, { type ToolCardData } from "./ToolCard";
import { ACTIVE_RUN, type ContentBlock, type Message, type ModelInfo, type Question } from "./types";

/** The editable text of a queued message (attachments are kept as they are). */
function queuedText(content: string | ContentBlock[]): string {
  if (typeof content === "string") return content;
  return content.map((b) => (b.type === "text" ? (b.text ?? "") : "")).filter(Boolean).join("\n");
}

/** A short one-line preview of a queued message's content, for the queue strip. */
function previewContent(content: string | ContentBlock[]): string {
  const text =
    typeof content === "string"
      ? content
      : content
          .map((b) => (b.type === "text" ? (b.text ?? "") : `[${b.type}]`))
          .join(" ");
  const trimmed = text.trim();
  return trimmed.length > 80 ? `${trimmed.slice(0, 80)}…` : trimmed || "(attachment)";
}

/** Tool results by the call they answer (R6: one result per tool message). */
function resultsByCall(messages: Message[]) {
  const map = new Map<string, { result?: unknown; error?: string | null; recordId?: string }>();
  for (const m of messages) {
    if (m.role !== "tool") continue;
    for (const r of m.toolResults ?? []) {
      map.set(r.tool_call_id, { result: r.result, error: r.error ?? null, recordId: r.call_id });
    }
  }
  return map;
}

export default function ChatThread({
  chatId,
  onOpenList,
  overviewOpen = false,
  onToggleOverview,
}: {
  chatId: string;
  onOpenList: () => void;
  overviewOpen?: boolean;
  onToggleOverview?: () => void;
}) {
  const toast = useToast();
  const invalidate = useInvalidateChat();
  const client = useQueryClient();
  const chat = useChat(chatId);
  const messagesQuery = useMessages(chatId);
  const agent = useAgent(chat.data?.agentId ?? null);
  const models = useModels();
  const profiles = useProfiles();
  const connections = useConnections();
  const navigate = useNavigate();
  const [toolsOpen, setToolsOpen] = useState(false);
  const [promptOpen, setPromptOpen] = useState(false);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [menuOpen, setMenuOpen] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const { state, connection, synced } = useChatStream(chatId);

  const path = useMemo(() => messagesQuery.data?.messages ?? [], [messagesQuery.data]);
  useEffect(() => {
    if (path.length > 0) synced(path.map((m) => m.id));
  }, [path, synced]);

  const { messages, drafts } = overlayThread(path, state);

  const lastRunId =
    state.activeRuns[state.activeRuns.length - 1] ??
    [...messages].reverse().find((m) => m.runId)?.runId ??
    null;
  const run = useRun(lastRunId);
  const runActive =
    state.activeRuns.length > 0 || (run.data ? ACTIVE_RUN.includes(run.data.status) : false);

  const results = useMemo(() => resultsByCall(messages), [messages]);
  const pathCallIds = useMemo(
    () => new Set(messages.flatMap((m) => (m.toolCalls ?? []).map((c) => c.id))),
    [messages],
  );

  const liveTools: ToolCardData[] = state.tools
    .filter((t) => !pathCallIds.has(t.callId) && t.name)
    .map((t) => ({
      callId: t.callId,
      name: t.name as string,
      arguments: t.arguments,
      done: t.done,
      result: t.result,
      error: t.error,
    }));

  const approvals = new Map<string, { runId: string; callId: string; tool: string; arguments: unknown; expiresAt: string; questions?: Question[] }>();
  for (const a of state.approvals) approvals.set(a.callId, a);
  if (run.data && run.data.status === "waiting") {
    for (const a of run.data.pendingApprovals ?? []) {
      if (!approvals.has(a.callId)) approvals.set(a.callId, { runId: run.data.id, ...a });
    }
  }

  // The call ran, or the user decided: no card (U31).
  const { decide, answer, dismissed, inflight } = useDecideApproval();
  for (const id of [...approvals.keys()]) {
    if (dismissed.has(id) || results.has(id) || state.tools.some((t) => t.callId === id && t.done)) approvals.delete(id);
  }
  const running = runActive
    ? runningToolNames(
        [
          ...messages.flatMap((m) => (m.toolCalls ?? []).map((c) => ({ callId: c.id, name: c.name, done: false }))),
          ...state.tools.map((t) => ({ callId: t.callId, name: t.name, done: t.done })),
        ],
        new Set(approvals.keys()),
        new Set(results.keys()),
      )
    : [];

  // Keep the newest content in view unless the reader scrolled up.
  const scroller = useRef<HTMLDivElement>(null);
  const stick = useRef(true);
  useEffect(() => {
    const el = scroller.current;
    if (el && stick.current) el.scrollTop = el.scrollHeight;
  });

  const refresh = () => invalidate(chatId);
  const fail = (error: unknown) => toast.show(error instanceof Error ? error.message : String(error));

  const actions: MessageActions = {
    busy: runActive,
    models: models.data?.models ?? [],
    regenerateWith: (m, model) => {
      stick.current = true;
      branchChat(chatId, { fromMessageId: m.id, model: { provider: model.provider, model: model.model } }).then(refresh, fail);
    },
    select: (id) => {
      patchChat(chatId, { selectMessageId: id }).then(refresh, fail);
    },
    regenerate: (m) => {
      stick.current = true;
      branchChat(chatId, { fromMessageId: m.id }).then(refresh, fail);
    },
    edit: (m, content) => {
      stick.current = true;
      branchChat(chatId, { fromMessageId: m.id, content }).then(refresh, fail);
    },
    branchHere: (m) => {
      patchChat(chatId, { activeLeafId: m.id }).then(() => {
        refresh();
        toast.show("branched: your next message continues from here");
      }, fail);
    },
  };

  // One key per user action, reused if the same send is retried.
  const pendingKey = useRef<string | null>(null);
  const sendNow = async (content: string | ContentBlock[], model: { provider: string; model: string } | null) => {
    pendingKey.current ??= newKey();
    await postMessage(
      chatId,
      { content, ...(model ? { model: { provider: model.provider, model: model.model } } : {}) },
      pendingKey.current,
    );
    pendingKey.current = null;
    stick.current = true;
    refresh();
  };

  // While a run is active, Enter/Send queues the message locally instead of
  // posting it right away: the composer clears immediately (so the next
  // message can be typed straight away), and each queued message is sent,
  // one at a time, once the chat is no longer running (below).
  interface Queued {
    id: string;
    content: string | ContentBlock[];
    model: ModelInfo | null;
  }
  const [queue, setQueue] = useState<Queued[]>([]);
  const draining = useRef(false);
  const send = async (content: string | ContentBlock[], model: ModelInfo | null) => {
    if (runActive || queue.length > 0) {
      setQueue((q) => [...q, { id: newKey(), content, model }]);
      return;
    }
    await sendNow(content, model);
  };
  useEffect(() => {
    if (runActive || draining.current || queue.length === 0) return;
    const [next, ...rest] = queue;
    draining.current = true;
    setQueue(rest);
    sendNow(next.content, next.model)
      .catch((error) => toast.show(error instanceof Error ? error.message : "could not send queued message"))
      .finally(() => {
        draining.current = false;
      });
    // sendNow and toast are stable enough for this effect's purpose; only
    // runActive and queue length should retrigger the drain.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [runActive, queue]);
  const [editingQueued, setEditingQueued] = useState<string | null>(null);
  const editQueued = (id: string, text: string) => {
    setEditingQueued(null);
    setQueue((q) =>
      q.flatMap((m) => {
        if (m.id !== id) return [m];
        if (typeof m.content === "string") return text.trim() ? [{ ...m, content: text }] : [];
        // Keep attachments; the edit replaces the text block.
        const rest = m.content.filter((b) => b.type !== "text");
        const blocks = text.trim() ? [{ type: "text" as const, text }, ...rest] : rest;
        return blocks.length ? [{ ...m, content: blocks }] : [];
      }),
    );
  };
  const removeQueued = (id: string) => setQueue((q) => q.filter((m) => m.id !== id));

  const title = chat.data?.title || "Untitled chat";
  // Without a profile the prompt is the chat's own text or none; only the prompt endpoint says which.
  const ownPrompt = useSystemPrompt(chatId, chat.data !== undefined && !chat.data.profileId);
  const promptChip = chat.data?.profileId
    ? `prompt: ${profiles.data?.find((p) => p.id === chat.data?.profileId)?.name ?? "removed"}`
    : ownPrompt.data?.source === "agent"
      ? "custom prompt"
      : "no prompt";
  const clientLabel = chat.data?.clientLabel ?? null;
  const clientConn = connections.data?.connections.find((c) => c.label === clientLabel);
  const removeChat = async () => {
    try {
      const { deletedChats } = await deleteChat(chatId);
      forgetTab("/chat");
      invalidate();
      toast.show(deletedChats > 1 ? `${deletedChats} chats deleted` : "Chat deleted");
      navigate("/chat", { replace: true });
    } catch (error) {
      fail(error);
    }
  };

  return (
    <div className="relative flex h-full min-h-0 flex-col">
      <header className="flex items-center gap-2 border-b border-border px-3 py-1">
        <div className="min-w-0 flex-1">
          <div className="flex items-baseline gap-2">
            <h1 className="truncate text-sm font-medium">{title}</h1>
            <span className="hidden shrink-0 truncate text-xs text-muted sm:inline">
              {agent.data ? modelLabel(agent.data.model) : ""}
              {connection !== "live" ? `${agent.data ? " · " : ""}stream ${connection}` : ""}
            </span>
          </div>
          {chat.data ? (
            <div
              className="flex min-w-0 flex-wrap items-center gap-x-1.5 text-[11px] text-muted"
              data-testid="chat-chips"
            >
              <span>{promptChip}</span>
              <span aria-hidden="true">·</span>
              {clientLabel ? (
                <span className={clientConn || !connections.data ? "" : "opacity-60"}>
                  <ClientBadge
                    label={clientLabel}
                    environment={clientConn?.client.environment}
                    connected={connections.data ? clientConn !== undefined : undefined}
                    compact
                    bare
                  />
                </span>
              ) : (
                <span>no client</span>
              )}
            </div>
          ) : null}
        </div>
        {/* Settings / System prompt / Tools: separate buttons once there's room
            (md+); on mobile they move into the "…" menu below so the title gets
            the header's width instead of being squeezed by a row of buttons. */}
        <button
          type="button"
          onClick={() => setSettingsOpen(true)}
          disabled={!chat.data}
          className="hidden rounded px-2 py-1 text-xs text-muted hover:bg-raised hover:text-text disabled:opacity-40 md:block"
        >
          Settings
        </button>
        <button
          type="button"
          onClick={() => setPromptOpen(true)}
          className="hidden rounded px-2 py-1 text-xs text-muted hover:bg-raised hover:text-text disabled:opacity-40 md:block"
        >
          System prompt
        </button>
        <button
          type="button"
          onClick={() => setToolsOpen(true)}
          className="hidden rounded px-2 py-1 text-xs text-muted hover:bg-raised hover:text-text disabled:opacity-40 md:block"
        >
          Tools
        </button>
        {onToggleOverview ? (
          <button
            type="button"
            aria-label="overview"
            aria-pressed={overviewOpen}
            title="Overview of the conversation's branches"
            onClick={onToggleOverview}
            className={`rounded px-2 py-1 text-xs hover:bg-raised hover:text-text ${overviewOpen ? "text-accent" : "text-muted"}`}
          >
            Overview
          </button>
        ) : null}
        <div className="relative">
          <button
            type="button"
            aria-label="chat menu"
            aria-haspopup="menu"
            aria-expanded={menuOpen}
            onClick={() => {
              setMenuOpen((v) => !v);
              setConfirmDelete(false);
            }}
            className="rounded px-2 py-1 text-sm text-muted hover:bg-raised hover:text-text"
          >
            <span aria-hidden="true">⋯</span>
          </button>
          {menuOpen ? (
            <>
              <div className="fixed inset-0 z-10" onClick={() => setMenuOpen(false)} aria-hidden="true" />
              <div
                role="menu"
                className="absolute right-0 z-20 mt-1 w-52 rounded border border-border bg-surface p-1 text-sm shadow"
              >
                <button
                  type="button"
                  role="menuitem"
                  disabled={!chat.data}
                  onClick={() => {
                    setMenuOpen(false);
                    setSettingsOpen(true);
                  }}
                  className="block w-full rounded px-2 py-2 text-left hover:bg-raised disabled:opacity-40 md:hidden"
                >
                  Settings
                </button>
                <button
                  type="button"
                  role="menuitem"
                  onClick={() => {
                    setMenuOpen(false);
                    setPromptOpen(true);
                  }}
                  className="block w-full rounded px-2 py-2 text-left hover:bg-raised md:hidden"
                >
                  System prompt
                </button>
                <button
                  type="button"
                  role="menuitem"
                  onClick={() => {
                    setMenuOpen(false);
                    setToolsOpen(true);
                  }}
                  className="block w-full rounded px-2 py-2 text-left hover:bg-raised md:hidden"
                >
                  Tools
                </button>
                <a
                  role="menuitem"
                  className="block rounded px-2 py-2 hover:bg-raised"
                  href={exportUrl(chatId, "json")}
                  download
                >
                  Export JSON (whole tree)
                </a>
                <a
                  role="menuitem"
                  className="block rounded px-2 py-2 hover:bg-raised"
                  href={exportUrl(chatId, "markdown")}
                  download
                >
                  Export Markdown (this path)
                </a>
                {confirmDelete ? (
                  <div className="flex items-center gap-1 px-2 py-1" role="group" aria-label="confirm delete chat">
                    <span className="flex-1 text-danger">Delete this chat and its sub-chats?</span>
                    <button
                      type="button"
                      onClick={() => void removeChat()}
                      className="min-h-[44px] rounded border border-danger px-2 text-danger md:min-h-0 md:py-1"
                    >
                      Delete
                    </button>
                    <button
                      type="button"
                      onClick={() => setConfirmDelete(false)}
                      className="min-h-[44px] rounded border border-border px-2 md:min-h-0 md:py-1"
                    >
                      Cancel
                    </button>
                  </div>
                ) : (
                  <button
                    type="button"
                    role="menuitem"
                    onClick={() => setConfirmDelete(true)}
                    className="block min-h-[44px] w-full rounded px-2 py-2 text-left text-danger hover:bg-raised md:min-h-0"
                  >
                    Delete chat…
                  </button>
                )}
              </div>
            </>
          ) : null}
        </div>
      </header>

      <div
        ref={scroller}
        onScroll={(e) => {
          const el = e.currentTarget;
          stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
        }}
        className="min-h-0 flex-1 space-y-3 overflow-y-auto px-3 py-3"
        data-testid="thread"
      >
        {messagesQuery.isError ? (
          <p className="text-sm text-danger">{(messagesQuery.error as Error).message}</p>
        ) : null}
        {messages.length === 0 && drafts.length === 0 && !messagesQuery.isLoading ? (
          <p className="pt-8 text-center text-sm text-muted">Say something to start.</p>
        ) : null}

        {messages
          .filter((m) => m.role !== "tool" && m.role !== "system")
          .map((m) => (
            <MessageView
              key={m.id}
              message={m}
              actions={actions}
              tools={(m.toolCalls ?? []).map((c) => {
                const r = results.get(c.id);
                return {
                  callId: c.id,
                  name: c.name,
                  arguments: c.arguments,
                  done: r !== undefined,
                  result: r?.result,
                  error: r?.error,
                  recordId: r?.recordId,
                };
              })}
            />
          ))}

        {drafts.map((d) => (
          <DraftView key={d.messageId} draft={d} />
        ))}
        {liveTools.map((t) => (
          <ToolCard key={t.callId} tool={t} />
        ))}
        {[...approvals.values()].map((a) =>
          a.questions?.length ? (
            <QuestionCard
              key={a.callId}
              questions={a.questions}
              expiresAt={a.expiresAt}
              busy={inflight.has(a.callId)}
              onAnswer={(answers) => answer(a, answers)}
              onSkip={() => decide(a, false)}
            />
          ) : (
          <ApprovalCard
            key={a.callId}
            {...a}
            busy={inflight.has(a.callId)}
            onDecide={(approved) => decide(a, approved)}
          />
          ),
        )}
        {running.length > 0 ? (
          <p
            role="status"
            data-testid="running-tools"
            className="flex items-center gap-2 font-mono text-xs text-muted"
          >
            <span
              aria-hidden="true"
              className="h-3 w-3 shrink-0 animate-spin rounded-full border-2 border-warn border-t-transparent"
            />
            <span className="min-w-0 break-words">{runningLabel(running)}</span>
          </p>
        ) : null}
        {state.error ? <p className="text-sm text-danger">{state.error}</p> : null}
        <TruncationWarning usage={run.data?.usage} />
        {!runActive && run.data && run.data.status === "error" && run.data.error ? (
          <p className="text-sm text-danger">run failed: {run.data.error}</p>
        ) : null}
      </div>

      {settingsOpen && chat.data ? (
        <ChatDialog
          chat={chat.data}
          onClose={() => setSettingsOpen(false)}
          onSaved={() => {
            setSettingsOpen(false);
            invalidate(chatId);
            client.invalidateQueries({ queryKey: ["chatSystemPrompt"] });
            client.invalidateQueries({ queryKey: ["chatTools"] });
            toast.show("Chat updated");
          }}
        />
      ) : null}
      {promptOpen ? <SystemPromptPanel chatId={chatId} onClose={() => setPromptOpen(false)} /> : null}
      {toolsOpen ? <ToolsPanel chatId={chatId} onClose={() => setToolsOpen(false)} /> : null}

      {queue.length > 0 ? (
        <ul className="shrink-0 space-y-1 border-t border-border bg-surface px-3 pt-2" data-testid="message-queue">
          {queue.map((q, i) => (
            <li
              key={q.id}
              className="flex items-center gap-2 rounded border border-border bg-raised px-2 py-1 text-sm text-muted"
            >
              <span className="shrink-0 text-[11px] text-muted">#{i + 1} queued</span>
              {editingQueued === q.id ? (
                <textarea
                  autoFocus
                  aria-label="edit queued message"
                  defaultValue={queuedText(q.content)}
                  rows={Math.min(6, queuedText(q.content).split("\n").length)}
                  className="min-w-0 flex-1 resize-none rounded border border-accent bg-bg px-1 py-0.5 text-sm text-text"
                  onFocus={(e) => e.currentTarget.setSelectionRange(e.currentTarget.value.length, e.currentTarget.value.length)}
                  onBlur={(e) => editQueued(q.id, e.currentTarget.value)}
                  onKeyDown={(e) => {
                    if (e.key === "Escape") setEditingQueued(null);
                    else if (e.key === "Enter" && !e.shiftKey) {
                      e.preventDefault();
                      editQueued(q.id, e.currentTarget.value);
                    }
                  }}
                />
              ) : (
                <button
                  type="button"
                  title="Click to edit"
                  onClick={() => setEditingQueued(q.id)}
                  className="min-w-0 flex-1 truncate text-left hover:text-text"
                >
                  {previewContent(q.content)}
                </button>
              )}
              <button
                type="button"
                aria-label="remove queued message"
                className="shrink-0 px-1 text-muted hover:text-text"
                onClick={() => removeQueued(q.id)}
              >
                ×
              </button>
            </li>
          ))}
        </ul>
      ) : null}

      <Composer
        chatId={chatId}
        models={models.data?.models ?? []}
        running={runActive}
        budget={run.data?.budgetSnapshot ?? agent.data?.budget}
        usage={run.data?.usage ?? {}}
        onSend={send}
        defaultModel={agent.data?.model}
        onStop={() => {
          if (lastRunId) cancelRun(lastRunId).then(refresh, fail);
        }}
        onOpenList={onOpenList}
      />
    </div>
  );
}
