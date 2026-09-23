import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { useDraft } from "../useDraft";

interface Call {
  method: string;
  path: string;
  body?: string;
  keepalive?: boolean;
}

let server: Record<string, string>;
let calls: Call[];
let putGate: Promise<void> | null;
let failPuts: boolean;

beforeEach(() => {
  server = {};
  calls = [];
  putGate = null;
  failPuts = false;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input);
      const id = path.match(/chats\/([^/]+)\/draft/)![1];
      const method = init?.method ?? "GET";
      calls.push({ method, path, body: init?.body as string, keepalive: init?.keepalive });
      if (method === "PUT") {
        if (putGate) await putGate;
        if (failPuts) return { ok: false, status: 500, statusText: "boom", json: async () => ({}) } as Response;
        const { draft } = JSON.parse(init!.body as string);
        server[id] = draft;
      }
      const draft = server[id] ?? "";
      return {
        ok: true,
        status: 200,
        json: async () => ({ draft, updatedAt: draft ? "2026-01-01T00:00:00Z" : null }),
      } as Response;
    }),
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

function setup(initial: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  return { client, ...renderHook(({ id }) => useDraft(id), { wrapper, initialProps: { id: initial } }) };
}

const puts = () => calls.filter((c) => c.method === "PUT");

describe("useDraft", () => {
  it("loads the saved draft into an untouched box", async () => {
    server.a = "half a thought";
    const { result } = setup("a");
    await waitFor(() => expect(result.current.text).toBe("half a thought"));
  });

  it("never clobbers local typing with a late response", async () => {
    server.a = "from server";
    const { result } = setup("a");
    act(() => result.current.setText("typed"));
    await new Promise((r) => setTimeout(r, 20));
    expect(result.current.text).toBe("typed");
  });

  it("debounces typing into one PUT", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const { result } = setup("a");
    act(() => result.current.setText("h"));
    act(() => result.current.setText("he"));
    act(() => result.current.setText("hey"));
    expect(puts()).toHaveLength(0);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(700);
    });
    expect(puts()).toHaveLength(1);
    expect(JSON.parse(puts()[0].body!)).toEqual({ draft: "hey" });
    expect(result.current.status).toBe("saved");
  });

  it("flushes a pending save on chat switch, under the old chat's id", async () => {
    const { result, rerender } = setup("a");
    await waitFor(() => expect(calls.some((c) => c.method === "GET")).toBe(true));
    act(() => result.current.setText("for A"));
    rerender({ id: "b" });
    await waitFor(() => expect(server.a).toBe("for A"));
    expect(puts()[0].path).toContain("chats/a/draft");
    expect(puts()[0].keepalive).toBe(true);
    expect(result.current.text).toBe("");
  });

  it("does not mix chats when the save is still in flight after a switch", async () => {
    let release!: () => void;
    putGate = new Promise((r) => (release = r));
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const { result, rerender } = setup("a");
    act(() => result.current.setText("for A"));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(700);
    });
    server.b = "B's draft";
    rerender({ id: "b" });
    await waitFor(() => expect(result.current.text).toBe("B's draft"));
    release();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(50);
    });
    expect(result.current.text).toBe("B's draft");
    expect(server.a).toBe("for A");
    expect(server.b).toBe("B's draft");
  });

  it("flushes on blur and on pagehide", async () => {
    const { result } = setup("a");
    act(() => result.current.setText("blurred"));
    act(() => result.current.onBlur());
    await waitFor(() => expect(server.a).toBe("blurred"));

    act(() => result.current.setText("leaving"));
    act(() => {
      window.dispatchEvent(new Event("pagehide"));
    });
    await waitFor(() => expect(server.a).toBe("leaving"));
    expect(puts().at(-1)!.keepalive).toBe(true);
  });

  it("does not re-save sent text: cancels the pending save and clears", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const { result } = setup("a");
    act(() => result.current.setText("sent me"));
    await act(async () => {
      await result.current.beforeSend();
    });
    act(() => result.current.cleared());
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2000);
    });
    expect(puts()).toHaveLength(0);
    expect(result.current.text).toBe("");
  });

  it("waits for an in-flight save before a send", async () => {
    let release!: () => void;
    putGate = new Promise((r) => (release = r));
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const { result } = setup("a");
    act(() => result.current.setText("x"));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(700);
    });
    let done = false;
    void result.current.beforeSend().then(() => (done = true));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(50);
    });
    expect(done).toBe(false);
    release();
    await waitFor(() => expect(done).toBe(true));
  });

  it("keeps the text and reports a failed save", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    failPuts = true;
    const { result } = setup("a");
    act(() => result.current.setText("precious"));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(700);
    });
    await waitFor(() => expect(result.current.status).toBe("error"));
    expect(result.current.text).toBe("precious");
  });

  it("refreshes an untouched box from another tab, but not a focused dirty one", async () => {
    server.a = "one";
    const { result, client } = setup("a");
    await waitFor(() => expect(result.current.text).toBe("one"));
    server.a = "two";
    await act(async () => {
      await client.invalidateQueries({ queryKey: ["chatDraft", "a"] });
    });
    await waitFor(() => expect(result.current.text).toBe("two"));

    act(() => result.current.onFocus());
    act(() => result.current.setText("two, and mine"));
    server.a = "three";
    await act(async () => {
      await client.invalidateQueries({ queryKey: ["chatDraft", "a"] });
    });
    expect(result.current.text).toBe("two, and mine");
  });
});
