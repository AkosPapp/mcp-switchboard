import { vi } from "vitest";

import type { ConnectionInfo, GraphView } from "../../../api/types";
import { chatsFor } from "../fixtures";

export interface Call {
  method: string;
  path: string;
  body: unknown;
}

/** A hub that serves a fixed graph and records every write. */
export function stubGraphHub(
  graph: GraphView,
  models = [{ provider: "anthropic", model: "sonnet" }, { provider: "anthropic", model: "haiku" }],
  connections: ConnectionInfo[] = [],
) {
  const calls: Call[] = [];
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const path = String(input).replace(/^api\//, "").split("?")[0];
    const method = init?.method ?? "GET";
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    calls.push({ method, path, body });
    let payload: unknown;
    let status = 200;
    if (path === "graph") payload = graph;
    else if (path === "chats" && method === "GET") payload = { chats: chatsFor(graph), limit: 500, offset: 0 };
    else if (path === "models") payload = { models };
    else if (path === "connections") payload = { connections };
    else if (method === "PUT" && path.endsWith("/grants")) payload = { grants: [], revoked: [] };
    else if (method === "PUT") payload = {};
    else if (method === "PATCH") payload = {};
    else if (method === "POST" && path === "chats") {
      payload = { id: "cnew", agentId: "new", title: body.title ?? "New chat" };
      status = 201;
    } else if (method === "DELETE") {
      return { ok: true, status: 200, statusText: "", json: async () => ({ deletedChats: 2 }) } as unknown as Response;
    } else throw new Error(`no fixture for ${method} ${path}`);
    return { ok: true, status, statusText: "OK", json: async () => payload } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetchMock);
  return { calls, fetchMock };
}

export function connectionFixture(overrides: Partial<ConnectionInfo> = {}): ConnectionInfo {
  return {
    id: "conn-1",
    label: "laptop",
    client: {
      name: "mcp-switchboard-client",
      version: "1.0.0",
      instance: "i1",
      label: "laptop",
      environment: { kinds: ["direnv"], project: "agent-reverse-proxy" },
    },
    connectedAt: "2026-09-20T10:00:00.000000+00:00",
    servers: [],
    ...overrides,
  };
}
