import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { useChatStream } from "../useChatStream";

class FakeES {
  static instances: FakeES[] = [];
  static CLOSED = 2;
  readyState = 1;
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  closed = false;
  listeners = new Map<string, ((e: MessageEvent) => void)[]>();
  constructor(public url: string) {
    FakeES.instances.push(this);
  }
  addEventListener(name: string, fn: (e: MessageEvent) => void) {
    this.listeners.set(name, [...(this.listeners.get(name) ?? []), fn]);
  }
  close() {
    this.closed = true;
    this.readyState = 2;
  }
  emit(name: string, data: unknown) {
    for (const fn of this.listeners.get(name) ?? []) {
      fn({ data: JSON.stringify(data) } as MessageEvent);
    }
  }
}

let client: QueryClient;
const wrapper = ({ children }: { children: ReactNode }) => (
  <QueryClientProvider client={client}>{children}</QueryClientProvider>
);

beforeEach(() => {
  vi.useFakeTimers();
  FakeES.instances = [];
  vi.stubGlobal("EventSource", FakeES);
  client = new QueryClient();
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("useChatStream", () => {
  it("subscribes to the chat's stream and assembles deltas", () => {
    const { result } = renderHook(() => useChatStream("c1"), { wrapper });
    const es = FakeES.instances[0];
    expect(es.url).toBe("api/chats/c1/stream");
    act(() => es.emit("delta", { type: "delta", runId: "r1", seq: 1, messageId: "m1", contentIndex: 0, text: "hi" }));
    expect(result.current.state.drafts[0].blocks[0]).toBe("hi");
    expect(result.current.state.lastEventId).toBe("r1:1");
  });

  it("invalidates the chat's queries on message_done and run_done", () => {
    const spy = vi.spyOn(client, "invalidateQueries");
    renderHook(() => useChatStream("c1"), { wrapper });
    const es = FakeES.instances[0];
    act(() => es.emit("run_done", { type: "run_done", runId: "r1", seq: 3, status: "done" }));
    const keys = spy.mock.calls.map((c) => JSON.stringify((c[0] as { queryKey: unknown }).queryKey));
    expect(keys).toContain('["chatMessages","c1"]');
    expect(keys).toContain('["run","r1"]');
  });

  it("on overflow closes the source, refetches and reconnects without a resume position", () => {
    const spy = vi.spyOn(client, "invalidateQueries");
    const { result } = renderHook(() => useChatStream("c1"), { wrapper });
    const first = FakeES.instances[0];
    act(() => first.emit("delta", { runId: "r1", seq: 1, messageId: "m1", contentIndex: 0, text: "x" }));
    act(() => first.emit("overflow", { type: "overflow", reason: "evicted" }));
    expect(first.closed).toBe(true);
    expect(result.current.state.overflow).toBe("evicted");
    expect(result.current.state.drafts).toHaveLength(0);
    expect(spy.mock.calls.some((c) => JSON.stringify(c[0]).includes("chatMessages"))).toBe(true);
    act(() => {
      vi.advanceTimersByTime(300);
    });
    expect(FakeES.instances).toHaveLength(2);
    expect(FakeES.instances[1].url).toBe("api/chats/c1/stream");
    expect(result.current.state.overflow).toBeNull();
  });

  it("resumes with lastEventId after the browser gives up", () => {
    renderHook(() => useChatStream("c1"), { wrapper });
    const first = FakeES.instances[0];
    act(() => first.emit("delta", { runId: "r1", seq: 7, messageId: "m1", contentIndex: 0, text: "x" }));
    first.readyState = FakeES.CLOSED;
    act(() => first.onerror?.());
    act(() => {
      vi.advanceTimersByTime(2000);
    });
    expect(FakeES.instances[1].url).toContain("lastEventId=r1%3A7");
  });

  it("closes the stream on unmount", () => {
    const { unmount } = renderHook(() => useChatStream("c1"), { wrapper });
    unmount();
    expect(FakeES.instances[0].closed).toBe(true);
  });
});
