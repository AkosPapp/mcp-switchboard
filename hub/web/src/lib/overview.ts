/**
 * The chat "Overview": the whole message tree as a small node graph, with the
 * path the thread is showing highlighted (like OpenWebUI's chat overview).
 *
 * Only what a reader would call a turn is drawn: user and assistant messages.
 * Tool results and system messages are folded away, so a turn that used tools
 * still hangs off the message before it. Free of React so it can be tested.
 */

import dagre from "@dagrejs/dagre";

import { indexTree, pathTo } from "./dag";
import type { Message } from "../views/chat/types";

export const OVERVIEW_NODE_WIDTH = 200;
export const OVERVIEW_NODE_HEIGHT = 52;

export interface OverviewNode {
  id: string;
  x: number;
  y: number;
  role: "user" | "assistant";
  label: string;
  snippet: string;
  /** on the path the thread is showing */
  active: boolean;
  /** the message the thread ends at */
  leaf: boolean;
}

export interface OverviewEdge {
  id: string;
  source: string;
  target: string;
  active: boolean;
}

export interface Overview {
  nodes: OverviewNode[];
  edges: OverviewEdge[];
}

const SNIPPET_MAX = 90;

function snippetOf(m: Message): string {
  const text = m.content
    .filter((b) => b.type === "text" && typeof b.text === "string")
    .map((b) => b.text as string)
    .join(" ")
    .replace(/\s+/g, " ")
    .trim();
  if (!text) return m.toolCalls?.length ? "used tools" : "(empty)";
  return text.length > SNIPPET_MAX ? `${text.slice(0, SNIPPET_MAX)}…` : text;
}

export function buildOverview(messages: readonly Message[], activeLeafId: string | null): Overview {
  const tree = indexTree([...messages]);
  const shown = (m: Message) => m.role === "user" || m.role === "assistant";
  const active = new Set(pathTo(tree, activeLeafId).map((m) => m.id));

  /** nearest shown ancestor, so a hidden tool message does not break the chain */
  const shownParent = (m: Message): string | null => {
    const seen = new Set<string>();
    let cursor = m.parentId !== null ? tree.byId.get(m.parentId) : undefined;
    while (cursor && !seen.has(cursor.id)) {
      if (shown(cursor)) return cursor.id;
      seen.add(cursor.id);
      cursor = cursor.parentId !== null ? tree.byId.get(cursor.parentId) : undefined;
    }
    return null;
  };

  const kept = messages.filter(shown);
  const g = new dagre.graphlib.Graph();
  g.setGraph({ rankdir: "TB", nodesep: 16, ranksep: 28, marginx: 8, marginy: 8 });
  g.setDefaultEdgeLabel(() => ({}));
  for (const m of kept) g.setNode(m.id, { width: OVERVIEW_NODE_WIDTH, height: OVERVIEW_NODE_HEIGHT });

  const edges: OverviewEdge[] = [];
  for (const m of kept) {
    const parent = shownParent(m);
    if (parent === null) continue;
    g.setEdge(parent, m.id);
    edges.push({ id: `${parent}->${m.id}`, source: parent, target: m.id, active: active.has(parent) && active.has(m.id) });
  }
  dagre.layout(g);

  // The leaf the thread ends at may be a hidden tool message; the last shown
  // message on the active path is what to mark.
  const shownActive = kept.filter((m) => active.has(m.id));
  const leafId = shownActive.length ? pathTo(tree, activeLeafId).filter(shown).pop()?.id : undefined;

  const nodes = kept.map((m): OverviewNode => {
    const p = g.node(m.id);
    return {
      id: m.id,
      x: p.x - OVERVIEW_NODE_WIDTH / 2,
      y: p.y - OVERVIEW_NODE_HEIGHT / 2,
      role: m.role as "user" | "assistant",
      label: m.role === "user" ? "You" : (m.model?.model ?? "assistant"),
      snippet: snippetOf(m),
      active: active.has(m.id),
      leaf: m.id === leafId,
    };
  });
  return { nodes, edges };
}
