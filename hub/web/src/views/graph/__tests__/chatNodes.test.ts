import { describe, expect, it } from "vitest";

import { attachChats } from "../chatNodes";
import { chatFixture, graphFixture } from "../fixtures";

describe("attachChats", () => {
  it("titles each node after its chat and keeps only records that have one", () => {
    const graph = graphFixture();
    const view = attachChats(graph, [chatFixture("a2", "the worker")]);
    expect(view.agents.map((a) => [a.id, a.name, a.chatId])).toEqual([["a2", "the worker", "c-a2"]]);
    // grants, edges and servers pass through untouched
    expect(view.edges).toBe(graph.edges);
    expect(view.grants).toBe(graph.grants);
  });

  it("shows an old record with several chats through its newest one, and names an untitled chat", () => {
    const view = attachChats(graphFixture(), [
      chatFixture("a1", "older", { id: "c1", updatedAt: "2026-01-01T00:00:00Z" }),
      chatFixture("a1", "", { id: "c2", updatedAt: "2026-02-01T00:00:00Z" }),
    ]);
    expect(view.agents).toHaveLength(1);
    expect(view.agents[0]).toMatchObject({ chatId: "c2", name: "Untitled chat" });
  });
});
