import type { Chat } from "../chat/types";
import { attachChats, type GraphChatView } from "./chatNodes";
import type {
  Edge,
  GraphAgent,
  GraphGrant,
  GraphServer,
  GraphView,
} from "../../api/types";

const TS = "2026-09-20T10:00:00.000000+00:00";

export function agentFixture(overrides: Partial<GraphAgent> = {}): GraphAgent {
  return {
    id: "a1",
    parentId: null,
    name: "root",
    description: "",
    project: null,
    model: { provider: "anthropic", model: "sonnet" },
    systemPrompt: "",
    depth: 0,
    status: "idle",
    budget: {},
    capabilities: { canSpawn: true, canMessage: true },
    approval: "never",
    autoWake: false,
    tokenTotal: 0,
    costTotalMicros: 0,
    createdAt: TS,
    updatedAt: TS,
    lastActivityAt: TS,
    deletedAt: null,
    unreadMail: 0,
    ...overrides,
  };
}

export function grantFixture(overrides: Partial<GraphGrant> = {}): GraphGrant {
  return {
    agentId: "a1",
    label: "laptop",
    project: "",
    server: "files",
    allowed: true,
    source: "inherited",
    createdAt: TS,
    updatedAt: TS,
    connected: true,
    ...overrides,
  };
}

export function edgeFixture(from: string, to: string, allowed = true): Edge {
  return { fromAgentId: from, toAgentId: to, allowed, createdAt: TS, updatedAt: TS };
}

export function serverFixture(overrides: Partial<GraphServer> = {}): GraphServer {
  return { label: "laptop", project: "", server: "files", connected: true, toolCount: 3, ...overrides };
}

/** root a1 -> child a2 (depth 1); root holds files+git, child only files. */
export function graphFixture(overrides: Partial<GraphView> = {}): GraphView {
  return {
    agents: [
      agentFixture({ id: "a1", name: "root" }),
      agentFixture({ id: "a2", parentId: "a1", name: "worker", depth: 1 }),
    ],
    edges: [edgeFixture("a1", "a2")],
    grants: [
      grantFixture({ agentId: "a1", server: "files", source: "explicit" }),
      grantFixture({ agentId: "a1", server: "git", source: "explicit" }),
      grantFixture({ agentId: "a2", server: "files", source: "inherited" }),
    ],
    servers: [serverFixture(), serverFixture({ server: "git" })],
    ...overrides,
  };
}

export function chatFixture(agentId: string, title: string, overrides: Partial<Chat> = {}): Chat {
  return {
    id: `c-${agentId}`,
    agentId,
    peerAgentId: null,
    title,
    kind: "human",
    activeLeafId: null,
    tags: [],
    tokenTotal: 0,
    costTotalMicros: 0,
    createdAt: TS,
    updatedAt: TS,
    archivedAt: null,
    ...overrides,
  };
}

/** One chat per record, titled after it: the shape the Graph draws (docs/CHAT_MODEL_API.md). */
export function chatsFor(graph: GraphView): Chat[] {
  return graph.agents.map((a) => chatFixture(a.id, a.name));
}

export function chatGraphFixture(overrides: Partial<GraphView> = {}): GraphChatView {
  const graph = graphFixture(overrides);
  return attachChats(graph, chatsFor(graph));
}
