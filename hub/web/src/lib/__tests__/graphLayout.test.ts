import { describe, expect, it } from "vitest";

import { agentFixture, edgeFixture } from "../../views/graph/fixtures";
import { ancestry, formatAge, formatCost, formatTokens, layoutGraph } from "../graphLayout";

const agents = [
  agentFixture({ id: "r", name: "root" }),
  agentFixture({ id: "c1", parentId: "r", name: "c1", depth: 1 }),
  agentFixture({ id: "c2", parentId: "r", name: "c2", depth: 1 }),
  agentFixture({ id: "g", parentId: "c1", name: "g", depth: 2 }),
];

describe("layoutGraph", () => {
  it("places children below their parents", () => {
    const { nodes } = layoutGraph(agents, []);
    const y = Object.fromEntries(nodes.map((n) => [n.id, n.y]));
    expect(y.c1).toBeGreaterThan(y.r);
    expect(y.c2).toBe(y.c1);
    expect(y.g).toBeGreaterThan(y.c1);
  });

  it("draws the parent tie as structural and hides its edge row", () => {
    const { edges } = layoutGraph(agents, [
      edgeFixture("r", "c1"),
      edgeFixture("c1", "c2", true),
      edgeFixture("c2", "c1", false),
    ]);
    const structural = edges.filter((e) => e.kind === "structural");
    expect(structural.map((e) => e.id).sort()).toEqual(["c1->g", "r->c1", "r->c2"]);
    const comm = edges.filter((e) => e.kind === "communication");
    expect(comm.map((e) => [e.id, e.allowed])).toEqual([
      ["c1->c2", true],
      ["c2->c1", false],
    ]);
  });

  it("ignores deleted agents, their edges, and self edges; tolerates cycles", () => {
    const withDeleted = [...agents, agentFixture({ id: "x", parentId: "r", deletedAt: "2026-01-01" })];
    const { nodes, edges } = layoutGraph(withDeleted, [
      edgeFixture("r", "x"),
      edgeFixture("c1", "c1"),
      edgeFixture("c1", "c2"),
      edgeFixture("c2", "c1"),
    ]);
    expect(nodes.map((n) => n.id)).not.toContain("x");
    expect(edges.filter((e) => e.kind === "communication")).toHaveLength(2);
  });

  it("lays out a forest of roots", () => {
    const { nodes } = layoutGraph([agentFixture({ id: "a" }), agentFixture({ id: "b" })], []);
    expect(nodes[0].x).not.toBe(nodes[1].x);
  });
});

describe("formatters", () => {
  it("formats age, cost and tokens", () => {
    const now = Date.parse("2026-09-20T10:10:00Z");
    expect(formatAge("2026-09-20T10:09:58Z", now)).toBe("now");
    expect(formatAge("2026-09-20T10:09:00Z", now)).toBe("1m");
    expect(formatAge("2026-09-20T07:10:00Z", now)).toBe("3h");
    expect(formatCost(0)).toBe("$0");
    expect(formatCost(1_500_000)).toBe("$1.50");
    expect(formatCost(2500)).toBe("$0.0025");
    expect(formatTokens(999)).toBe("999");
    expect(formatTokens(12_400)).toBe("12k");
  });
  it("walks ancestry root first", () => {
    expect(ancestry(agents, "g").map((a) => a.id)).toEqual(["r", "c1", "g"]);
  });
});
