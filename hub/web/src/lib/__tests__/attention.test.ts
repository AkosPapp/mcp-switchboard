import { afterEach, describe, expect, it, vi } from "vitest";

import type { Agent, ApprovalItem, Chat } from "../../views/chat/types";
import { AttentionTracker, showNotification, titleWithCount, type AttentionInput } from "../attention";

const chat = (id: string, updatedAt: string, over: Partial<Chat> = {}): Chat => ({
  id, agentId: "a1", peerAgentId: null, title: id, kind: "human", activeLeafId: null, tags: [],
  tokenTotal: 0, costTotalMicros: 0, createdAt: "2026-01-01T00:00:00Z", updatedAt, archivedAt: null, ...over,
});
const agent = (status: Agent["status"] = "idle"): Agent =>
  ({ id: "a1", parentId: null, name: "helper", description: "", project: null, model: {}, status, budget: {}, deletedAt: null }) as Agent;
const approval = (chatId: string, callId = "k1"): ApprovalItem => ({
  runId: "r1", chatId, agentId: "a1", agentName: "helper", chatTitle: chatId, callId,
  tool: "run_command", arguments: {}, expiresAt: "2026-01-01T00:05:00Z",
});
const T1 = "2026-01-01T10:00:00Z";
const T2 = "2026-01-01T11:00:00Z";
const input = (over: Partial<AttentionInput> = {}): AttentionInput => ({
  approvals: [], chats: [chat("c1", T1), chat("c2", T1)], agents: [agent()], openChatId: null, visible: true, ...over,
});

describe("AttentionTracker", () => {
  it("does nothing until both sources have answered", () => {
    const t = new AttentionTracker(null);
    expect(t.update(input({ approvals: undefined }))).toEqual({ marks: {}, notify: [] });
    expect(t.update(input({ chats: undefined }))).toEqual({ marks: {}, notify: [] });
  });

  it("baseline is silent, even for a pending approval", () => {
    const t = new AttentionTracker(null);
    const out = t.update(input({ approvals: [approval("c1")] }));
    expect(out.marks.c1.reason).toBe("approval");
    expect(out.notify).toEqual([]);
    // Nothing is a reply on a fresh browser.
    expect(out.marks.c2).toBeUndefined();
  });

  it("marks a chat that moved on while the console was closed, without notifying", () => {
    const t = new AttentionTracker({ c1: T1, c2: T1 });
    const out = t.update(input({ chats: [chat("c1", T2), chat("c2", T1)] }));
    expect(out.marks.c1.reason).toBe("reply");
    expect(out.notify).toEqual([]);
  });

  it("an approval appears, notifies once, and clears when resolved", () => {
    const t = new AttentionTracker(null);
    t.update(input());
    const a = t.update(input({ approvals: [approval("c1")] }));
    expect(a.notify.map((m) => m.chatId)).toEqual(["c1"]);
    const again = t.update(input({ approvals: [approval("c1")] }));
    expect(again.marks.c1).toBeDefined();
    expect(again.notify).toEqual([]); // dedupe
    const gone = t.update(input({ approvals: [] }));
    expect(gone.marks).toEqual({});
    const back = t.update(input({ approvals: [approval("c1", "k2")] }));
    expect(back.notify).toHaveLength(1);
  });

  it("a finished turn in another chat is a new reply; opening it marks it seen", () => {
    const t = new AttentionTracker(null);
    t.update(input({ openChatId: "c2" }));
    const out = t.update(input({ chats: [chat("c1", T2), chat("c2", T1)], openChatId: "c2" }));
    expect(out.marks.c1.reason).toBe("reply");
    expect(out.notify.map((m) => m.chatId)).toEqual(["c1"]);
    const opened = t.update(input({ chats: [chat("c1", T2), chat("c2", T1)], openChatId: "c1" }));
    expect(opened.marks).toEqual({});
    expect(t.snapshot().c1).toBe(T2);
  });

  it("the open, visible chat is never marked or notified; a hidden tab is", () => {
    const t = new AttentionTracker(null);
    t.update(input({ openChatId: "c1" }));
    const shown = t.update(input({ chats: [chat("c1", T2), chat("c2", T1)], openChatId: "c1", approvals: [approval("c1")] }));
    expect(shown.marks).toEqual({});
    expect(shown.notify).toEqual([]);
    const hidden = t.update(input({ chats: [chat("c1", T2), chat("c2", T1)], openChatId: "c1", visible: false, approvals: [approval("c1")] }));
    expect(hidden.marks.c1.reason).toBe("approval");
    expect(hidden.notify).toHaveLength(1);
  });

  it("waits while the agent is still working, and reports a failure", () => {
    const t = new AttentionTracker(null);
    t.update(input());
    const chats = [chat("c1", T2), chat("c2", T1)];
    expect(t.update(input({ chats, agents: [agent("running")] })).marks).toEqual({});
    const failed = t.update(input({ chats, agents: [agent("error")] }));
    expect(failed.marks.c1.reason).toBe("error");
    expect(failed.notify).toHaveLength(1);
  });

  it("ignores old peer conversations and archived chats, but not sub-chats", () => {
    const t = new AttentionTracker(null);
    t.update(input());
    const out = t.update(
      input({
        chats: [
          chat("c1", T2, { kind: "spawn", peerAgentId: "a9" }),
          chat("c2", T2, { archivedAt: T2 }),
          chat("c3", T2, { kind: "agent" }),
          chat("c4", T2, { kind: "spawn", parentChatId: "c0" }),
        ],
      }),
    );
    expect(Object.keys(out.marks)).toEqual(["c4"]);
  });

  it("words its notifications in terms of chats", () => {
    const t = new AttentionTracker(null);
    t.update(input({ approvals: [approval("c1")] }));
    const out = t.update(input({ chats: [chat("c1", T2), chat("c2", T1)], approvals: [approval("c1")] }));
    expect(out.marks.c1.body).toMatch(/^wants to run /);
    expect(JSON.stringify(out.marks)).not.toMatch(/agent/i);
  });
});

describe("browser notification", () => {
  afterEach(() => vi.unstubAllGlobals());
  const mark = { reason: "approval" as const, key: "k", chatId: "c1", title: "T", body: "b" };

  it("tags per chat and focuses on click; silent when denied or unsupported", () => {
    const made: { title: string; opts: { tag: string }; onclick?: () => void; close: () => void }[] = [];
    class Fake {
      static permission = "granted";
      onclick?: () => void;
      close = vi.fn();
      constructor(public title: string, public opts: { tag: string }) { made.push(this); }
    }
    vi.stubGlobal("Notification", Fake);
    vi.spyOn(window, "focus").mockImplementation(() => {});
    const open = vi.fn();
    showNotification(mark, open);
    expect(made[0].opts.tag).toBe("mcpsb-chat-c1");
    made[0].onclick!();
    expect(open).toHaveBeenCalledWith("c1");

    Fake.permission = "denied";
    showNotification(mark, open);
    expect(made).toHaveLength(1);
    vi.stubGlobal("Notification", undefined);
    expect(() => showNotification(mark, open)).not.toThrow();
  });

  it("prefixes the title with the count", () => {
    expect(titleWithCount("Console", 2)).toBe("(2) Console");
    expect(titleWithCount("(2) Console", 3)).toBe("(3) Console");
    expect(titleWithCount("(2) Console", 0)).toBe("Console");
  });
});
