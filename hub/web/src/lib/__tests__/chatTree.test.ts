import { describe, expect, it } from "vitest";

import type { Chat } from "../../views/chat/types";
import { ancestorsOf, buildChatTree, groupId, isLegacy, subtreeIds } from "../chatTree";

const chat = (id: string, over: Partial<Chat> = {}): Chat => ({
  id, agentId: `a-${id}`, peerAgentId: null, title: id, kind: "human", activeLeafId: null, tags: [], tokenTotal: 0,
  costTotalMicros: 0, createdAt: "", updatedAt: "2026-01-01T00:00:00Z", archivedAt: null, clientLabel: "box", parentChatId: null, ...over,
});

describe("buildChatTree", () => {
  it("groups by client (no client last), roots newest first, children nested recursively", () => {
    const groups = buildChatTree([
      chat("old", { updatedAt: "2026-01-01T00:00:00Z" }),
      chat("new", { updatedAt: "2026-01-05T00:00:00Z" }),
      chat("kid", { parentChatId: "old", updatedAt: "2026-01-09T00:00:00Z" }),
      chat("grand", { parentChatId: "kid" }),
      chat("free", { clientLabel: null }),
      chat("acme", { clientLabel: "acme" }),
    ]);
    expect(groups.map((g) => g.id)).toEqual([groupId("acme"), groupId("box"), groupId(null)]);
    const box = groups[1];
    // a child's newer activity lifts its whole root above a chat that was busier itself
    expect(box.roots.map((n) => n.chat.id)).toEqual(["old", "new"]);
    expect(box.roots[0].children[0].children[0].chat.id).toBe("grand");
    expect(box.roots[0].children[0].children[0].depth).toBe(2);
    expect(box.chatIds).toEqual(["old", "kid", "grand", "new"]);
  });

  it("treats an unknown parent, a self-parent and a cycle as roots so nothing is lost", () => {
    const groups = buildChatTree([
      chat("a", { parentChatId: "missing" }),
      chat("b", { parentChatId: "b" }),
      chat("x", { parentChatId: "y" }),
      chat("y", { parentChatId: "x" }),
    ]);
    const ids = groups[0].chatIds.slice().sort();
    expect(ids).toEqual(["a", "b", "x", "y"]);
  });

  it("recognises legacy peer chats, which stay ordinary rows", () => {
    expect(isLegacy(chat("p", { kind: "agent" }))).toBe(true);
    expect(isLegacy(chat("p", { kind: "spawn", peerAgentId: "a9" }))).toBe(true);
    expect(isLegacy(chat("p", { kind: "spawn", parentChatId: "z" }))).toBe(false);
    expect(buildChatTree([chat("p", { kind: "agent" })])[0].roots).toHaveLength(1);
  });
});

describe("paths and subtrees", () => {
  const chats = [chat("r"), chat("k", { parentChatId: "r" }), chat("g", { parentChatId: "k" }), chat("other")];
  it("names the group and every ancestor chat that must be open", () => {
    const groups = buildChatTree(chats);
    expect(ancestorsOf(groups, "g")).toEqual([groupId("box"), "r", "k"]);
    expect(ancestorsOf(groups, "r")).toEqual([groupId("box")]);
    expect(ancestorsOf(groups, "nope")).toBeNull();
  });
  it("lists a chat with everything nested under it", () => {
    expect(subtreeIds(chats, "r")).toEqual(["r", "k", "g"]);
    expect(subtreeIds(chats, "g")).toEqual(["g"]);
  });
});
