import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { renderView } from "../../__tests__/helpers";
import ApprovalCard from "../ApprovalCard";
import ChatList from "../ChatList";
import ChatThread from "../ChatThread";
import { buildContent } from "../Composer";
import { snippetParts } from "../format";
import ToolCard from "../ToolCard";
import type { Chat, Message } from "../types";

afterEach(() => vi.unstubAllGlobals());

const message = (over: Partial<Message>): Message => ({
  id: "m",
  chatId: "c1",
  parentId: null,
  role: "user",
  content: [{ type: "text", text: "" }],
  toolCalls: null,
  toolResults: null,
  tokenInput: 0,
  tokenOutput: 0,
  costMicros: 0,
  latencyMs: 0,
  model: null,
  finishReason: null,
  runId: null,
  lastActiveChildId: null,
  createdAt: "2026-01-01T00:00:00+00:00",
  ...over,
});

const chat = (over: Partial<Chat>): Chat => ({
  id: "c1",
  agentId: "a1",
  peerAgentId: null,
  title: "First",
  kind: "human",
  activeLeafId: null,
  tags: [],
  tokenTotal: 1500,
  costTotalMicros: 20000,
  createdAt: "",
  updatedAt: "",
  archivedAt: null,
  ...over,
});

const agent = {
  id: "a1", name: "helper", model: { provider: "p", model: "big" }, status: "idle", budget: {}, deletedAt: null,
  origin: "manual", clientLabel: "laptop", profileId: null, parentId: null, createdAt: "2026-01-01T00:00:00Z",
};

/** A hub that answers by "METHOD path"; records requests. */
function hub(routes: Record<string, unknown>) {
  const calls: { key: string; body?: string }[] = [];
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const path = String(input).replace(/^api\//, "");
    const key = `${init?.method ?? "GET"} ${path.split("?")[0]}`;
    calls.push({ key, body: init?.body as string | undefined });
    if (!(key in routes)) {
      return { ok: false, status: 404, statusText: "nf", json: async () => ({ detail: "nf" }) } as Response;
    }
    const status = key.startsWith("POST") || key.startsWith("PATCH") ? 200 : 200;
    return { ok: true, status, statusText: "OK", json: async () => routes[key] } as Response;
  });
  vi.stubGlobal("fetch", fn);
  return calls;
}

