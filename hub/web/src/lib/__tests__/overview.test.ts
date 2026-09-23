import { describe, expect, it } from "vitest";

import { buildOverview } from "../overview";
import type { Message } from "../../views/chat/types";

const msg = (id: string, parentId: string | null, role: Message["role"], text = id, over: Partial<Message> = {}): Message =>
  ({
    id, chatId: "c", parentId, role, content: [{ type: "text", text }], toolCalls: null, toolResults: null,
    tokenInput: 0, tokenOutput: 0, costMicros: 0, latencyMs: 0, model: null, finishReason: null, runId: null,
    lastActiveChildId: null, createdAt: "", ...over,
  }) as Message;

describe("buildOverview", () => {
  const tree = [
    msg("u1", null, "user"),
    msg("a1", "u1", "assistant", "first", { model: { provider: "p", model: "m1" } }),
    msg("a2", "u1", "assistant", "regenerated", { model: { provider: "p", model: "m2" } }),
    msg("u2", "a2", "user"),
  ];

  it("draws every branch and marks the active path", () => {
    const o = buildOverview(tree, "u2");
    expect(o.nodes.map((n) => n.id).sort()).toEqual(["a1", "a2", "u1", "u2"]);
    const byId = Object.fromEntries(o.nodes.map((n) => [n.id, n]));
    expect(byId.a1.active).toBe(false);
    expect(byId.a2.active).toBe(true);
    expect(byId.u2.leaf).toBe(true);
    expect(byId.a1.label).toBe("m1");
    expect(byId.u1.label).toBe("You");
    expect(o.edges.find((e) => e.target === "a1")!.active).toBe(false);
    expect(o.edges.find((e) => e.target === "a2")!.active).toBe(true);
    // siblings sit side by side, children below their parent
    expect(byId.a1.y).toBe(byId.a2.y);
    expect(byId.a1.x).not.toBe(byId.a2.x);
    expect(byId.u2.y).toBeGreaterThan(byId.a2.y);
  });

  it("folds tool messages away without breaking the chain", () => {
    const withTools = [
      msg("u1", null, "user"),
      msg("a1", "u1", "assistant", "", { toolCalls: [{ id: "t", name: "x", arguments: {} }] as never }),
      msg("t1", "a1", "tool"),
      msg("a2", "t1", "assistant", "done"),
    ];
    const o = buildOverview(withTools, "a2");
    expect(o.nodes.map((n) => n.id)).toEqual(["u1", "a1", "a2"]);
    expect(o.edges.map((e) => e.id)).toEqual(["u1->a1", "a1->a2"]);
    expect(o.nodes.find((n) => n.id === "a1")!.snippet).toBe("used tools");
  });
});
