/**
 * Which chats need the user (spec.md U33), as a small state machine.
 *
 * Pure apart from the injected sinks: `update` takes the latest server data and
 * returns the marks to show and the notifications to raise. Three reasons, in
 * priority order: a run waits for approval, a run failed, a chat replied
 * (the chat changed after the user last saw it, and its run stopped working).
 *
 * The first update with data establishes a baseline and notifies nothing, so
 * opening the console never produces a storm. With no remembered "seen" map at
 * all, every existing chat is also marked seen; with one, chats that moved on
 * while the console was closed stay marked (quietly).
 */

import type { Agent, ApprovalItem, Chat } from "../views/chat/types";
import { isLegacy } from "./chatTree";
import { readMemory, writeMemory, type Validate } from "./uiMemory";

export type Reason = "approval" | "question" | "error" | "reply";

export interface Mark {
  reason: Reason;
  /** changes when the cause changes; the dedupe key for notifications */
  key: string;
  chatId: string;
  title: string;
  body: string;
}

export interface AttentionInput {
  approvals: ApprovalItem[] | undefined;
  chats: Chat[] | undefined;
  agents: Agent[] | undefined;
  openChatId: string | null;
  /** the tab is visible and focused */
  visible: boolean;
}

export interface AttentionOutput {
  marks: Record<string, Mark>;
  notify: Mark[];
}

export const REASON_LABEL: Record<Reason, string> = {
  approval: "needs approval",
  question: "has a question",
  error: "run failed",
  reply: "new reply",
};

const ms = (s: string | undefined) => {
  const n = Date.parse(s ?? "");
  return Number.isNaN(n) ? 0 : n;
};

export class AttentionTracker {
  private seen: Record<string, string>;
  private readonly fresh: boolean;
  private primed = false;
  private notified = new Set<string>();

  constructor(seen: Record<string, string> | null) {
    this.fresh = seen === null;
    this.seen = { ...(seen ?? {}) };
  }

  snapshot(): Record<string, string> {
    return { ...this.seen };
  }

  update(input: AttentionInput): AttentionOutput {
    const { approvals, chats, openChatId, visible } = input;
    if (approvals === undefined || chats === undefined) return { marks: {}, notify: [] };
    const agents = new Map((input.agents ?? []).map((a) => [a.id, a]));
    const watching = (chatId: string) => visible && openChatId === chatId;

    const baseline = !this.primed;
    this.primed = true;
    if (baseline && this.fresh) for (const c of chats) this.seen[c.id] = c.updatedAt;
    // The chat in front of the user is seen by definition.
    for (const c of chats) if (watching(c.id)) this.seen[c.id] = c.updatedAt;

    const marks: Record<string, Mark> = {};
    const byChat = new Map<string, ApprovalItem[]>();
    for (const a of approvals) {
      const list = byChat.get(a.chatId);
      if (list) list.push(a);
      else byChat.set(a.chatId, [a]);
    }
    for (const [chatId, list] of byChat) {
      if (watching(chatId)) continue; // the card is on screen
      // A question is more urgent to read than an approval is to click: it leads.
      const first = list.find((a) => a.questions?.length) ?? list[0];
      const asking = first.questions?.length ? first.questions[0].question : null;
      marks[chatId] = {
        reason: asking !== null ? "question" : "approval",
        key: `approval:${list.map((a) => a.callId).sort().join(",")}`,
        chatId,
        title: first.chatTitle || "Untitled chat",
        body: asking ?? `wants to run ${first.tool}${list.length > 1 ? ` (+${list.length - 1} more)` : ""}`,
      };
    }
    for (const c of chats) {
      // Old peer conversations never change any more; they are not a reason to look.
      if (marks[c.id] || isLegacy(c) || c.archivedAt) continue;
      if (ms(c.updatedAt) <= ms(this.seen[c.id])) continue;
      const agent = agents.get(c.agentId);
      if (agent && (agent.status === "running" || agent.status === "waiting")) continue; // mid-turn
      const failed = agent?.status === "error";
      marks[c.id] = {
        reason: failed ? "error" : "reply",
        key: `${failed ? "error" : "reply"}:${c.updatedAt}`,
        chatId: c.id,
        title: c.title || "Untitled chat",
        body: failed ? "the run failed" : "replied",
      };
    }

    const keys = new Set(Object.values(marks).map((m) => `${m.chatId}|${m.key}`));
    const notify = baseline
      ? []
      : Object.values(marks).filter((m) => !this.notified.has(`${m.chatId}|${m.key}`) && !watching(m.chatId));
    // Resolved marks drop out, so the same cause raised again would notify again.
    this.notified = keys;
    return { marks, notify };
  }
}

/* ---- persistence ---- */

const SEEN_KEY = "attention.seen";
const validSeen: Validate<Record<string, string>> = (v) => {
  if (v === null || typeof v !== "object" || Array.isArray(v)) return undefined;
  const out: Record<string, string> = {};
  for (const [k, val] of Object.entries(v)) if (typeof val === "string") out[k] = val;
  return out;
};
export const loadSeen = (): Record<string, string> | null => readMemory<Record<string, string> | null>(SEEN_KEY, validSeen, null);
export const saveSeen = (seen: Record<string, string>, keep: string[]) => {
  const live = new Set(keep);
  writeMemory(SEEN_KEY, Object.fromEntries(Object.entries(seen).filter(([id]) => live.has(id))));
};

/* ---- browser Notification API, degrading to nothing ---- */

export type NotifyPermission = "unsupported" | "default" | "granted" | "denied";

export function notifyPermission(): NotifyPermission {
  try {
    if (typeof Notification === "undefined") return "unsupported";
    return Notification.permission;
  } catch {
    return "unsupported";
  }
}

export async function requestNotifyPermission(): Promise<NotifyPermission> {
  try {
    if (typeof Notification === "undefined") return "unsupported";
    await Notification.requestPermission();
  } catch {
    // Some browsers throw on a non-user-gesture call; the state below says what happened.
  }
  return notifyPermission();
}

/** One notification per chat (`tag`), so a repeat replaces rather than stacks. */
export function showNotification(mark: Mark, onClick: (chatId: string) => void): void {
  if (notifyPermission() !== "granted") return;
  try {
    const n = new Notification(mark.title, { body: `${REASON_LABEL[mark.reason]}: ${mark.body}`, tag: `mcpsb-chat-${mark.chatId}` });
    n.onclick = () => {
      try {
        window.focus();
        n.close();
      } catch {
        // nothing to do
      }
      onClick(mark.chatId);
    };
  } catch {
    // Unsupported constructor (Android Chrome demands a service worker): stay silent.
  }
}

/** `(3) Title`, replacing any earlier prefix. */
export function titleWithCount(title: string, count: number): string {
  const base = title.replace(/^\(\d+\) /, "");
  return count > 0 ? `(${count}) ${base}` : base;
}
