import { act, fireEvent, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { renderView } from "../../__tests__/helpers";
import ChatList from "../ChatList";
import type { Chat } from "../types";

const chat = (over: Partial<Chat>): Chat => ({
  id: "c1",
  agentId: "a1",
  peerAgentId: null,
  title: "First",
  kind: "human",
  activeLeafId: null,
  tags: [],
  tokenTotal: 0,
  costTotalMicros: 0,
  createdAt: "",
  updatedAt: "",
  archivedAt: null,
  ...over,
});
const agent = {
  id: "a1", name: "helper", model: null, status: "idle", budget: {}, deletedAt: null,
  origin: "manual", clientLabel: "box", profileId: null, parentId: null, createdAt: "2026-01-01T00:00:00Z",
};

function hub(all: Chat[]) {
  let chats = all;
  const calls: { key: string; body?: string }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input).replace(/^api\//, "").split("?")[0];
      const key = `${init?.method ?? "GET"} ${path}`;
      calls.push({ key, body: init?.body as string | undefined });
      if (key.startsWith("PATCH")) chats = chats.filter(() => !JSON.parse(init!.body as string).archived);
      if (key.startsWith("DELETE")) {
        chats = [];
        return { ok: true, status: 204, statusText: "No Content", json: async () => undefined } as Response;
      }
      const body =
        key === "GET agents"
          ? { agents: [agent] }
          : key === "GET chats"
            ? { chats, limit: 500, offset: 0 }
            : key.startsWith("PATCH")
              ? all[0]
              : undefined;
      if (body === undefined) return { ok: false, status: 404, statusText: "nf", json: async () => ({ detail: "nf" }) } as Response;
      return { ok: true, status: 200, statusText: "OK", json: async () => body } as Response;
    }),
  );
  return calls;
}

beforeEach(() => localStorage.clear());
afterEach(() => vi.unstubAllGlobals());

describe("archive from the chat list", () => {
  it("archives without confirming, and without opening the chat", async () => {
    const confirm = vi.spyOn(window, "confirm");
    const calls = hub([chat({})]);
    const onNavigate = vi.fn();
    renderView(<ChatList activeId={null} onNavigate={onNavigate} onNewChat={() => {}} />, "/chat");
    fireEvent.click(await screen.findByLabelText("Archive"));

    await waitFor(() => expect(calls.some((c) => c.key === "PATCH chats/c1")).toBe(true));
    expect(JSON.parse(calls.find((c) => c.key === "PATCH chats/c1")!.body!)).toEqual({ archived: true });
    expect(confirm).not.toHaveBeenCalled();
    expect(onNavigate).not.toHaveBeenCalled();
    // Optimistically gone from the list.
    await waitFor(() => expect(screen.queryByText("First")).toBeNull());
  });

  it("offers Unarchive on an archived chat", async () => {
    const calls = hub([chat({ archivedAt: "2026-01-01T00:00:00+00:00" })]);
    renderView(<ChatList activeId={null} onNavigate={() => {}} onNewChat={() => {}} />, "/chat");
    fireEvent.click(await screen.findByLabelText("Unarchive"));
    await waitFor(() => expect(calls.some((c) => c.key === "PATCH chats/c1")).toBe(true));
    expect(JSON.parse(calls.find((c) => c.key === "PATCH chats/c1")!.body!)).toEqual({ archived: false });
  });

  it("archiving the open chat drops its route memory and leaves for /chat", async () => {
    localStorage.setItem(
      "mcpsb.ui.v1.route",
      JSON.stringify({ v: 1, d: { tabs: { "/chat": "/chat/c1" }, last: "/chat/c1" } }),
    );
    hub([chat({})]);
    renderView(<ChatList activeId="c1" onNavigate={() => {}} onNewChat={() => {}} />, "/chat/c1");
    fireEvent.click(await screen.findByLabelText("Archive"));
    await waitFor(() => expect(screen.getByTestId("search")).toBeInTheDocument());
    await waitFor(() => expect(localStorage.getItem("mcpsb.ui.v1.route") ?? "").not.toContain("c1"));
  });
});

describe("easy delete from the chat list", () => {
  it("arms inline on the first press, deletes on the check mark, never opens a modal", async () => {
    const confirm = vi.spyOn(window, "confirm");
    const calls = hub([chat({})]);
    renderView(<ChatList activeId={null} onNavigate={() => {}} onNewChat={() => {}} />, "/chat");
    fireEvent.click(await screen.findByLabelText("Delete"));
    expect(screen.getByText("Delete?")).toBeTruthy();
    expect(calls.some((c) => c.key === "DELETE chats/c1")).toBe(false);
    fireEvent.click(screen.getByLabelText("Confirm delete"));
    await waitFor(() => expect(calls.some((c) => c.key === "DELETE chats/c1")).toBe(true));
    expect(confirm).not.toHaveBeenCalled();
    await waitFor(() => expect(screen.queryByText("First")).toBeNull());
  });

  it("cancels with the cross, and disarms by itself after a few seconds", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const calls = hub([chat({})]);
      renderView(<ChatList activeId={null} onNavigate={() => {}} onNewChat={() => {}} />, "/chat");
      fireEvent.click(await screen.findByLabelText("Delete"));
      fireEvent.click(screen.getByLabelText("Cancel delete"));
      expect(screen.queryByText("Delete?")).toBeNull();
      fireEvent.click(screen.getByLabelText("Delete"));
      expect(screen.getByText("Delete?")).toBeTruthy();
      await act(async () => {
        vi.advanceTimersByTime(4500);
      });
      expect(screen.queryByText("Delete?")).toBeNull();
      expect(calls.some((c) => c.key.startsWith("DELETE"))).toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });

  it("deleting the open chat drops its route memory and leaves for /chat", async () => {
    localStorage.setItem(
      "mcpsb.ui.v1.route",
      JSON.stringify({ v: 1, d: { tabs: { "/chat": "/chat/c1" }, last: "/chat/c1" } }),
    );
    hub([chat({})]);
    renderView(<ChatList activeId="c1" onNavigate={() => {}} onNewChat={() => {}} />, "/chat/c1");
    fireEvent.click(await screen.findByLabelText("Delete"));
    fireEvent.click(screen.getByLabelText("Confirm delete"));
    await waitFor(() => expect(screen.getByTestId("search").textContent).toBe(""));
    await waitFor(() => expect(localStorage.getItem("mcpsb.ui.v1.route") ?? "").not.toContain("c1"));
  });
});
