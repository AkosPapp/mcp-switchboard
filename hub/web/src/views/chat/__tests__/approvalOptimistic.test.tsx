import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { ToastProvider } from "../../../components/Toast";
import { chatKeys, useDecideApproval } from "../api";
import type { ApprovalItem, Run } from "../types";

afterEach(() => vi.unstubAllGlobals());

const item = (callId: string): ApprovalItem => ({
  runId: "r1", chatId: "c1", agentId: "a1", agentName: "helper", chatTitle: "t", callId,
  tool: "run_command", arguments: {}, expiresAt: "2026-01-01T00:00:00Z",
});
const run = (): Run =>
  ({ id: "r1", agentId: "a1", chatId: "c1", status: "waiting", budgetSnapshot: {}, usage: {}, error: null, finishReason: null,
    pendingApprovals: [{ callId: "k1", tool: "run_command", arguments: {}, expiresAt: "" }] }) as Run;

function setup(respond: () => Promise<Partial<Response>>) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
  client.setQueryData(chatKeys.run("r1"), run());
  client.setQueryData(chatKeys.approvals, [item("k1"), item("k2")]);
  vi.stubGlobal("fetch", vi.fn(respond));
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>
      <ToastProvider>{children}</ToastProvider>
    </QueryClientProvider>
  );
  return { client, ...renderHook(() => useDecideApproval(), { wrapper }) };
}

describe("useDecideApproval", () => {
  it("removes the approval before the POST answers, then invalidates", async () => {
    let finish!: (r: Partial<Response>) => void;
    const { client, result } = setup(() => new Promise((r) => (finish = r)));
    let done!: Promise<void>;
    act(() => {
      done = result.current.decide({ runId: "r1", callId: "k1" }, true);
    });
    expect(result.current.dismissed.has("k1")).toBe(true);
    expect(result.current.inflight.has("k1")).toBe(true);
    expect(client.getQueryData<Run>(chatKeys.run("r1"))!.pendingApprovals).toEqual([]);
    expect(client.getQueryData<ApprovalItem[]>(chatKeys.approvals)!.map((a) => a.callId)).toEqual(["k2"]);
    await act(async () => {
      finish({ ok: true, status: 200, statusText: "OK", json: async () => ({}) });
      await done;
    });
    expect(result.current.inflight.has("k1")).toBe(false);
    expect(result.current.dismissed.has("k1")).toBe(true);
    expect(client.getQueryState(chatKeys.run("r1"))!.isInvalidated).toBe(true);
  });

  it("puts the approval back when the POST fails", async () => {
    const { client, result } = setup(async () => ({ ok: false, status: 500, statusText: "boom", json: async () => ({ detail: "boom" }) }));
    await act(async () => {
      await result.current.decide({ runId: "r1", callId: "k1" }, false);
    });
    expect(result.current.dismissed.has("k1")).toBe(false);
    expect(client.getQueryData<Run>(chatKeys.run("r1"))!.pendingApprovals).toHaveLength(1);
    expect(client.getQueryData<ApprovalItem[]>(chatKeys.approvals)).toHaveLength(2);
  });

  it("keeps it removed on 409 (already decided)", async () => {
    const { client, result } = setup(async () => ({ ok: false, status: 409, statusText: "no", json: async () => ({ detail: "not pending" }) }));
    await act(async () => {
      await result.current.decide({ runId: "r1", callId: "k1" }, true);
    });
    await waitFor(() => expect(result.current.inflight.size).toBe(0));
    expect(result.current.dismissed.has("k1")).toBe(true);
    expect(client.getQueryData<ApprovalItem[]>(chatKeys.approvals)!.map((a) => a.callId)).toEqual(["k2"]);
  });
});
