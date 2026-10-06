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

/** What model produced a message (the group split signal, item 9). */
function modelOf(m: Message): string {
  return m.model?.provider || m.model?.model ? `${m.model.provider ?? ""}/${m.model.model ?? ""}` : "";
}

/**
 * Collapse a run's consecutive same-model assistant messages into one turn
 * node ("don't create a new child for every mcp call, just when I change a
 * prompt or switch a model", docs/improvements.md I9). Inside one run the
 * model answers, calls tools, and answers again — in the overview that must
 * read as one turn. The node is keyed by the group's LAST message, so
 * clicking it lands on the newest part of the run.
 */
interface Turn {
  ids: string[];
  role: "user" | "assistant";
  model: string;
  first: Message;
  last: Message;
}

function groupTurns(kept: Message[], shownParent: (m: Message) => string | null, byId: Map<string, Message>): Turn[] {
  const turns: Turn[] = [];
  const turnByMsg = new Map<string, Turn>();
  const childrenOf = new Map<string, number>();
  for (const m of kept) {
    const parent = turnByMsg.get(m.parentId ?? "") ?? null;
    const sameRunAsst =
      m.role === "assistant" &&
      parent !== null &&
      parent.role === "assistant" &&
      parent.model === modelOf(m) &&
      (childrenOf.get(parent.last.id) ?? 0) === 0 &&
      m.parentId === parent.last.id;
    if (sameRunAsst && parent) {
      parent.ids.push(m.id);
      parent.last = m;
      turnByMsg.set(m.id, parent);
    } else {
      const t: Turn = { ids: [m.id], role: m.role as "user" | "assistant", model: modelOf(m), first: m, last: m };
      turns.push(t);
      turnByMsg.set(m.id, t);
    }
    const parentMsg = shownParent(m);
    if (parentMsg) childrenOf.set(parentMsg, (childrenOf.get(parentMsg) ?? 0) + 1);
  }
  void byId;
  return turns;
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
  const turns = groupTurns(kept, shownParent, tree.byId);
  const turnOf = new Map<string, Turn>();
  for (const t of turns) for (const id of t.ids) turnOf.set(id, t);

  const g = new dagre.graphlib.Graph();
  g.setGraph({ rankdir: "TB", nodesep: 16, ranksep: 28, marginx: 8, marginy: 8 });
  g.setDefaultEdgeLabel(() => ({}));
  for (const t of turns) g.setNode(t.last.id, { width: OVERVIEW_NODE_WIDTH, height: OVERVIEW_NODE_HEIGHT });

  // One edge per turn pair, following the message DAG (deterministically).
  const edges: OverviewEdge[] = [];
  const seenEdge = new Set<string>();
  for (const m of kept) {
    const parentMsg = shownParent(m);
    if (parentMsg === null) continue;
    const from = turnOf.get(parentMsg)!;
    const to = turnOf.get(m.id)!;
    if (from === to) continue;
    const key = `${from.last.id}->${to.last.id}`;
    if (seenEdge.has(key)) continue;
    seenEdge.add(key);
    g.setEdge(from.last.id, to.last.id);
    const onPath = active.has(from.last.id) || active.has(from.ids[from.ids.length - 1]) || active.has(to.last.id) || active.has(m.id);
    edges.push({ id: key, source: from.last.id, target: to.last.id, active: onPath && active.has(m.id) });
  }
  dagre.layout(g);

  // The leaf the thread ends at may be a hidden tool message; the last shown
  // message on the active path is what to mark.
  const shownActive = kept.filter((m) => active.has(m.id));
  const lastShown = shownActive.length ? pathTo(tree, activeLeafId).filter(shown).pop()?.id : undefined;
  const leafTurn = lastShown ? turnOf.get(lastShown) : undefined;

  const nodes = turns.map((t): OverviewNode => {
    const p = g.node(t.last.id);
    const text = snippetOf(t.first);
    return {
      id: t.last.id,
      x: p.x - OVERVIEW_NODE_WIDTH / 2,
      y: p.y - OVERVIEW_NODE_HEIGHT / 2,
      role: t.role,
      label: t.role === "user" ? "You" : (t.first.model?.model ?? "assistant"),
      snippet: t.role === "assistant" && t.ids.length > 1 ? `${text} · ${t.ids.length} steps` : text,
      active: t.ids.some((id) => active.has(id)),
      leaf: leafTurn === t,
    };
  });
  return { nodes, edges };
}
