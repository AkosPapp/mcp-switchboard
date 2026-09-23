/**
 * Which chats the Graph view shows (spec.md U31-U33). Each node is a chat's
 * execution record (a GraphAgent on the wire).
 *
 * A conversation is a tree: a root chat and all its sub-chats. Every chat
 * makes its own root, so showing everything buries the live work under old
 * idle trees. Pure and React-free so it can be table-tested.
 */

import type { GraphAgent } from "../api/types";

const ACTIVE = new Set(["running", "waiting", "blocked"]);

export function isActive(agent: Pick<GraphAgent, "status">): boolean {
  return ACTIVE.has(agent.status);
}

export interface VisibleAgents<T extends GraphAgent = GraphAgent> {
  /** Live nodes to draw, in input order. */
  agents: T[];
  /** Trees with at least one active agent. */
  runningTrees: number;
  /** Live agents left out by the filter. */
  hiddenAgents: number;
  /** The selected agent's tree is shown although nothing in it is active. */
  selectedKeptIdle: boolean;
}

function rootOf(id: string, byId: Map<string, GraphAgent>): string {
  const seen = new Set<string>();
  let current = id;
  for (;;) {
    seen.add(current);
    const parent = byId.get(current)?.parentId;
    // A missing or soft-deleted parent makes this agent the top of what is shown.
    if (!parent || !byId.has(parent) || seen.has(parent)) return current;
    current = parent;
  }
}

export function visibleAgents<T extends GraphAgent>(
  agents: readonly T[],
  selectedId: string | undefined | null,
  runningOnly: boolean,
): VisibleAgents<T> {
  const live = agents.filter((a) => a.deletedAt === null);
  const byId = new Map(live.map((a) => [a.id, a]));
  const roots = new Map(live.map((a) => [a.id, rootOf(a.id, byId)]));

  const activeRoots = new Set<string>();
  for (const a of live) if (isActive(a)) activeRoots.add(roots.get(a.id)!);
  const selectedRoot = selectedId && byId.has(selectedId) ? roots.get(selectedId)! : undefined;

  if (!runningOnly) {
    return {
      agents: live,
      runningTrees: activeRoots.size,
      hiddenAgents: 0,
      selectedKeptIdle: false,
    };
  }
  const shown = live.filter((a) => {
    const r = roots.get(a.id)!;
    return activeRoots.has(r) || r === selectedRoot;
  });
  return {
    agents: shown,
    runningTrees: activeRoots.size,
    hiddenAgents: live.length - shown.length,
    selectedKeptIdle: selectedRoot !== undefined && !activeRoots.has(selectedRoot),
  };
}
