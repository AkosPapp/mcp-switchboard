import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { AttentionContext } from "../../../hooks/useAttention";
import { renderView } from "../../__tests__/helpers";
import ChatList from "../ChatList";
import ChatThread from "../ChatThread";

const ag = (id: string, over: Record<string, unknown> = {}) => ({
  id, parentId: null, name: id, model: null, status: "idle", budget: {}, deletedAt: null, createdAt: "2026-01-01T00:00:00Z",
  origin: "manual", clientLabel: "box", profileId: null, ...over,
});
const ch = (id: string, updatedAt: string, over: Record<string, unknown> = {}) => ({
  id, agentId: `ag-${id}`, peerAgentId: null, title: `chat-${id}`, kind: "human", activeLeafId: null, tags: [], tokenTotal: 0,
  costTotal: 0, costTotalMicros: 0, createdAt: "", updatedAt, archivedAt: null, clientLabel: "box", parentChatId: null, ...over,
});

function hub(routes: Record<string, unknown>) {
  vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const key = `${init?.method ?? "GET"} ${String(input).replace(/^api\//, "").split("?")[0]}`;
    if (!(key in routes)) return { ok: false, status: 404, statusText: "nf", json: async () => ({ detail: "nf" }) } as Response;
    return { ok: true, status: 200, statusText: "OK", json: async () => routes[key] } as Response;
  }));
}
beforeEach(() => localStorage.clear());
afterEach(() => vi.unstubAllGlobals());