describe("ChatThread", () => {
  const u1 = message({ id: "u1", content: [{ type: "text", text: "question" }], siblings: { ids: ["u1"], index: 0 } });
  const a2 = message({
    id: "a2",
    parentId: "u1",
    role: "assistant",
    content: [{ type: "text", text: "second answer" }],
    siblings: { ids: ["a1", "a2"], index: 1 },
  });

  const routes = () => ({
    "GET chats/c1": chat({ activeLeafId: "a2" }),
    "GET chats/c1/messages": { chatId: "c1", activeLeafId: "a2", tree: false, messages: [u1, a2] },
    "GET agents/a1": agent,
    "GET models": { models: [{ provider: "p", model: "big" }] },
    "PATCH chats/c1": chat({}),
    "POST chats/c1/branch": { messageId: "a3", runId: "r9" },
  });

  it("shows n/m, navigates siblings by selectMessageId, and regenerates", async () => {
    const calls = hub(routes());
    renderView(<ChatThread chatId="c1" onOpenList={() => {}} />);

    await screen.findByText("second answer");
    expect(screen.getByLabelText("version 2 of 2")).toBeTruthy();
    expect((screen.getByLabelText("next version") as HTMLButtonElement).disabled).toBe(true);

    fireEvent.click(screen.getByLabelText("previous version"));
    await waitFor(() => expect(calls.some((c) => c.key === "PATCH chats/c1")).toBe(true));
    expect(JSON.parse(calls.find((c) => c.key === "PATCH chats/c1")!.body!)).toEqual({ selectMessageId: "a1" });

    fireEvent.click(screen.getByText("Regenerate"));
    await waitFor(() => expect(calls.some((c) => c.key === "POST chats/c1/branch")).toBe(true));
    expect(JSON.parse(calls.find((c) => c.key === "POST chats/c1/branch")!.body!)).toEqual({ fromMessageId: "a2" });
  });

  it("editing a user message branches with the new content", async () => {
    const calls = hub(routes());
    renderView(<ChatThread chatId="c1" onOpenList={() => {}} />);
    await screen.findByText("question");
    fireEvent.click(screen.getByText("Edit"));
    fireEvent.change(screen.getByLabelText("edit message"), { target: { value: "reworded" } });
    fireEvent.click(screen.getByText(/Save/));
    await waitFor(() => expect(calls.some((c) => c.key === "POST chats/c1/branch")).toBe(true));
    expect(JSON.parse(calls.find((c) => c.key === "POST chats/c1/branch")!.body!)).toEqual({
      fromMessageId: "u1",
      content: "reworded",
    });
  });

  it("queues a message sent while a run is active instead of posting it right away", async () => {
    const running = { ...a2, runId: "r1" };
    const r = {
      "GET chats/c1": chat({ activeLeafId: "a2" }),
      "GET chats/c1/messages": { chatId: "c1", activeLeafId: "a2", tree: false, messages: [u1, running] },
      "GET agents/a1": agent,
      "GET models": { models: [{ provider: "p", model: "big" }] },
      "GET runs/r1": { status: "running" },
      "POST chats/c1/messages": { messageId: "u9", runId: "r2" },
    };
    const calls = hub(r);
    renderView(<ChatThread chatId="c1" onOpenList={() => {}} />);
    await screen.findByText("second answer");
    await screen.findByText("Stop"); // confirms the run is seen as active

    fireEvent.change(screen.getByLabelText("message"), { target: { value: "queued one" } });
    fireEvent.click(screen.getByText("Send"));
    await screen.findByTestId("message-queue");
    expect(screen.getByText("queued one")).toBeTruthy();
    expect(calls.some((c) => c.key === "POST chats/c1/messages")).toBe(false);
    // The textarea is free again immediately, ready for the next message.
    expect((screen.getByLabelText("message") as HTMLTextAreaElement).value).toBe("");

    // Removing it before it is ever sent drops it for good.
    fireEvent.click(screen.getByLabelText("remove queued message"));
    expect(screen.queryByTestId("message-queue")).toBeNull();
    expect(calls.some((c) => c.key === "POST chats/c1/messages")).toBe(false);
  });

  it("sends with an Idempotency-Key header", async () => {
    const r = { ...routes(), "POST chats/c1/messages": { messageId: "u9", runId: "r1" } };
    const seen: Record<string, string>[] = [];
    const calls = hub(r);
    const inner = globalThis.fetch as unknown as (i: RequestInfo, init?: RequestInit) => Promise<Response>;
    vi.stubGlobal("fetch", (i: RequestInfo, init?: RequestInit) => {
      if (init?.method === "POST") seen.push(init.headers as Record<string, string>);
      return inner(i, init);
    });
    renderView(<ChatThread chatId="c1" onOpenList={() => {}} />);
    await screen.findByText("second answer");
    fireEvent.change(screen.getByLabelText("message"), { target: { value: "hello" } });
    fireEvent.click(screen.getByText("Send"));
    await waitFor(() => expect(calls.some((c) => c.key === "POST chats/c1/messages")).toBe(true));
    expect(seen[0]["Idempotency-Key"]).toBeTruthy();
  });
});

describe("ToolCard", () => {
  it("degrades to 'call record expired' when the call row is gone", async () => {
    hub({});
    renderView(
      <ToolCard tool={{ callId: "t1", name: "echo", arguments: { a: 1 }, done: true, result: "x", recordId: "gone" }} />,
    );
    fireEvent.click(screen.getByRole("button", { name: /echo/ }));
    await screen.findByText("call record expired");
  });

  it("links to the Calls row and shows label, duration and status", async () => {
    hub({
      "GET calls/rec1": {
        id: "rec1", label: "laptop", server: "files", tool: "read", status: "ok", durationMs: 42, arguments: {}, result: null,
      },
    });
    renderView(
      <ToolCard tool={{ callId: "t1", name: "read", arguments: {}, done: true, result: "x", recordId: "rec1" }} />,
    );
    await screen.findByText("42 ms");
    fireEvent.click(screen.getByRole("button", { name: /read/ }));
    expect(screen.getByText("open in Calls").getAttribute("href")).toBe("/calls?call=rec1");
  });
});

