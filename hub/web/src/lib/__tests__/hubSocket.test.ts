import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { FakeWS, installFakeWS } from "../../hooks/__tests__/fakeWS";
import { HubSocket } from "../hubSocket";

beforeEach(() => {
  vi.useFakeTimers();
  installFakeWS();
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

const up = (ws: FakeWS) => {
  ws.serverOpen();
  ws.serverSend({ t: "hello" });
};

describe("HubSocket", () => {
  it("connects when something listens, is live on hello, and closes when the last listener leaves", () => {
    const s = new HubSocket(() => "ws://hub/api/ws");
    expect(FakeWS.instances).toHaveLength(0);
    const off = s.onEvent(() => {});
    expect(s.getState()).toBe("connecting");
    const ws = FakeWS.instances[0];
    ws.serverOpen();
    expect(s.getState()).toBe("connecting"); // open is not yet listening
    ws.serverSend({ t: "hello" });
    expect(s.getState()).toBe("live");
    off();
    expect(ws.closed).toBe(true);
  });

  it("delivers change events", () => {
    const s = new HubSocket(() => "ws://hub/api/ws");
    const got: unknown[] = [];
    s.onEvent((e) => got.push(e));
    const ws = FakeWS.instances[0];
    up(ws);
    ws.serverSend({ t: "event", data: { type: "draft", chatId: "c1" } });
    ws.serverSend({ t: "hb" });
    expect(got).toEqual([{ type: "draft", chatId: "c1" }]);
  });

  it("reconnects with backoff, tells reopen listeners, and resubscribes chats", () => {
    const s = new HubSocket(() => "ws://hub/api/ws");
    const reopen = vi.fn();
    s.onReopen(reopen);
    s.onEvent(() => {});
    const first = FakeWS.instances[0];
    up(first);
    expect(reopen).not.toHaveBeenCalled(); // the first connection is not a reopen
    s.subscribeChat("c1", () => "r:2", () => {});
    expect(first.sent.at(-1)).toEqual({ op: "sub", chat: "c1", last: "r:2" });

    first.serverClose();
    expect(s.getState()).toBe("offline");
    vi.advanceTimersByTime(499);
    expect(FakeWS.instances).toHaveLength(1);
    vi.advanceTimersByTime(2);
    const second = FakeWS.instances[1];
    up(second);
    expect(s.getState()).toBe("live");
    expect(reopen).toHaveBeenCalledTimes(1);
    expect(second.sent).toEqual([{ op: "sub", chat: "c1", last: "r:2" }]);
  });

  it("gives up on a link that has gone silent", () => {
    const s = new HubSocket(() => "ws://hub/api/ws");
    s.onEvent(() => {});
    const ws = FakeWS.instances[0];
    up(ws);
    vi.advanceTimersByTime(45_001); // just past the watchdog, before its retry
    expect(ws.closed).toBe(true);
    expect(s.getState()).toBe("offline");
  });

  it("a heartbeat keeps the link", () => {
    const s = new HubSocket(() => "ws://hub/api/ws");
    s.onEvent(() => {});
    const ws = FakeWS.instances[0];
    up(ws);
    for (let i = 0; i < 4; i++) {
      vi.advanceTimersByTime(15_000);
      ws.serverSend({ t: "hb" });
    }
    expect(ws.closed).toBe(false);
  });
});
