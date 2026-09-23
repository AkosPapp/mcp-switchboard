import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { renderView } from "../../__tests__/helpers";
import ChatList from "../ChatList";
import ChatThread from "../ChatThread";
import ChatView from "../ChatView";

beforeEach(() => localStorage.clear());
afterEach(() => vi.unstubAllGlobals());

const profile = (id: string, name: string, over: Record<string, unknown> = {}) => ({
  id, name, description: `${name} desc`, systemPrompt: "", model: null,
  capabilities: { canSpawn: true, canMessage: false }, approval: "destructive", budget: {},
  isDefault: false, createdAt: "", updatedAt: "", ...over,
});
const conn = (label: string, environment?: unknown) => ({
  id: `id-${label}`, label, connectedAt: "", servers: [],
  client: { name: "c", version: "1", instance: "i", label, ...(environment ? { environment } : {}) },
});
const agent = (id: string, over: Record<string, unknown> = {}) => ({
  id, parentId: null, name: id, model: null, status: "idle", budget: {}, deletedAt: null, createdAt: "2026-01-01T00:00:00Z",
  lastActivityAt: "2026-01-01T00:00:00Z", origin: "manual", clientLabel: "laptop", profileId: null, ...over,
});
const chatRow = (id: string, over: Record<string, unknown> = {}) => ({
  id, agentId: `a-${id}`, peerAgentId: null, title: "T", kind: "human", activeLeafId: null, tags: [], tokenTotal: 0,
  costTotalMicros: 0, createdAt: "", updatedAt: "", archivedAt: null, profileId: null, clientLabel: "laptop", parentChatId: null, ...over,
});

interface Call { key: string; body?: Record<string, unknown> }
function hub(routes: Record<string, unknown>) {
  const calls: Call[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const key = `${init?.method ?? "GET"} ${String(input).replace(/^api\//, "").split("?")[0]}`;
      calls.push({ key, body: init?.body ? JSON.parse(init.body as string) : undefined });
      if (!(key in routes)) return { ok: false, status: 404, statusText: "nf", json: async () => ({ detail: "nf" }) } as Response;
      const r = routes[key];
      const failure = r && typeof r === "object" && "__status" in r ? (r as { __status: number; detail: string }) : null;
      if (failure) return { ok: false, status: failure.__status, statusText: "err", json: async () => ({ detail: failure.detail }) } as Response;
      return { ok: true, status: 200, statusText: "OK", json: async () => r } as Response;
    }),
  );
  return calls;
}
const base = {
  "GET chats": { chats: [], limit: 500, offset: 0 },
  "GET profiles": { profiles: [profile("p0", "Other"), profile("p1", "Assistant", { isDefault: true })] },
  "GET connections": { connections: [conn("laptop", { kinds: ["devcontainer", "direnv"], project: "webapp", workspace: "/w" }), conn("box")] },
};
const post = (calls: Call[], key: string) => calls.find((c) => c.key === key)?.body;

/** ChatView owns the route so the new-chat form (now inline in the main panel,
 * not a popup) has somewhere to render. */
const chatRoutes = () => (
  <Routes>
    <Route path="/chat" element={<ChatView />} />
    <Route path="/chat/:chatId" element={<ChatView />} />
  </Routes>
);

