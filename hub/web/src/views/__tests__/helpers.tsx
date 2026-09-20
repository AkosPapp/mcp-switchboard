import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";
import type { ReactElement } from "react";
import { vi } from "vitest";

import { ToastProvider } from "../../components/Toast";
import type { CallRecord, Endpoints, Stats } from "../../api/types";

/** A hub that answers whatever the test hands it, keyed by path prefix. */
export type Routes = Record<string, unknown>;

export interface StubOptions {
  /** Paths (as passed to the client, e.g. "calls") that fail, with the hub's message. */
  errors?: Record<string, { status: number; detail: string }>;
}

export function stubHub(routes: Routes, options: StubOptions = {}) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input).replace(/^api\//, "");
    const path = url.split("?")[0];

    const failure = options.errors?.[path];
    if (failure) {
      return {
        ok: false,
        status: failure.status,
        statusText: "error",
        json: async () => ({ detail: failure.detail }),
      } as unknown as Response;
    }

    const body = routes[path];
    if (body === undefined) throw new Error(`no fixture for ${path}`);
    return {
      ok: true,
      status: 200,
      statusText: "OK",
      json: async () => body,
    } as unknown as Response;
  });

  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

/** Shows the current query string, so a test can assert a filter is linkable. */
function LocationProbe() {
  const location = useLocation();
  return <div data-testid="search">{location.search}</div>;
}

export function renderView(ui: ReactElement, initialEntry = "/") {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });

  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[initialEntry]}>
        <ToastProvider>
          {ui}
          <LocationProbe />
        </ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

export function callFixture(overrides: Partial<CallRecord> = {}): CallRecord {
  return {
    id: "call-1",
    connectionId: "conn-1",
    label: "laptop",
    server: "files",
    tool: "read_file",
    exposedName: "laptop__files__read_file",
    arguments: { path: "/etc/hosts" },
    result: { content: "127.0.0.1" },
    error: null,
    status: "ok",
    source: "mcp",
    startedAt: "2026-09-20T10:00:00Z",
    durationMs: 42,
    agentId: null,
    chatId: null,
    runId: null,
    ...overrides,
  };
}

export const statsFixture: Stats = {
  databaseBytes: 4096,
  rows: { calls: 2 },
  calls: { total: 2, ok: 1, error: 1 },
};

export function endpointsFixture(overrides: Partial<Endpoints> = {}): Endpoints {
  return {
    localBaseUrl: "http://127.0.0.1:8099",
    publicUrl: null,
    installCommand: null,
    rows: [
      {
        path: "/mcp",
        scope: "all",
        description: "Every connected machine, names prefixed by label.",
        example: "laptop__files__read_file",
      },
      {
        path: "/mcp/host/laptop",
        scope: "host",
        description: "One machine's servers.",
        example: "files__read_file",
      },
    ],
    ...overrides,
  };
}
