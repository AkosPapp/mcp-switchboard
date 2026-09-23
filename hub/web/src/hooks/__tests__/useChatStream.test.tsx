import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { HubSocket } from "../../lib/hubSocket";
import { useChatStream } from "../useChatStream";
import { FakeWS, installFakeWS } from "./fakeWS";

let client: QueryClient;
let socket: HubSocket;
const wrapper = ({ children }: { children: ReactNode }) => (
  <QueryClientProvider client={client}>{children}</QueryClientProvider>
);

beforeEach(() => {
  vi.useFakeTimers();
  installFakeWS();
  client = new QueryClient();
  socket = new HubSocket(() => "ws://hub/api/ws");
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

const frame = (name: string, data: Record<string, unknown>) => ({ t: "frame", chat: "c1", name, id: `${data.runId}:${data.seq}`, data: { type: name, ...data } });

function live() {
  const ws = FakeWS.instances[0];
  act(() => ws.serverOpen());
  act(() => ws.serverSend({ t: "hello" }));
  return ws;
}

describe("useChatStream", () => {
  it("subscribes to the chat once the hub says hello, and assembles deltas", () => {
    const { result } = renderHook(() => useChatStream("c1", socket), { wrapper });
    const ws = live();
    expect(ws.sent).toEqual([{ op: "sub", chat: "c1", last: "" }]);
    act(() => ws.serverSend(frame("delta", { runId: "r1", seq: 1, messageId: "m1", contentIndex: 0, text: "hi" })));
    expect(result.current.state.drafts[0].blocks[0]).toBe("hi");
    expect(result.current.state.lastEventId).toBe("r1:1");
    expect(result.current.connection).toBe("live");
  });

  it("ignores other chats' frames", () => {
    const { result } = renderHook(() => useChatStream("c1", socket), { wrapper });
    const ws = live();
    act(() => ws.serverSend({ ...frame("delta", { runId: "r1", seq: 1, messageId: "m1", contentIndex: 0, text: "x" }), chat: "c2" }));
    expect(result.current.state.drafts).toHaveLength(0);
  });

  it("invalidates the chat's queries on message_done and run_done", () => {
    const spy = vi.spyOn(client, "invalidateQueries");
    renderHook(() => useChatStream("c1", socket), { wrapper });
    const ws = live();
    act(() => ws.serverSend(frame("run_done", { runId: "r1", seq: 3, status: "done" })));
    const keys = spy.mock.calls.map((c) => JSON.stringify((c[0] as { queryKey: unknown }).queryKey));
    expect(keys).toContain('["chatMessages","c1"]');
    expect(keys).toContain('["run","r1"]');
  });

  it("on overflow refetches and resubscribes from the live edge, without a resume position", () => {
    const spy = vi.spyOn(client, "invalidateQueries");
    const { result } = renderHook(() => useChatStream("c1", socket), { wrapper });
    const ws = live();
    act(() => ws.serverSend(frame("delta", { runId: "r1", seq: 1, messageId: "m1", contentIndex: 0, text: "x" })));
    act(() => ws.serverSend({ t: "overflow", chat: "c1", reason: "evicted" }));
    expect(result.current.state.overflow).toBe("evicted");
    expect(result.current.state.drafts).toHaveLength(0);
    expect(spy.mock.calls.some((c) => JSON.stringify(c[0]).includes("chatMessages"))).toBe(true);
    act(() => {
      vi.advanceTimersByTime(300);
    });
    expect(ws.sent.at(-1)).toEqual({ op: "sub", chat: "c1", last: "" });
    expect(result.current.state.overflow).toBeNull();
  });

  it("resumes from the last frame after the socket drops and comes back", () => {
    renderHook(() => useChatStream("c1", socket), { wrapper });
    const first = live();
    act(() => first.serverSend(frame("delta", { runId: "r1", seq: 7, messageId: "m1", contentIndex: 0, text: "x" })));
    act(() => first.serverClose());
    act(() => {
      vi.advanceTimersByTime(600);
    });
    const second = FakeWS.instances[1];
    act(() => second.serverOpen());
    act(() => second.serverSend({ t: "hello" }));
    expect(second.sent).toEqual([{ op: "sub", chat: "c1", last: "r1:7" }]);
  });

  it("unsubscribes and closes the socket on unmount", () => {
    const { unmount } = renderHook(() => useChatStream("c1", socket), { wrapper });
    const ws = live();
    unmount();
    expect(ws.sent.at(-1)).toEqual({ op: "unsub", chat: "c1" });
    expect(ws.closed).toBe(true);
  });
});
