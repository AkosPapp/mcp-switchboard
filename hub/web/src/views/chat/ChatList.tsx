import { useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";

import { useQueryClient } from "@tanstack/react-query";

import { useConnections } from "../../api/queries";
import { ApiError } from "../../api/client";
import { useAgents } from "../../api/resources";
import ClientBadge from "../../components/ClientBadge";
import { useToast } from "../../components/Toast";
import { useAttention } from "../../hooks/useAttention";
import { ancestorsOf, buildChatTree, subtreeIds, type ChatNode, type ClientGroup } from "../../lib/chatTree";
import type { Mark } from "../../lib/attention";
import { forgetTab } from "../../lib/routeMemory";
import { readMemory, writeMemory, type Validate } from "../../lib/uiMemory";
import { deleteChat, exportUrl, patchChat, useChats, useInvalidateChat } from "./api";
import ChatRow from "./ChatRow";
import type { Agent, Chat } from "./types";

const COLLAPSED_KEY = "chat.tree.collapsed";
const validCollapsed: Validate<string[]> = (v) =>
  Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : undefined;

interface TreeCtx {
  collapsed: ReadonlySet<string>;
  /** while searching, every node with a match is open so the match is visible */
  forceOpen: boolean;
  toggle: (id: string) => void;
  marks: Record<string, Mark>;
  chatProps: (c: Chat, childCount: number) => Parameters<typeof ChatRow>[0];
}

/** One chat and, nested beneath it, the chats it spawned (recursively, collapsible). */
function ChatBranch({ node, ctx }: { node: ChatNode; ctx: TreeCtx }) {
  const childCount = node.chatIds.length - 1;
  const open = ctx.forceOpen || !ctx.collapsed.has(node.chat.id);
  return (
    <ChatRow
      key={node.chat.id}
      {...ctx.chatProps(node.chat, childCount)}
      depth={node.depth}
      childCount={childCount}
      open={open}
      onToggle={() => ctx.toggle(node.chat.id)}
      nested={node.children.map((child) => (
        <ChatBranch key={child.chat.id} node={child} ctx={ctx} />
      ))}
    />
  );
}

function GroupBranch({
  group,
  ctx,
  connected,
  environment,
  onNewChat,
}: {
  group: ClientGroup;
  ctx: TreeCtx;
  connected?: boolean;
  environment?: Parameters<typeof ClientBadge>[0]["environment"];
  onNewChat: () => void;
}) {
  const open = ctx.forceOpen || !ctx.collapsed.has(group.id);
  const needy = group.chatIds.filter((id) => ctx.marks[id]).length;
  return (
    <li role="none" data-testid="client-group" data-client={group.label ?? ""}>
      <div className="group/h sticky top-0 z-[2] flex min-h-[44px] items-center border-y border-border bg-surface pr-1 md:min-h-0">
        <button
          type="button"
          aria-expanded={open}
          onClick={() => ctx.toggle(group.id)}
          className="flex min-h-[44px] min-w-0 flex-1 items-center gap-1.5 px-1.5 py-1 text-left text-xs md:min-h-0"
        >
          <span aria-hidden="true" className="w-3 shrink-0 text-center text-[10px] text-muted">
            {open ? "▾" : "▸"}
          </span>
          <span className="min-w-0 flex-1 uppercase tracking-wide">
            {group.label === null ? (
              <span className="font-semibold">No client</span>
            ) : (
              <span className={connected === false ? "opacity-60" : ""} title={connected === false ? "offline" : undefined}>
                <ClientBadge label={group.label} environment={environment} connected={connected} compact wrap bare />
              </span>
            )}
          </span>
          {!open && needy > 0 ? (
            <span
              data-testid="group-attention"
              className="shrink-0 rounded-full bg-warn px-1.5 py-0.5 text-[10px] leading-none text-bg"
            >
              {needy}
            </span>
          ) : null}
          <span
            className="shrink-0 text-[11px] text-muted"
            data-testid="group-count"
            title={`${group.chatIds.length} chat${group.chatIds.length === 1 ? "" : "s"}`}
          >
            {group.chatIds.length}
          </span>
        </button>
        <button
          type="button"
          aria-label={`new chat for ${group.label ?? "no client"}`}
          title="New chat in this group"
          onClick={onNewChat}
          className="flex h-[44px] w-[44px] shrink-0 items-center justify-center rounded text-sm text-muted hover:bg-raised hover:text-text md:h-5 md:w-5 md:opacity-0 md:focus:opacity-100 md:group-hover/h:opacity-100 [@media(hover:none)]:md:opacity-100"
        >
          +
        </button>
      </div>
      {open ? (
        <ul className="pb-1">
          {group.roots.map((node) => (
            <ChatBranch key={node.chat.id} node={node} ctx={ctx} />
          ))}
        </ul>
      ) : null}
    </li>
  );
}

const FILTER_KEY = "chat.list";
const EMPTY_FILTER = { q: "", archived: false };
function validFilter(v: unknown): typeof EMPTY_FILTER | undefined {
  if (v === null || typeof v !== "object") return undefined;
  const o = v as Record<string, unknown>;
  return { q: typeof o.q === "string" ? o.q : "", archived: o.archived === true };
}

/** A count for a toast: one chat, or a chat with the sub-chats that went with it. */
const deletedText = (n: number) => (n > 1 ? `${n} chats deleted` : "Chat deleted");

export default function ChatList({
  activeId,
  onNavigate,
  onNewChat,
}: {
  activeId: string | null;
  onNavigate: () => void;
  /** Start the new-chat flow, preselecting this client (null: "New chat" at the top, or no client). */
  onNewChat: (client: string | null) => void;
}) {
  const toast = useToast();
  const navigate = useNavigate();
  const invalidate = useInvalidateChat();
  const queryClient = useQueryClient();
  const agents = useAgents();
  const connections = useConnections();
  // The search box and archived toggle are remembered by the browser.
  const [saved] = useState(() => readMemory(FILTER_KEY, validFilter, EMPTY_FILTER));
  const [input, setInput] = useState(saved.q);
  const [q, setQ] = useState(saved.q);
  const [archived, setArchived] = useState(saved.archived);
  useEffect(() => {
    writeMemory(FILTER_KEY, { q: input, archived });
  }, [input, archived]);
  const [selecting, setSelecting] = useState(false);
  const [picked, setPicked] = useState<Set<string>>(new Set());

  // Typing searches after a pause, not on every key.
  useEffect(() => {
    const id = window.setTimeout(() => setQ(input.trim()), 250);
    return () => window.clearTimeout(id);
  }, [input]);

  const chats = useChats({ q, includeArchived: archived });
  const chatList = useMemo(() => chats.data?.chats ?? [], [chats.data]);
  const agentById = useMemo(() => new Map((agents.data ?? []).map((a) => [a.id, a] as [string, Agent])), [agents.data]);
  const filtering = q !== "";
  const tree = useMemo(() => buildChatTree(chatList), [chatList]);
  const connByLabel = useMemo(
    () => new Map((connections.data?.connections ?? []).map((c) => [c.label, c])),
    [connections.data],
  );
  const attention = useAttention();
  const [collapsedList, setCollapsedList] = useState(() => readMemory(COLLAPSED_KEY, validCollapsed, []));
  const collapsed = useMemo(() => new Set(collapsedList), [collapsedList]);
  const toggleNode = (id: string) => {
    const next = collapsed.has(id) ? collapsedList.filter((x) => x !== id) : [...collapsedList, id];
    setCollapsedList(next);
    writeMemory(COLLAPSED_KEY, next);
  };
  // The open chat's path stays open, whatever was collapsed before.
  const openPath = useMemo(() => new Set(activeId ? (ancestorsOf(tree, activeId) ?? []) : []), [tree, activeId]);

  const fail = (error: unknown) => toast.show(error instanceof Error ? error.message : String(error));

  const toggle = (id: string) =>
    setPicked((current) => {
      const next = new Set(current);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });

  const leaveIfOpen = (ids: string[]) => {
    if (activeId === null || !ids.includes(activeId)) return;
    forgetTab("/chat");
    navigate("/chat");
  };

  const dropFromCache = (ids: string[]) => {
    const gone = new Set(ids);
    queryClient.setQueriesData<{ chats: Chat[] }>({ queryKey: ["chats"] }, (old) =>
      old && Array.isArray(old.chats) ? { ...old, chats: old.chats.filter((c) => !gone.has(c.id)) } : old,
    );
  };

  const archiveOne = async (chat: Chat) => {
    const archiving = !chat.archivedAt;
    // Optimistic: the row goes at once, the refetch below confirms it.
    if (archiving && !archived) dropFromCache([chat.id]);
    try {
      await patchChat(chat.id, { archived: archiving });
      toast.show(archiving ? "Archived" : "Unarchived");
      if (archiving && !archived) leaveIfOpen([chat.id]);
    } catch (error) {
      fail(error);
    } finally {
      invalidate(chat.id);
    }
  };

  // The row already asked ("Delete?" then the check mark), so this goes straight
  // through. Its sub-chats go with it, so they leave the list at once too.
  const deleteOne = async (chat: Chat) => {
    const ids = subtreeIds(chatList, chat.id);
    dropFromCache(ids);
    try {
      const { deletedChats } = await deleteChat(chat.id);
      toast.show(deletedText(deletedChats));
      leaveIfOpen(ids);
    } catch (error) {
      fail(error);
    } finally {
      invalidate(chat.id);
    }
  };

  const bulk = async (what: "archive" | "delete" | "export") => {
    const ids = [...picked];
    if (ids.length === 0) return;
    try {
      if (what === "delete") {
        if (!window.confirm(`Delete ${ids.length} chat(s) and their sub-chats? This cannot be undone.`)) return;
        // One at a time: deleting a parent removes a picked child, whose own delete is then a 404.
        const gone = new Set<string>();
        for (const id of ids) {
          try {
            subtreeIds(chatList, id).forEach((x) => gone.add(x));
            await deleteChat(id);
          } catch (error) {
            if (!(error instanceof ApiError && error.status === 404)) throw error;
          }
        }
        leaveIfOpen([...gone]);
      } else if (what === "archive") {
        await Promise.all(ids.map((id) => patchChat(id, { archived: true })));
      } else {
        const exported = await Promise.all(
          ids.map(async (id) => (await fetch(exportUrl(id, "json"))).json()),
        );
        const blob = new Blob([JSON.stringify(exported, null, 2)], { type: "application/json" });
        const a = document.createElement("a");
        a.href = URL.createObjectURL(blob);
        a.download = `chats-${ids.length}.json`;
        a.click();
        URL.revokeObjectURL(a.href);
      }
      setPicked(new Set());
      invalidate();
    } catch (error) {
      fail(error);
    }
  };

  const field =
    "min-h-[44px] min-w-0 flex-1 rounded border border-border bg-bg px-2 py-1 text-sm placeholder:text-muted focus:border-accent md:min-h-0";

  const ctx: TreeCtx = {
    collapsed: new Set([...collapsed].filter((id) => !openPath.has(id))),
    forceOpen: filtering,
    toggle: toggleNode,
    marks: attention.marks,
    chatProps: (c) => ({
      chat: c,
      agent: agentById.get(c.agentId),
      hits: chats.data?.hits?.[c.id],
      mark: attention.marks[c.id],
      active: c.id === activeId,
      selecting,
      checked: picked.has(c.id),
      onCheck: () => toggle(c.id),
      onNavigate,
      onArchive: () => void archiveOne(c),
      onDelete: () => void deleteOne(c),
    }),
  };

  return (
    <div className="flex h-full min-h-0 flex-col bg-surface">
      <div className="space-y-1.5 border-b border-border p-2">
        <div className="flex items-center gap-1.5">
          <input
            type="search"
            value={input}
            onChange={(e) => setInput(e.target.value)}
            placeholder="search"
            aria-label="search chats"
            className={field}
          />
          <button
            type="button"
            onClick={() => onNewChat(null)}
            className="min-h-[44px] shrink-0 rounded bg-accent px-2.5 py-1 text-sm font-medium text-on-accent md:min-h-0"
          >
            New chat
          </button>
          {attention.count > 0 ? (
            <span
              data-testid="chat-attention-badge"
              aria-label={`${attention.count} chat${attention.count === 1 ? "" : "s"} need you`}
              className="shrink-0 rounded-full bg-warn px-1.5 py-0.5 text-[11px] font-semibold leading-none text-bg"
            >
              {attention.count}
            </span>
          ) : null}
        </div>
        {attention.permission === "default" ? (
          <button
            type="button"
            onClick={() => void attention.enableNotifications()}
            className="min-h-[44px] w-full text-left text-[11px] text-muted hover:text-text md:min-h-0"
          >
            Enable notifications for approvals and replies
          </button>
        ) : null}
        <div className="flex items-center gap-3 text-[11px] text-muted">
          <label className="flex min-h-[44px] items-center gap-1 md:min-h-0">
            <input type="checkbox" checked={archived} onChange={(e) => setArchived(e.target.checked)} />
            archived
          </label>
          <button
            type="button"
            aria-pressed={selecting}
            onClick={() => {
              setSelecting((v) => !v);
              setPicked(new Set());
            }}
            className={`min-h-[44px] md:min-h-0 ${selecting ? "text-accent" : "hover:text-text"}`}
          >
            {selecting ? "Done" : "Select"}
          </button>
          {selecting ? (
            <>
              <span>{picked.size} selected</span>
              {(["archive", "export", "delete"] as const).map((what) => (
                <button
                  key={what}
                  type="button"
                  disabled={picked.size === 0}
                  onClick={() => void bulk(what)}
                  className={`min-h-[44px] capitalize disabled:opacity-40 md:min-h-0 ${
                    what === "delete" ? "text-danger" : "hover:text-text"
                  }`}
                >
                  {what}
                </button>
              ))}
            </>
          ) : null}
        </div>
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto" data-testid="chat-list">
        {chats.isError ? (
          <p className="p-3 text-sm text-danger">{(chats.error as Error).message}</p>
        ) : null}
        {chats.data && tree.length === 0 ? (
          <p className="p-3 text-sm text-muted">
            {filtering ? "No chats match." : "No chats yet. Start one with New chat."}
          </p>
        ) : null}
        <ul data-testid="chat-tree">
          {tree.map((g) => {
            const conn = g.label === null ? undefined : connByLabel.get(g.label);
            return (
              <GroupBranch
                key={g.id}
                group={g}
                ctx={ctx}
                connected={connections.data ? conn !== undefined : undefined}
                environment={conn?.client.environment}
                onNewChat={() => onNewChat(g.label)}
              />
            );
          })}
        </ul>
      </div>
    </div>
  );
}
