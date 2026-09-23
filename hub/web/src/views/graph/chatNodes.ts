/**
 * The Graph shows chats. The hub's graph is still made of execution records
 * (agents), one per chat since docs/CHAT_MODEL_API.md; this joins each record to
 * its chat so a node is titled and opened as the chat it is. A record with no
 * chat (an old hidden one) has nothing to show and is left out.
 */

import type { ConnectionInfo, GraphAgent, GraphView } from "../../api/types";
import type { Chat } from "../chat/types";

export interface GraphChat extends GraphAgent {
  /** the chat this node stands for; `name` is its title */
  chatId: string;
}

export interface GraphChatView extends Omit<GraphView, "agents"> {
  agents: GraphChat[];
}

const ts = (s: string) => {
  const n = Date.parse(s);
  return Number.isNaN(n) ? 0 : n;
};

export function attachChats(graph: GraphView, chats: readonly Chat[]): GraphChatView {
  // 1:1 for anything new; an old agent with several chats shows its newest.
  const byAgent = new Map<string, Chat>();
  for (const c of chats) {
    const have = byAgent.get(c.agentId);
    if (!have || ts(c.updatedAt) > ts(have.updatedAt)) byAgent.set(c.agentId, c);
  }
  const agents: GraphChat[] = [];
  for (const a of graph.agents) {
    const chat = byAgent.get(a.id);
    if (chat) agents.push({ ...a, name: chat.title || "Untitled chat", chatId: chat.id });
  }
  return { ...graph, agents };
}

/**
 * Live connections keyed by label, the same join ChatThread/ChatList already do
 * (a chat's `clientLabel` is only a name; the environment lives on the matching
 * connection, when one is still open).
 */
export function connectionsByLabel(connections: readonly ConnectionInfo[]): Map<string, ConnectionInfo> {
  return new Map(connections.map((c) => [c.label, c]));
}