describe("ApprovalCard", () => {
  it("names the tool, counts down and hands the decision up", () => {
    const decide = vi.fn();
    const expiresAt = new Date(Date.now() + 90_000).toISOString();
    renderView(<ApprovalCard runId="r1" callId="c1" tool="run_command" arguments={{ path: "/" }} expiresAt={expiresAt} onDecide={decide} />);
    expect(screen.getByText("run_command").tagName).toBe("CODE");
    expect(screen.getByText(/Approve/, { selector: "span" }).textContent).toBe("Approve run_command?");
    expect(screen.getByText(/auto-deny in 1:/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Approve" }));
    expect(decide).toHaveBeenCalledWith(true);
    fireEvent.click(screen.getByRole("button", { name: "Deny" }));
    expect(decide).toHaveBeenCalledWith(false);
  });
});

describe("ChatList", () => {
  it("shows chat badges on one row per chat, under its client group", async () => {
    hub({
      "GET agents": { agents: [agent] },
      "GET chats": { chats: [chat({ tags: ["x"], clientLabel: "laptop" }), chat({ id: "c2", title: "Second", clientLabel: "laptop" })], limit: 500, offset: 0 },
    });
    renderView(<ChatList activeId="c1" onNavigate={() => {}} onNewChat={() => {}} />);
    await screen.findByText("First");
    expect(screen.queryByRole("button", { name: "helper" })).toBeNull();
    expect(screen.getByTestId("client-group").getAttribute("data-client")).toBe("laptop");
    expect(screen.getAllByTestId("chat-row")).toHaveLength(2);
    // Tokens, cost and tags live in the row's tooltip, not as badges.
    const tip = screen.getAllByTestId("chat-row")[0].querySelector("a")!.getAttribute("title")!;
    expect(tip).toContain("1.5k tok");
    expect(tip).toContain("$0.02");
    expect(tip).toContain("#x");
  });
});

describe("injected messages", () => {
  const injected = (over: Partial<Message> = {}) =>
    message({
      id: "i1",
      content: [{ type: "text", text: '[Message from chat "researcher" (id r1). Reply with switchboard.chat.send to that id.]\n\nfound **it**' }],
      sender: { chatId: "r1", chatTitle: "researcher", kind: "message" },
      siblings: { ids: ["i1"], index: 0 },
      ...over,
    });
  const human = message({ id: "u1", content: [{ type: "text", text: "typed by me" }], siblings: { ids: ["u1"], index: 0 } });
  const open = async (messages: Message[]) => {
    hub({
      "GET chats/c1": chat({ profileId: "p1", clientLabel: null }),
      "GET chats/c1/messages": { chatId: "c1", activeLeafId: null, tree: false, messages },
      "GET agents/a1": agent,
      "GET models": { models: [] },
      "GET profiles": { profiles: [] },
      "GET connections": { connections: [] },
    });
    renderView(<ChatThread chatId="c1" onOpenList={() => {}} />, "/chat/c1");
    await screen.findByText("typed by me");
  };

  it("renders as a distinct block from the sender chat, linked, without the model-facing preamble", async () => {
    await open([human, injected({ parentId: "u1" })]);
    const block = await screen.findByTestId("message-injected");
    const chip = within(block).getByTestId("sender-chip");
    expect(chip.textContent).toBe("researcher");
    expect(chip.getAttribute("href")).toBe("/chat/r1");
    expect(within(block).getByTestId("sender-kind").textContent).toBe("message");
    expect(block.textContent).not.toContain("[Message from chat");
    expect((await within(block).findByText("it")).tagName).toBe("STRONG");
    // the human message is still a plain user message
    expect(screen.getAllByTestId("message-user")).toHaveLength(1);
  });

  it("has no Edit and no Regenerate, but keeps Branch here; the human message keeps Edit", async () => {
    await open([human, injected({ parentId: "u1" })]);
    const block = await screen.findByTestId("message-injected");
    expect(within(block).queryByRole("button", { name: "Edit" })).toBeNull();
    expect(within(block).queryByRole("button", { name: "Regenerate" })).toBeNull();
    expect(within(block).getByRole("button", { name: "Branch here" })).toBeTruthy();
    expect(within(screen.getByTestId("message-user")).getByRole("button", { name: "Edit" })).toBeTruthy();
  });

  it("labels a reply and a spawn task", async () => {
    await open([
      human,
      injected({ id: "i2", parentId: "u1", sender: { chatId: "r2", chatTitle: "helper", kind: "reply" } }),
      injected({ id: "i3", parentId: "i2", sender: { chatId: "r3", chatTitle: "boss", kind: "spawn" } }),
    ]);
    await waitFor(() => expect(screen.getAllByTestId("sender-kind").map((e) => e.textContent)).toEqual(["reply", "task"]));
  });
});

describe("helpers", () => {
  it("buildContent returns a string without attachments and blocks with", () => {
    expect(buildContent("hi", [])).toBe("hi");
    expect(buildContent("hi", [{ name: "a.png", block: { type: "image", media_type: "image/png", data: "AA" } }])).toEqual([
      { type: "text", text: "hi" },
      { type: "image", media_type: "image/png", data: "AA" },
    ]);
  });
  it("splits search snippets on the match markers", () => {
    expect(snippetParts("a \u0002hit\u0003 b")).toEqual([
      { text: "a ", hit: false },
      { text: "hit", hit: true },
      { text: " b", hit: false },
    ]);
  });
});

const profile = (over: Record<string, unknown>) => ({
  id: "p1",
  name: "Assistant",
  description: "General helper",
  systemPrompt: "",
  model: null,
  capabilities: { canSpawn: false, canMessage: false },
  approval: "destructive",
  budget: {},
  isDefault: false,
  createdAt: "",
  updatedAt: "",
  ...over,
});
describe("Tools panel and header", () => {
  const routes = (tools: unknown) => ({
    "GET chats/c1": chat({ profileId: "p1", clientLabel: "laptop" }),
    "GET chats/c1/messages": { chatId: "c1", activeLeafId: null, tree: true, messages: [] },
    "GET agents/a1": agent,
    "GET models": { models: [] },
    "GET profiles": { profiles: [profile({ id: "p1", name: "Assistant" })] },
    "GET chats/c1/tools": tools,
  });

  it("shows prompt and client chips and lists tools grouped by origin", async () => {
    hub(
      routes({
        clientLabel: "laptop",
        clientConnected: true,
        tools: [
          { name: "laptop__files__read", description: "Read a file", origin: "mcp", server: { label: "laptop", project: "", server: "files" }, annotations: { readOnlyHint: true }, inputSchema: { type: "object", title: "ReadArgs" } },
          { name: "switchboard.chat.spawn", description: "Spawn", origin: "hub" },
        ],
      }),
    );
    renderView(<ChatThread chatId="c1" onOpenList={() => {}} />);
    await screen.findByText("laptop");
    await screen.findByText("prompt: Assistant");
    fireEvent.click(screen.getByText("Tools"));
    await screen.findByText("laptop__files__read");
    expect(screen.getByText("laptop/files")).toBeTruthy();
    expect(screen.getByText("Hub tools")).toBeTruthy();
    expect(screen.getByText("read-only")).toBeTruthy();
    expect(screen.queryByText(/ReadArgs/)).toBeNull();
    fireEvent.click(screen.getByText("Show input schema"));
    expect(screen.getByText(/ReadArgs/)).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("warns when the client is offline", async () => {
    hub(
      routes({
        clientLabel: "laptop",
        clientConnected: false,
        tools: [{ name: "switchboard.chat.spawn", description: "Spawn", origin: "hub" }],
      }),
    );
    renderView(<ChatThread chatId="c1" onOpenList={() => {}} />);
    fireEvent.click(await screen.findByText("Tools"));
    expect((await screen.findByRole("alert")).textContent).toMatch(/client offline/i);
  });

  it("falls back for legacy chats", async () => {
    hub({ ...routes({ clientLabel: null, clientConnected: false, tools: [] }), "GET chats/c1": chat({}) });
    renderView(<ChatThread chatId="c1" onOpenList={() => {}} />);
    await screen.findByText("no client");
    expect(screen.getByText("no prompt")).toBeTruthy();
  });
});
