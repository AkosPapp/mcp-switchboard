import { describe, expect, it } from "vitest";

import {
  draftText,
  draftThinking,
  initialStream,
  overlayThread,
  parseFrame,
  streamReducer,
  type FrameBase,
  type StreamState,
} from "../streamAssembly";
import type { Message } from "../../views/chat/types";

const frame = (type: string, seq: number, extra: Record<string, unknown> = {}, runId = "r1"): FrameBase => ({
  type,
  runId,
  seq,
  ...extra,
});

const feed = (frames: FrameBase[], from: StreamState = initialStream) =>
  frames.reduce((s, f) => streamReducer(s, { type: "frame", frame: f }), from);

const msg = (id: string, parentId: string | null, role: Message["role"] = "assistant", text = ""): Message => ({
  id,
  chatId: "c",
  parentId,
  role,
  content: [{ type: "text", text }],
  toolCalls: null,
  toolResults: null,
  tokenInput: 0,
  tokenOutput: 0,
  costMicros: 0,
  latencyMs: 0,
  model: null,
  finishReason: "stop",
  runId: "r1",
  lastActiveChildId: null,
  createdAt: "",
});

describe("streamAssembly", () => {
  it("keeps reasoning apart from the answer", () => {
    const s = feed([
      frame("run_started", 1),
      frame("delta", 2, { messageId: "m1", contentIndex: 0, text: "hmm ", kind: "thinking" }),
      frame("delta", 3, { messageId: "m1", contentIndex: 0, text: "ok", kind: "thinking" }),
      frame("delta", 4, { messageId: "m1", contentIndex: 1, text: "Answer" }),
    ]);
    expect(draftThinking(s.drafts[0])).toBe("hmm ok");
    expect(draftText(s.drafts[0])).toBe("Answer");
  });

  it("assembles deltas by messageId and content index", () => {
    const s = feed([
      frame("run_started", 1),
      frame("delta", 2, { messageId: "m1", contentIndex: 0, text: "Hel" }),
      frame("delta", 3, { messageId: "m1", contentIndex: 0, text: "lo" }),
      frame("delta", 4, { messageId: "m1", contentIndex: 1, text: "second" }),
      frame("delta", 5, { messageId: "m2", contentIndex: 0, text: "other" }),
    ]);
    expect(s.drafts).toHaveLength(2);
    expect(draftText(s.drafts[0])).toBe("Hello\n\nsecond");
    expect(s.activeRuns).toEqual(["r1"]);
    expect(s.lastEventId).toBe("r1:5");
  });

  it("drops duplicates and older frames (resume replay)", () => {
    const s = feed([
      frame("delta", 1, { messageId: "m1", contentIndex: 0, text: "a" }),
      frame("delta", 2, { messageId: "m1", contentIndex: 0, text: "b" }),
      frame("delta", 2, { messageId: "m1", contentIndex: 0, text: "b" }),
      frame("delta", 1, { messageId: "m1", contentIndex: 0, text: "a" }),
    ]);
    expect(draftText(s.drafts[0])).toBe("ab");
    expect(s.lastEventId).toBe("r1:2");
  });

  it("tracks seq per run, so a queued run's frames are not mistaken for old ones", () => {
    const s = feed([
      frame("delta", 5, { messageId: "m1", contentIndex: 0, text: "x" }, "r1"),
      frame("delta", 1, { messageId: "m2", contentIndex: 0, text: "y" }, "r2"),
    ]);
    expect(s.drafts.map((d) => d.messageId)).toEqual(["m1", "m2"]);
    expect(s.lastEventId).toBe("r2:1");
  });

  it("applies an out-of-order gap but counts it, and message_done repairs it", () => {
    let s = feed([
      frame("delta", 1, { messageId: "m1", contentIndex: 0, text: "a" }),
      frame("delta", 4, { messageId: "m1", contentIndex: 0, text: "d" }),
    ]);
    expect(s.gaps).toBe(1);
    expect(draftText(s.drafts[0])).toBe("ad");
    s = feed([frame("message_done", 5, { message: msg("m1", "u1", "assistant", "abcd") })], s);
    expect(s.drafts).toHaveLength(0);
    expect(s.committed[0].content[0].text).toBe("abcd");
  });

  it("message_done replaces the assembled draft wholesale (N7)", () => {
    const s = feed([
      frame("delta", 1, { messageId: "m1", contentIndex: 0, text: "garbled" }),
      frame("message_done", 2, { message: msg("m1", "u1", "assistant", "the truth") }),
      frame("delta", 3, { messageId: "m1", contentIndex: 0, text: "late" }),
    ]);
    expect(s.drafts).toHaveLength(0);
    expect(s.committed).toHaveLength(1);
  });

  it("tracks tool calls, results and approvals, and clears approvals on run_done", () => {
    let s = feed([
      frame("tool_call", 1, { callId: "c1", name: "echo", arguments: { a: 1 } }),
      frame("approval_required", 2, { callId: "c1", tool: "echo", arguments: { a: 1 }, expiresAt: "2030-01-01T00:00:00Z" }),
    ]);
    expect(s.approvals).toHaveLength(1);
    s = feed([frame("tool_result", 3, { callId: "c1", result: "ok" })], s);
    expect(s.tools[0]).toMatchObject({ name: "echo", done: true, result: "ok" });
    s = feed([frame("run_done", 4, { status: "done" })], s);
    expect(s.approvals).toHaveLength(0);
    expect(s.lastRun).toEqual({ runId: "r1", status: "done" });
  });

  it("overflow discards assembled state and the resume position", () => {
    let s = feed([
      frame("run_started", 1),
      frame("delta", 2, { messageId: "m1", contentIndex: 0, text: "a" }),
    ]);
    s = streamReducer(s, { type: "overflow", reason: "slow_consumer" });
    expect(s).toMatchObject({ overflow: "slow_consumer", drafts: [], activeRuns: [], lastEventId: null });
    s = streamReducer(s, { type: "overflow_handled" });
    expect(s.overflow).toBeNull();
  });

  it("synced drops local copies of messages the fetched path now has", () => {
    let s = feed([
      frame("delta", 1, { messageId: "m2", contentIndex: 0, text: "a" }),
      frame("message_done", 2, { message: msg("m1", "u1") }),
    ]);
    s = streamReducer(s, { type: "synced", ids: ["m1"] });
    expect(s.committed).toHaveLength(0);
    expect(s.drafts).toHaveLength(1);
  });

  it("parses frames and rejects junk", () => {
    expect(parseFrame("delta", '{"runId":"r","seq":1}')?.type).toBe("delta");
    expect(parseFrame("delta", "nope")).toBeNull();
  });

  it("overlays committed messages and undecided drafts on the fetched path", () => {
    const path = [msg("u1", null, "user", "hi")];
    const s = feed([
      frame("delta", 1, { messageId: "m2", contentIndex: 0, text: "typing" }),
      frame("message_done", 2, { message: msg("m1", "u1") }),
    ]);
    const { messages, drafts } = overlayThread(path, s);
    expect(messages.map((m) => m.id)).toEqual(["u1", "m1"]);
    expect(drafts.map((d) => d.messageId)).toEqual(["m2"]);
  });
});

describe("injected messages on the stream", () => {
  it("keeps sender metadata through message_done, so a live injected message renders as one", () => {
    const sender = { chatId: "r1", chatTitle: "researcher", kind: "reply" };
    const injected = { ...msg("i1", "u1", "user", "hello from r1"), sender };
    const state = streamReducer(initialStream, {
      type: "frame",
      frame: { type: "message_done", runId: "run1", seq: 1, message: injected } as never,
    });
    expect(state.committed[0].sender).toEqual(sender);
    const { messages } = overlayThread([msg("u1", null, "user", "hi")], state);
    expect(messages.map((m) => m.sender ?? null)).toEqual([null, sender]);
  });
});