describe("New chat form (inline in the main panel)", () => {
  const open = async (routes: Record<string, unknown> = {}) => {
    const calls = hub({ ...base, "GET agents": { agents: [] }, ...routes });
    renderView(chatRoutes(), "/chat");
    fireEvent.click(await screen.findByRole("button", { name: "New chat" }));
    await screen.findByRole("heading", { name: "New chat" });
    return calls;
  };
  const select = () => screen.getByLabelText("System prompt") as HTMLSelectElement;

  it("creates with a prompt (live reference, default preselected) and exactly one client, showing project and environment", async () => {
    const calls = await open({ "POST chats": chatRow("new") });
    await waitFor(() => expect(select().value).toBe("p:p1"));
    expect(screen.getByText("can create sub-chats")).toBeTruthy();
    expect(screen.getByText(/Follows the prompt/)).toBeTruthy();
    fireEvent.change(select(), { target: { value: "p:p0" } });
    fireEvent.change(screen.getByLabelText("Title"), { target: { value: "  helper " } });
    await screen.findByText("webapp");
    expect(screen.getByText("devcontainer")).toBeTruthy();
    expect(screen.getByText("direnv")).toBeTruthy();
    expect(screen.getAllByText("connected").length).toBe(2);
    fireEvent.click(screen.getByRole("radio", { name: "laptop" }));
    expect(screen.getAllByRole("radio").filter((r) => (r as HTMLInputElement).checked)).toHaveLength(1);
    fireEvent.click(screen.getByRole("button", { name: "Create chat" }));
    await waitFor(() => expect(post(calls, "POST chats")).toBeTruthy());
    expect(post(calls, "POST chats")).toEqual({ title: "helper", profileId: "p0", clientLabel: "laptop" });
  });

  it("the title is optional, and None and None sends an explicit null profile and client", async () => {
    const calls = await open({ "POST chats": chatRow("new") });
    await waitFor(() => expect(select().value).toBe("p:p1"));
    fireEvent.change(select(), { target: { value: "none" } });
    expect(screen.getByText(/No system prompt is sent/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Create chat" }));
    await waitFor(() => expect(post(calls, "POST chats")).toBeTruthy());
    const body = post(calls, "POST chats")!;
    expect(body).toEqual({ profileId: null, clientLabel: null });
    expect("profileId" in body && "clientLabel" in body).toBe(true);
  });

  it("Custom sends its own text with profileId null", async () => {
    const calls = await open({ "POST chats": chatRow("new") });
    fireEvent.change(select(), { target: { value: "custom" } });
    fireEvent.change(screen.getByLabelText("Custom system prompt"), { target: { value: "Be terse." } });
    fireEvent.change(screen.getByLabelText("Title"), { target: { value: "terse" } });
    fireEvent.click(screen.getByRole("radio", { name: "box" }));
    fireEvent.click(screen.getByRole("button", { name: "Create chat" }));
    await waitFor(() => expect(post(calls, "POST chats")).toBeTruthy());
    expect(post(calls, "POST chats")).toEqual({ title: "terse", profileId: null, systemPrompt: "Be terse.", clientLabel: "box" });
  });

  it("shows the hub's error and stays open", async () => {
    await open({ "POST chats": { __status: 400, detail: "clientLabel must not be empty" } });
    await waitFor(() => expect(select().value).not.toBe(""));
    fireEvent.click(screen.getByRole("button", { name: "Create chat" }));
    expect((await screen.findByRole("alert")).textContent).toBe("clientLabel must not be empty");
    expect(screen.getByRole("heading", { name: "New chat" })).toBeTruthy();
  });

  it("says when no client is connected, and None stays available", async () => {
    await open({ "GET connections": { connections: [] } });
    await screen.findByText(/No MCP client is connected/);
    expect((screen.getByLabelText(/None/, { selector: "input" }) as HTMLInputElement).checked).toBe(true);
  });

  it("a client group's + chat preselects that client", async () => {
    const calls = hub({
      ...base,
      "GET agents": { agents: [] },
      "GET chats": { chats: [chatRow("c1")], limit: 500, offset: 0 },
      "POST chats": chatRow("new"),
    });
    renderView(chatRoutes(), "/chat");
    fireEvent.click(await screen.findByRole("button", { name: "new chat for laptop" }));
    await screen.findByRole("heading", { name: "New chat" });
    expect((screen.getByRole("radio", { name: "laptop" }) as HTMLInputElement).checked).toBe(true);
    await waitFor(() => expect(select().value).toBe("p:p1"));
    fireEvent.click(screen.getByRole("button", { name: "Create chat" }));
    await waitFor(() => expect(post(calls, "POST chats")).toEqual({ profileId: "p1", clientLabel: "laptop" }));
  });

  it("has no agent vocabulary: no agent rows, no New agent, no agent menus", async () => {
    hub({ ...base, "GET agents": { agents: [agent("helper")] }, "GET chats": { chats: [chatRow("c1")], limit: 500, offset: 0 } });
    renderView(<ChatList activeId={null} onNavigate={() => {}} onNewChat={() => {}} />);
    await screen.findByText("T");
    expect(screen.queryByRole("button", { name: /agent/i })).toBeNull();
    expect(screen.queryByRole("button", { name: "helper" })).toBeNull();
    expect(screen.queryByRole("combobox")).toBeNull();
  });
});

describe("chat header: settings, system prompt and delete", () => {
  const chat = chatRow("c1", { agentId: "a1", profileId: "p1" });
  const thread = (extra: Record<string, unknown> = {}, over: Record<string, unknown> = {}) => ({
    "GET chats/c1": { ...chat, ...over },
    "GET chats/c1/messages": { chatId: "c1", activeLeafId: null, tree: true, messages: [] },
    "GET agents/a1": agent("a1"),
    "GET models": { models: [] },
    ...base,
    ...extra,
  });

  it("settings PATCH only what changed: a new prompt and client, through the same form", async () => {
    const calls = hub(thread({ "PATCH chats/c1": chat }));
    renderView(<ChatThread chatId="c1" onOpenList={() => {}} />);
    await screen.findByText("prompt: Assistant");
    fireEvent.click(await screen.findByRole("button", { name: "Settings" }));
    const dialog = await screen.findByRole("dialog", { name: "Chat settings" });
    expect((within(dialog).getByLabelText("System prompt") as HTMLSelectElement).value).toBe("p:p1");
    fireEvent.change(within(dialog).getByLabelText("System prompt"), { target: { value: "p:p0" } });
    fireEvent.click(within(dialog).getByRole("radio", { name: "box" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(post(calls, "PATCH chats/c1")).toBeTruthy());
    expect(post(calls, "PATCH chats/c1")).toEqual({ clientLabel: "box", profileId: "p0" });
  });

  it("settings can switch a profile chat to custom text, and rename it", async () => {
    const calls = hub(thread({ "PATCH chats/c1": chat }));
    renderView(<ChatThread chatId="c1" onOpenList={() => {}} />);
    await screen.findByText("prompt: Assistant");
    fireEvent.click(await screen.findByRole("button", { name: "Settings" }));
    const dialog = await screen.findByRole("dialog", { name: "Chat settings" });
    fireEvent.change(within(dialog).getByLabelText("Title"), { target: { value: "Renamed" } });
    fireEvent.change(within(dialog).getByLabelText("System prompt"), { target: { value: "custom" } });
    fireEvent.change(within(dialog).getByLabelText("Custom system prompt"), { target: { value: "Be brief." } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(post(calls, "PATCH chats/c1")).toBeTruthy());
    expect(post(calls, "PATCH chats/c1")).toEqual({ title: "Renamed", profileId: null, systemPrompt: "Be brief." });
  });

  it("a chat with its own prompt opens the settings on Custom with that text, and saving unchanged sends nothing", async () => {
    const calls = hub(
      thread(
        { "GET chats/c1/system-prompt": { systemPrompt: "mine", source: "agent", profileId: null, profileName: null, model: null, toolCount: 0 } },
        { profileId: null },
      ),
    );
    renderView(<ChatThread chatId="c1" onOpenList={() => {}} />);
    await screen.findByText("custom prompt");
    fireEvent.click(await screen.findByRole("button", { name: "Settings" }));
    const dialog = await screen.findByRole("dialog", { name: "Chat settings" });
    await waitFor(() => expect((within(dialog).getByLabelText("System prompt") as HTMLSelectElement).value).toBe("custom"));
    expect((within(dialog).getByLabelText("Custom system prompt") as HTMLTextAreaElement).value).toBe("mine");
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Chat settings" })).toBeNull());
    expect(calls.some((c) => c.key === "PATCH chats/c1")).toBe(false);
  });

  it("shows the exact prompt, its source, model, tool count and a link to the prompt", async () => {
    const text = "Line one\n  indented line\n\nLast";
    hub(thread({ "GET chats/c1/system-prompt": { systemPrompt: text, source: "profile", profileId: "p1", profileName: "Assistant", model: { provider: "anthropic", model: "sonnet" }, toolCount: 7 } }));
    renderView(<ChatThread chatId="c1" onOpenList={() => {}} />);
    fireEvent.click(await screen.findByRole("button", { name: "System prompt" }));
    const panel = await screen.findByRole("dialog", { name: "system prompt for this chat" });
    const pre = await within(panel).findByTestId("prompt-text");
    expect(pre.textContent).toBe(text);
    expect(pre.className).toContain("font-mono");
    expect(pre.className).toContain("whitespace-pre-wrap");
    expect(within(panel).getByTestId("prompt-source").textContent).toBe("from prompt Assistant");
    expect(within(panel).getByText("anthropic/sonnet")).toBeTruthy();
    expect(within(panel).getByText("7 tools")).toBeTruthy();
    expect(within(panel).getByRole("link", { name: "Edit prompt" }).getAttribute("href")).toBe("/prompts/p1");
    expect(within(panel).getByRole("button", { name: "Copy" })).toBeTruthy();
  });

  it("labels the chat's own prompt and the no-prompt case without a prompt link", async () => {
    hub(thread({ "GET chats/c1/system-prompt": { systemPrompt: "mine", source: "agent", profileId: null, profileName: null, model: null, toolCount: 0 } }));
    const { unmount } = renderView(<ChatThread chatId="c1" onOpenList={() => {}} />);
    fireEvent.click(await screen.findByRole("button", { name: "System prompt" }));
    expect((await screen.findByTestId("prompt-source")).textContent).toBe("this chat's own prompt");
    expect(screen.queryByRole("link", { name: "Edit prompt" })).toBeNull();
    unmount();

    hub(thread({ "GET chats/c1/system-prompt": { systemPrompt: "", source: "none", profileId: null, profileName: null, model: null, toolCount: 1 } }));
    renderView(<ChatThread chatId="c1" onOpenList={() => {}} />);
    fireEvent.click(await screen.findByRole("button", { name: "System prompt" }));
    expect((await screen.findByTestId("prompt-source")).textContent).toBe("none — no system prompt is sent");
    expect(screen.getByText("1 tool")).toBeTruthy();
  });

  it("deletes from the header menu after an inline confirm that names the sub-chats", async () => {
    const calls = hub(thread({ "DELETE chats/c1": { deletedChats: 2 } }));
    renderView(<ChatThread chatId="c1" onOpenList={() => {}} />, "/chat/c1");
    fireEvent.click(await screen.findByRole("button", { name: "chat menu" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Delete chat…" }));
    expect(screen.getByText("Delete this chat and its sub-chats?")).toBeTruthy();
    expect(calls.some((c) => c.key === "DELETE chats/c1")).toBe(false);
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(calls.some((c) => c.key === "DELETE chats/c1")).toBe(true));
    expect(await screen.findByText("2 chats deleted")).toBeTruthy();
  });

  it("a floating Chats button opens the list, and the '…' menu also opens Settings/System prompt/Tools (the mobile overflow)", async () => {
    hub(thread());
    const onOpenList = vi.fn();
    renderView(<ChatThread chatId="c1" onOpenList={onOpenList} />, "/chat/c1");
    await screen.findByText("prompt: Assistant");

    fireEvent.click(screen.getByRole("button", { name: "Chats" }));
    expect(onOpenList).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByRole("button", { name: "chat menu" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "System prompt" }));
    expect(await screen.findByRole("dialog", { name: "system prompt for this chat" })).toBeTruthy();
  });
});
