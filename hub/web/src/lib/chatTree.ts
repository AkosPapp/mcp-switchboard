/**
 * The chat list as a tree (spec.md U37): client group -> chats (roots) -> child
 * chats, recursively, by `parentChatId`. Pure: no React, no clock.
 *
 * Grouping is by `clientLabel` and nothing else: never by prompt. A chat whose
 * parent is unknown (deleted, archived, or filtered out by a search) is a root,
 * so nothing is ever lost from the list; legacy peer chats are ordinary chats.
 */

import type { Chat } from "../views/chat/types";

export interface ChatNode {
  chat: Chat;
  /** 0 for a root chat; each nesting level adds one */
  depth: number;
  children: ChatNode[];
  /** newest activity (ms) anywhere in this subtree */
  latest: number;
  /** ids of every chat in this subtree, this one first */
  chatIds: string[];
}

export interface ClientGroup {
  /** unique across the tree, usable as a collapse-memory key */
  id: string;
  /** the client's label; null for the "No client" group */
  label: string | null;
  roots: ChatNode[];
  chatIds: string[];
  latest: number;
}

export const NO_CLIENT_ID = "client-none";
export const groupId = (label: string | null) => (label === null ? NO_CLIENT_ID : `client:${label}`);

const ts = (s: string | undefined) => {
  const n = Date.parse(s ?? "");
  return Number.isNaN(n) ? 0 : n;
};

/** A peer conversation from before chats messaged each other directly. */
export const isLegacy = (c: Chat) => c.kind === "agent" || (c.kind === "spawn" && Boolean(c.peerAgentId));

const byActivity = (a: ChatNode, b: ChatNode) =>
  b.latest - a.latest || (a.chat.title || "").localeCompare(b.chat.title || "");

export function buildChatTree(chats: Chat[]): ClientGroup[] {
  const byId = new Map(chats.map((c) => [c.id, c]));
  const kids = new Map<string, Chat[]>();
  const roots: Chat[] = [];
  for (const c of chats) {
    const p = c.parentChatId;
    // An unknown parent, or a self-parent, makes this a root.
    if (p && p !== c.id && byId.has(p)) kids.set(p, [...(kids.get(p) ?? []), c]);
    else roots.push(c);
  }

  const seen = new Set<string>();
  const build = (chat: Chat, depth: number): ChatNode | null => {
    if (seen.has(chat.id)) return null; // a parent cycle
    seen.add(chat.id);
    const children = (kids.get(chat.id) ?? [])
      .map((k) => build(k, depth + 1))
      .filter((n): n is ChatNode => n !== null)
      .sort(byActivity);
    return {
      chat,
      depth,
      children,
      latest: Math.max(ts(chat.updatedAt), ...children.map((c) => c.latest)),
      chatIds: [chat.id, ...children.flatMap((c) => c.chatIds)],
    };
  };

  const perClient = new Map<string | null, ChatNode[]>();
  const place = (chat: Chat) => {
    const node = build(chat, 0);
    if (!node) return;
    const label = chat.clientLabel ?? null;
    perClient.set(label, [...(perClient.get(label) ?? []), node]);
  };
  for (const c of roots) place(c);
  // Anything a cycle left unreachable still shows up, as a root.
  for (const c of chats) if (!seen.has(c.id)) place(c);

  return [...perClient]
    .map(([label, nodes]): ClientGroup => {
      nodes.sort(byActivity);
      return {
        id: groupId(label),
        label,
        roots: nodes,
        chatIds: nodes.flatMap((n) => n.chatIds),
        latest: Math.max(0, ...nodes.map((n) => n.latest)),
      };
    })
    // Stable, predictable order: by label, the client-less group last.
    .sort((a, b) => (a.label === null ? 1 : b.label === null ? -1 : a.label.localeCompare(b.label)));
}

/** Indentation steps beyond this add no further left margin, so deep trees stay usable on a phone. */
export const MAX_INDENT = 3;
export const indentFor = (depth: number) => Math.min(depth, MAX_INDENT);

function pathIn(nodes: ChatNode[], chatId: string, trail: string[]): string[] | null {
  for (const n of nodes) {
    if (n.chat.id === chatId) return trail;
    const r = pathIn(n.children, chatId, [...trail, n.chat.id]);
    if (r) return r;
  }
  return null;
}

/** Group id plus every ancestor chat id on the way to the chat: what must be open to show it. */
export function ancestorsOf(groups: ClientGroup[], chatId: string): string[] | null {
  for (const g of groups) {
    const r = pathIn(g.roots, chatId, [g.id]);
    if (r) return r;
  }
  return null;
}

/** The chat itself plus every chat nested under it, by `parentChatId`. */
export function subtreeIds(chats: Chat[], id: string): string[] {
  const kids = new Map<string, string[]>();
  for (const c of chats) if (c.parentChatId) kids.set(c.parentChatId, [...(kids.get(c.parentChatId) ?? []), c.id]);
  const out: string[] = [];
  const seen = new Set<string>();
  const walk = (x: string) => {
    if (seen.has(x)) return;
    seen.add(x);
    out.push(x);
    for (const k of kids.get(x) ?? []) walk(k);
  };
  walk(id);
  return out;
}
