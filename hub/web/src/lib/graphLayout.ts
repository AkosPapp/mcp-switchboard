/**
 * Turning the /api/graph payload into something React Flow can draw.
 *
 * Kept free of React so the tree shape (children below parents, U13) and the
 * structural/communication edge split (U15) can be tested without a canvas.
 */

import dagre from "@dagrejs/dagre";

import type { Edge, GraphAgent } from "../api/types";

export const NODE_WIDTH = 240;
// Tall enough for the client badge row (host/project/environment chips) on top
// of the existing status/model/token rows.
export const NODE_HEIGHT = 160;

export interface LayoutNode<T extends GraphAgent = GraphAgent> {
  id: string;
  x: number;
  y: number;
  agent: T;
}

export interface LayoutEdge {
  id: string;
  source: string;
  target: string;
  /** `structural` is the parent-to-child tie: always on, never deletable. */
  kind: "structural" | "communication";
  allowed: boolean;
}

export interface GraphLayout<T extends GraphAgent = GraphAgent> {
  nodes: LayoutNode<T>[];
  edges: LayoutEdge[];
}

export function edgeId(from: string, to: string): string {
  return `${from}->${to}`;
}

/**
 * Nodes are placed by a top-to-bottom tree layout over the structural edges
 * only. Communication edges may form cycles (A9), which a layered layout would
 * have to break, and they say nothing about hierarchy.
 */
export function layoutGraph<T extends GraphAgent>(agents: readonly T[], edges: readonly Edge[]): GraphLayout<T> {
  const live = agents.filter((a) => a.deletedAt === null);
  const ids = new Set(live.map((a) => a.id));

  const g = new dagre.graphlib.Graph();
  g.setGraph({ rankdir: "TB", nodesep: 32, ranksep: 72, marginx: 16, marginy: 16 });
  g.setDefaultEdgeLabel(() => ({}));
  for (const agent of live) g.setNode(agent.id, { width: NODE_WIDTH, height: NODE_HEIGHT });

  const structuralPairs = new Set<string>();
  const out: LayoutEdge[] = [];
  for (const agent of live) {
    if (agent.parentId !== null && ids.has(agent.parentId)) {
      g.setEdge(agent.parentId, agent.id);
      structuralPairs.add(edgeId(agent.parentId, agent.id));
      out.push({
        id: edgeId(agent.parentId, agent.id),
        source: agent.parentId,
        target: agent.id,
        kind: "structural",
        allowed: true,
      });
    }
  }

  dagre.layout(g);

  for (const edge of edges) {
    if (!ids.has(edge.fromAgentId) || !ids.has(edge.toAgentId)) continue;
    if (edge.fromAgentId === edge.toAgentId) continue;
    // The parent->child row (A8) is the structural tie already drawn; a second
    // line on top of it would be a control that toggles nothing visible.
    if (structuralPairs.has(edgeId(edge.fromAgentId, edge.toAgentId))) continue;
    out.push({
      id: edgeId(edge.fromAgentId, edge.toAgentId),
      source: edge.fromAgentId,
      target: edge.toAgentId,
      kind: "communication",
      allowed: edge.allowed,
    });
  }

  const nodes = live.map((agent) => {
    const p = g.node(agent.id);
    // dagre reports centres; React Flow positions by the top-left corner.
    return { id: agent.id, x: p.x - NODE_WIDTH / 2, y: p.y - NODE_HEIGHT / 2, agent };
  });

  return { nodes, edges: out };
}

/** "3s", "4m", "2h", "5d" - the node is glanced at, not read. */
export function formatAge(iso: string, now: number = Date.now()): string {
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return "";
  const seconds = Math.max(0, Math.round((now - then) / 1000));
  if (seconds < 5) return "now";
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return `${hours}h`;
  return `${Math.floor(hours / 24)}d`;
}

export function formatCost(micros: number): string {
  const dollars = micros / 1_000_000;
  if (dollars === 0) return "$0";
  if (dollars < 0.01) return `$${dollars.toFixed(4)}`;
  return `$${dollars.toFixed(2)}`;
}

export function formatTokens(n: number): string {
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toFixed(n < 10_000 ? 1 : 0)}k`;
  return `${(n / 1_000_000).toFixed(1)}M`;
}

export function modelLabel(model: { provider?: string; model?: string }): string {
  return model.model ?? model.provider ?? "no model";
}

/** Ids on the path from a root down to `id`, for the depth breadcrumb. */
export function ancestry<T extends GraphAgent>(agents: readonly T[], id: string): T[] {
  const byId = new Map(agents.map((a) => [a.id, a]));
  const chain: T[] = [];
  let current = byId.get(id);
  while (current) {
    chain.unshift(current);
    current = current.parentId ? byId.get(current.parentId) : undefined;
  }
  return chain;
}