describe("ChatList tree", () => {
  const routes = {
    "GET agents": { agents: [ag("ag-root", { status: "running" }), ag("ag-kid"), ag("ag-grandkid")] },
    "GET chats": {
      chats: [
        ch("root", "2026-01-01"),
        ch("kid", "2026-01-02", { parentChatId: "root" }),
        ch("grandkid", "2026-01-03", { parentChatId: "kid" }),
        ch("orphan", "2026-01-01", { parentChatId: "gone" }),
        ch("old", "2026-01-01", { kind: "agent", peerAgentId: "x" }),
        ch("solo", "2026-01-01", { clientLabel: null }),
      ],
      limit: 500,
      offset: 0,
    },
    "GET connections": {
      connections: [
        {
          id: "c", label: "box", connectedAt: "", servers: [],
          client: { name: "c", version: "1", instance: "i", label: "box", environment: { kinds: ["devcontainer"], project: "proj" } },
        },
      ],
    },
  };
  const list = () => renderView(<ChatList activeId={null} onNavigate={() => {}} onNewChat={() => {}} />);
  const row = (id: string) => screen.getByText(`chat-${id}`).closest('[data-testid="chat-node"]') as HTMLElement;

  it("nests child chats under their parent, one row per chat, with no agents anywhere", async () => {
    hub(routes);
    list();
    await screen.findByText("chat-root");
    const groups = screen.getAllByTestId("client-group");
    expect(groups.map((g) => g.getAttribute("data-client"))).toEqual(["box", ""]);
    await waitFor(() => expect(within(groups[0]).getByText("proj")).toBeTruthy());
    expect(within(groups[0]).queryByText("devcontainer")).toBeNull(); // chips stay in the tooltip
    expect(within(groups[1]).getByText("No client")).toBeTruthy();
    expect(within(groups[1]).getByText("chat-solo")).toBeTruthy();
    // depth: root 0 > kid 1 > grandkid 2, all inside the root's item
    expect(row("root").getAttribute("data-depth")).toBe("0");
    expect(row("kid").getAttribute("data-depth")).toBe("1");
    expect(row("grandkid").getAttribute("data-depth")).toBe("2");
    expect(row("root").contains(row("kid"))).toBe(true);
    expect(row("kid").contains(row("grandkid"))).toBe(true);
    // an unknown parent and a legacy peer chat are ordinary roots; the legacy one is tagged
    expect(row("orphan").getAttribute("data-depth")).toBe("0");
    expect(row("old").getAttribute("data-depth")).toBe("0");
    expect(within(row("old")).getByText("legacy")).toBeTruthy();
    expect(screen.queryByText(/Older chats/)).toBeNull();
    expect(screen.queryByText(/agent/i)).toBeNull();
    expect(within(groups[0]).getByTestId("group-count").textContent).toBe("5");
  });

  it("shows a long client label in full in the group header, not clipped to a few characters", async () => {
    hub({
      ...routes,
      "GET connections": {
        connections: [
          {
            id: "c",
            label: "agent-reverse-proxy-workspace",
            connectedAt: "",
            servers: [],
            client: { name: "c", version: "1", instance: "i", label: "agent-reverse-proxy-workspace" },
          },
        ],
      },
      "GET chats": { ...routes["GET chats"], chats: routes["GET chats"].chats.map((c) => ({ ...c, clientLabel: "agent-reverse-proxy-workspace" })) },
    });
    list();
    await screen.findByText("chat-root");
    const group = screen.getAllByTestId("client-group")[0];
    // the full label is present as text (not truncated to "agent-…"), and its
    // element does not carry the truncating class.
    const label = within(group).getByText("agent-reverse-proxy-workspace");
    expect(label.className).not.toContain("truncate");
  });

  it("collapses a chat's children, remembers it, and shows how many are hidden", async () => {
    hub(routes);
    list();
    await screen.findByText("chat-root");
    fireEvent.click(screen.getByRole("button", { name: "collapse sub-chats of chat-root" }));
    expect(screen.queryByText("chat-kid")).toBeNull();
    expect(screen.queryByText("chat-grandkid")).toBeNull();
    expect(within(row("root")).getByTestId("child-count").textContent).toBe("2");
    expect(JSON.parse(localStorage.getItem("mcpsb.ui.v1.chat.tree.collapsed")!).d).toEqual(["root"]);
    fireEvent.click(screen.getByRole("button", { name: "expand sub-chats of chat-root" }));
    expect(screen.getByText("chat-grandkid")).toBeTruthy();
  });

  it("starts collapsed from memory, but keeps the open chat's path open", async () => {
    localStorage.setItem("mcpsb.ui.v1.chat.tree.collapsed", JSON.stringify({ v: 1, d: ["client:box", "root", "kid"] }));
    hub(routes);
    const { unmount } = list();
    await screen.findAllByTestId("client-group");
    expect(screen.queryByText("chat-root")).toBeNull();
    unmount();
    renderView(<ChatList activeId="grandkid" onNavigate={() => {}} onNewChat={() => {}} />);
    await screen.findByText("chat-grandkid");
  });

  it("shows attention on a collapsed group and counts its chats", async () => {
    localStorage.setItem("mcpsb.ui.v1.chat.tree.collapsed", JSON.stringify({ v: 1, d: ["client:box"] }));
    hub(routes);
    const marks = { grandkid: { reason: "approval", key: "k", chatId: "grandkid", title: "t", body: "b" } };
    renderView(
      <AttentionContext.Provider value={{ marks: marks as never, count: 1, permission: "default", enableNotifications: async () => {} }}>
        <ChatList activeId={null} onNavigate={() => {}} onNewChat={() => {}} />
      </AttentionContext.Provider>,
    );
    const box = (await screen.findAllByTestId("client-group"))[0];
    expect(within(box).getByTestId("group-attention").textContent).toBe("1");
    expect(within(box).getByTestId("group-count").textContent).toBe("5");
  });

  it("a search opens every collapsed chat so the match is visible", async () => {
    localStorage.setItem("mcpsb.ui.v1.chat.tree.collapsed", JSON.stringify({ v: 1, d: ["client:box", "root", "kid"] }));
    localStorage.setItem("mcpsb.ui.v1.chat.list", JSON.stringify({ v: 1, d: { q: "chat", archived: false } }));
    hub({ ...routes, "GET chats": { chats: [ch("root", "2026-01-01"), ch("kid", "2026-01-02", { parentChatId: "root" })], limit: 500, offset: 0 } });
    list();
    await screen.findByText("chat-kid");
    expect(screen.getAllByTestId("chat-node").map((n) => n.getAttribute("data-chat"))).toEqual(["root", "kid"]);
  });

  it("marks rows that need attention", async () => {
    hub(routes);
    const marks = { kid: { reason: "approval", key: "k", chatId: "kid", title: "t", body: "b" } };
    renderView(
      <AttentionContext.Provider value={{ marks: marks as never, count: 1, permission: "default", enableNotifications: async () => {} }}>
        <ChatList activeId={null} onNavigate={() => {}} onNewChat={() => {}} />
      </AttentionContext.Provider>,
    );
    expect((await screen.findByTestId("attention-marker")).textContent).toBe("needs approval");
    expect(screen.getByText(/Enable notifications/)).toBeTruthy();
  });

  it("the status dot comes from the chat's run record", async () => {
    hub(routes);
    list();
    await screen.findByText("chat-root");
    await waitFor(() => expect(within(row("root")).getByTitle("running")).toBeTruthy());
  });

  it("delete arms with the sub-chat count and reports every chat that went", async () => {
    const calls: string[] = [];
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const key = `${init?.method ?? "GET"} ${String(input).replace(/^api\//, "").split("?")[0]}`;
      calls.push(key);
      const deleted = calls.includes("DELETE chats/root");
      const left = routes["GET chats"].chats.filter((c) => !deleted || !["root", "kid", "grandkid"].includes(c.id));
      const r: Record<string, unknown> = { ...routes, "GET chats": { chats: left, limit: 500, offset: 0 }, "DELETE chats/root": { deletedChats: 3 } };
      if (!(key in r)) return { ok: false, status: 404, statusText: "nf", json: async () => ({ detail: "nf" }) } as Response;
      return { ok: true, status: 200, statusText: "OK", json: async () => r[key] } as Response;
    }));
    list();
    await screen.findByText("chat-root");
    const own = within(row("root")).getAllByRole("button", { name: "Delete" })[0];
    fireEvent.click(own);
    expect(within(row("root")).getByText("Delete + 2 sub-chats?")).toBeTruthy();
    fireEvent.click(within(row("root")).getAllByRole("button", { name: "Confirm delete" })[0]);
    await waitFor(() => expect(calls).toContain("DELETE chats/root"));
    expect(await screen.findByText("3 chats deleted")).toBeTruthy();
    // its sub-chats left the list at once
    await waitFor(() => expect(screen.queryByText("chat-grandkid")).toBeNull());
  });
});

