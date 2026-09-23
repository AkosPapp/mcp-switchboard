import { describe, expect, it } from "vitest";

import { resourceKeys } from "../../../api/resources";
import { graphKeys } from "../../graph/hooks";
import { chatKeys } from "../api";

const flat = (k: readonly unknown[]) => JSON.stringify(k);

describe("query keys", () => {
  it("never cache two different shapes under one key across modules", () => {
    const all = [
      ...Object.values(resourceKeys).map((k) => (typeof k === "function" ? k("x") : k)),
      ...Object.values(graphKeys),
      chatKeys.models,
      chatKeys.chat("x"),
      chatKeys.chatTools("x"),
      chatKeys.systemPrompt("x"),
    ].map((k) => flat(k as readonly unknown[]));
    expect(new Set(all).size).toBe(all.length);
  });

  it("sit under their resource root so the global feed's prefix invalidation reaches them", () => {
    expect(resourceKeys.agents[0]).toBe("agents");
    expect(resourceKeys.agent("a")[0]).toBe("agents");
    expect(resourceKeys.profiles[0]).toBe("profiles");
    expect(resourceKeys.profileModels[0]).toBe("profiles");
  });

  it("has one list key per resource: the chat module no longer defines its own", () => {
    expect(Object.keys(chatKeys)).not.toContain("profiles");
    expect(Object.keys(chatKeys)).not.toContain("agents");
  });
});
