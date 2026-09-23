import { describe, expect, it } from "vitest";

import {
  applyToPath,
  deepestLeaf,
  indexTree,
  pathTo,
  resolveSiblings,
  siblingTarget,
  siblingsOf,
  type DagNode,
} from "../dag";

const n = (id: string, parentId: string | null, extra: Partial<DagNode> = {}): DagNode => ({
  id,
  parentId,
  ...extra,
});

// u1 -> a1 -> u2 -> a2
//   \-> a1b (regenerate of a1) -> u3
//        u2 also has sibling u2b
const all = [
  n("u1", null),
  n("a1", "u1"),
  n("u2", "a1"),
  n("a2", "u2"),
  n("a1b", "u1"),
  n("u3", "a1b"),
  n("u2b", "a1"),
];

describe("dag", () => {
  const tree = indexTree(all);

  it("walks a leaf to the root, root first", () => {
    expect(pathTo(tree, "a2").map((m) => m.id)).toEqual(["u1", "a1", "u2", "a2"]);
    expect(pathTo(tree, "u3").map((m) => m.id)).toEqual(["u1", "a1b", "u3"]);
    expect(pathTo(tree, "nope")).toEqual([]);
  });

  it("survives a cycle", () => {
    const cyc = indexTree([n("x", "y"), n("y", "x")]);
    expect(pathTo(cyc, "x").length).toBeLessThanOrEqual(2);
  });

  it("computes siblings in creation order", () => {
    expect(siblingsOf(tree, "a1b")).toEqual({ ids: ["a1", "a1b"], index: 1 });
    expect(siblingsOf(tree, "u2")).toEqual({ ids: ["u2", "u2b"], index: 0 });
    expect(siblingsOf(tree, "u1")).toEqual({ ids: ["u1"], index: 0 });
  });

  it("prefers the hub's siblings over computing", () => {
    const m = n("a1", "u1", { siblings: { ids: ["a1", "zz"], index: 0 } });
    expect(resolveSiblings(m, tree).ids).toEqual(["a1", "zz"]);
    expect(resolveSiblings(n("q", null)).ids).toEqual(["q"]);
  });

  it("steps to a neighbour and stops at the ends", () => {
    const s = { ids: ["a", "b", "c"], index: 1 };
    expect(siblingTarget(s, -1)).toBe("a");
    expect(siblingTarget(s, 1)).toBe("c");
    expect(siblingTarget({ ids: ["a", "b"], index: 0 }, -1)).toBeNull();
    expect(siblingTarget({ ids: ["a", "b"], index: 1 }, 1)).toBeNull();
  });

  it("finds the deepest leaf, following lastActiveChildId then newest", () => {
    expect(deepestLeaf(tree, "u1")).toBe("u3"); // newest child a1b -> u3
    const t2 = indexTree([
      n("u1", null, { lastActiveChildId: "a1" }),
      n("a1", "u1", { lastActiveChildId: "u2" }),
      n("u2", "a1"),
      n("u2b", "a1"),
      n("a1b", "u1"),
    ]);
    expect(deepestLeaf(t2, "u1")).toBe("u2");
    expect(deepestLeaf(t2, "a1b")).toBe("a1b");
  });

  describe("applyToPath", () => {
    const path = [n("u1", null), n("a1", "u1")];
    it("replaces by id", () => {
      const out = applyToPath(path, { ...n("a1", "u1"), siblings: { ids: ["a1"], index: 0 } });
      expect(out).toHaveLength(2);
      expect(out[1].siblings).toBeDefined();
    });
    it("appends under the last message", () => {
      expect(applyToPath(path, n("u2", "a1")).map((m) => m.id)).toEqual(["u1", "a1", "u2"]);
    });
    it("a regenerate sibling replaces the old answer in view", () => {
      expect(applyToPath(path, n("a1b", "u1")).map((m) => m.id)).toEqual(["u1", "a1b"]);
    });
    it("leaves the path alone when the parent is unknown", () => {
      expect(applyToPath(path, n("z", "missing"))).toBe(path);
    });
    it("places a root on an empty path", () => {
      expect(applyToPath([], n("r", null)).map((m) => m.id)).toEqual(["r"]);
    });
  });
});