describe("ChatThread running tool and approval", () => {
  it("shows Running <tool> and the approval headline, and the approval vanishes on click", async () => {
    let decided = false;
    const msg = {
      id: "a1", chatId: "c1", parentId: null, role: "assistant", content: [{ type: "text", text: "on it" }],
      toolCalls: [{ id: "t1", name: "run_command", arguments: {} }, { id: "t2", name: "fetch__fetch", arguments: {} }, { id: "t3", name: "rm", arguments: { p: 1 } }],
      toolResults: null, tokenInput: 0, tokenOutput: 0, costMicros: 0, latencyMs: 0, model: null, finishReason: null,
      runId: "r1", lastActiveChildId: null, createdAt: "", siblings: { ids: ["a1"], index: 0 },
    };
    hub({
      "GET chats/c1": ch("c1", "2026-01-01", { agentId: "a1" }),
      "GET chats/c1/messages": { chatId: "c1", activeLeafId: "a1", tree: false, messages: [msg] },
      "GET agents/a1": ag("a1"),
      "GET models": { models: [] },
      "GET runs/r1": {
        id: "r1", agentId: "a1", chatId: "c1", status: "waiting", budgetSnapshot: {}, usage: {}, error: null, finishReason: null,
        pendingApprovals: decided ? [] : [{ callId: "t3", tool: "rm", arguments: { p: 1 }, expiresAt: new Date(Date.now() + 60000).toISOString() }],
      },
      "POST runs/r1/approvals/t3": {},
    });
    renderView(<ChatThread chatId="c1" onOpenList={() => {}} />);
    expect((await screen.findByTestId("running-tools")).textContent).toBe("Running run_command, fetch__fetch…");
    await screen.findByText("rm", { selector: "code" });
    expect(screen.getAllByTestId("tool-spinner").length).toBeGreaterThan(0);
    decided = true;
    fireEvent.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(screen.queryByTestId("approval-card")).toBeNull());
  });
});
