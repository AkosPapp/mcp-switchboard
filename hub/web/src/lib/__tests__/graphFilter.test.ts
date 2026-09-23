import { describe, expect, it } from "vitest";

import type { AgentStatus } from "../../api/types";
import { agentFixture } from "../../views/graph/fixtures";
import { visibleAgents } from "../graphFilter";

const a = (id: string, parentId: string | null, status: AgentStatus, extra = {}) =>
  agentFixture({ id, parentId, status, ...extra });

const ids = (r: ReturnType<typeof visibleAgents>) => r.agents.map((x) => x.id);

describe("visibleAgents", () => {
  const cases: {
    name: string;
    agents: ReturnType<typeof a>[];
    selected?: string;
    runningOnly?: boolean;
    shown: string[];
    hidden: number;
    running: number;
  }[] = [
    { name: "idle tree is hidden", agents: [a("r", null, "idle"), a("c", "r", "done")], shown: [], hidden: 2, running: 0 },
    {
      name: "a running child shows its whole tree",
      agents: [a("r", null, "idle"), a("c", "r", "idle"), a("g", "c", "running"), a("o", null, "idle")],
      shown: ["r", "c", "g"],
      hidden: 1,
      running: 1,
    },
    { name: "waiting counts as active", agents: [a("r", null, "waiting"), a("x", null, "error")], shown: ["r"], hidden: 1, running: 1 },
    { name: "blocked counts as active", agents: [a("r", null, "idle"), a("c", "r", "blocked")], shown: ["r", "c"], hidden: 0, running: 1 },
    {
      name: "selected idle tree is kept",
      agents: [a("r", null, "idle"), a("c", "r", "idle"), a("o", null, "idle")],
      selected: "c",
      shown: ["r", "c"],
      hidden: 1,
      running: 0,
    },
    {
      name: "soft-deleted agents are ignored, even running ones",
      agents: [a("r", null, "idle"), a("c", "r", "running", { deletedAt: "2026-01-01" })],
      shown: [],
      hidden: 1,
      running: 0,
    },
    {
      name: "a deleted selection keeps nothing",
      agents: [a("r", null, "idle", { deletedAt: "2026-01-01" }), a("o", null, "idle")],
      selected: "r",
      shown: [],
      hidden: 1,
      running: 0,
    },
    {
      name: "All agents shows everything",
      agents: [a("r", null, "idle"), a("s", null, "running")],
      runningOnly: false,
      shown: ["r", "s"],
      hidden: 0,
      running: 1,
    },
  ];
  for (const c of cases) {
    it(c.name, () => {
      const r = visibleAgents(c.agents, c.selected, c.runningOnly ?? true);
      expect(ids(r)).toEqual(c.shown);
      expect(r.hiddenAgents).toBe(c.hidden);
      expect(r.runningTrees).toBe(c.running);
    });
  }

  it("flags a selected tree kept only by the selection", () => {
    const list = [a("r", null, "idle")];
    expect(visibleAgents(list, "r", true).selectedKeptIdle).toBe(true);
    expect(visibleAgents([a("r", null, "running")], "r", true).selectedKeptIdle).toBe(false);
  });
});
