import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ToastProvider } from "../../components/Toast";
import type { CallRecord, Snapshot } from "../../api/types";
import Connections from "../Connections";

const snapshot: Snapshot = {
  connections: [
    {
      id: "c1",
      label: "legion5",
      client: { name: "mcp-switchboard-client", version: "0.3.0", instance: "i", label: "legion5" },
      connectedAt: "2026-01-02T03:04:05+00:00",
      servers: [
        {
          name: "demo",
          project: null,
          command: "uvx demo",
          state: "running",
          error: null,
          exitCode: null,
          toolCount: 2,
          tools: [
            {
              name: "echo",
              exposedName: "legion5__demo__echo",
              title: "echo · demo @ legion5",
              description: "says it back",
              inputSchema: {
                type: "object",
                properties: { message: { type: "string" } },
                required: ["message"],
              },
            },
            {
              name: "shout",
              exposedName: null,
              title: null,
              description: null,
              inputSchema: { type: "object" },
            },
          ],
        },
        {
          name: "lsp",
          project: "nix",
          command: "nixd",
          state: "failed",
          error: "it died",
          exitCode: 1,
          toolCount: 0,
          tools: [],
        },
      ],
    },
  ],
};

const okCall: CallRecord = {
  id: "call-1",
  connectionId: "c1",
  label: "legion5",
  server: "demo",
  tool: "echo",
  exposedName: "legion5__demo__echo",
  arguments: { message: "hi" },
  result: { content: [{ type: "text", text: "echo: hi" }] },
  error: null,
  status: "ok",
  source: "console",
  startedAt: "2026-01-02T03:04:05+00:00",
  durationMs: 12.5,
};

function stubFetch(handler: (url: string, init?: RequestInit) => unknown) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const body = handler(String(input), init);
      return {
        ok: true,
        status: 200,
        statusText: "OK",
        json: async () => body,
      } as Response;
    }),
  );
}

function renderView(entry = "/connections") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[entry]} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
        <ToastProvider>
          <Connections />
        </ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  localStorage.clear();
  stubFetch(() => snapshot);
});
afterEach(() => vi.unstubAllGlobals());

describe("Connections memory", () => {
  it("brings the filter text back after the view is left and reopened", async () => {
    const user = userEvent.setup();
    const first = renderView();
    await screen.findByText("echo");
    await user.type(screen.getByPlaceholderText("filter tools, servers, machines"), "lsp");
    first.unmount();

    renderView();
    expect(screen.getByPlaceholderText("filter tools, servers, machines")).toHaveValue("lsp");
    await screen.findByText("lsp");
    expect(screen.queryByText("echo")).not.toBeInTheDocument();
  });

  it("works when storage throws", async () => {
    const spy = vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("blocked");
    });
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("blocked");
    });
    const user = userEvent.setup();
    renderView();
    await screen.findByText("echo");
    await user.type(screen.getByPlaceholderText("filter tools, servers, machines"), "x");
    expect(spy).toHaveBeenCalled();
    vi.restoreAllMocks();
  });

  it("reopens on the tool named by a remembered location", async () => {
    renderView("/connections?connection=c1&server=demo&tool=echo");
    const row = await screen.findByRole("treeitem", { name: /echo/ });
    expect(row).toHaveAttribute("aria-selected", "true");
  });
});

describe("Connections", () => {
  it("shows every machine, server and tool", async () => {
    renderView();

    expect(await screen.findByText("legion5")).toBeInTheDocument();
    expect(screen.getByText("demo")).toBeInTheDocument();
    expect(screen.getByText("lsp")).toBeInTheDocument();
    expect(screen.getByText("nix")).toBeInTheDocument();
    expect(screen.getByText("echo")).toBeInTheDocument();
  });

  it("surfaces a failed server's error in the tree", async () => {
    renderView();
    expect(await screen.findByText("it died")).toBeInTheDocument();
  });

  // A tool dropped under I4 exists upstream but cannot be called, and saying so
  // is the difference between "missing" and "unusable".
  it("marks a tool that is not exposed", async () => {
    renderView();
    await screen.findByText("shout");
    expect(screen.getByTitle("not exposed")).toBeInTheDocument();
  });

  it("filters by tool, server and machine name", async () => {
    const user = userEvent.setup();
    renderView();
    await screen.findByText("echo");

    const filter = screen.getByPlaceholderText("filter tools, servers, machines");

    await user.type(filter, "shout");
    await waitFor(() => expect(screen.queryByText("echo")).not.toBeInTheDocument());
    expect(screen.getByText("shout")).toBeInTheDocument();

    // A server name keeps that server's whole subtree.
    await user.clear(filter);
    await user.type(filter, "demo");
    await waitFor(() => expect(screen.getByText("echo")).toBeInTheDocument());
    expect(screen.queryByText("lsp")).not.toBeInTheDocument();
  });

  it("counts what is connected", async () => {
    renderView();
    expect(await screen.findByText(/1 machine · 2 servers · 2 tools/)).toBeInTheDocument();
  });

  it("opens the tool panel on selection", async () => {
    const user = userEvent.setup();
    renderView();

    await user.click(await screen.findByText("echo"));

    expect(await screen.findByRole("heading", { name: "echo" })).toBeInTheDocument();
    expect(screen.getByText("legion5__demo__echo")).toBeInTheDocument();
    expect(screen.getByText("says it back")).toBeInTheDocument();
  });

  it("builds a form from the schema and calls the tool", async () => {
    const user = userEvent.setup();
    let posted: RequestInit | undefined;
    stubFetch((url, init) => {
      if (url.includes("/call")) {
        posted = init;
        return okCall;
      }
      return snapshot;
    });

    renderView();
    await user.click(await screen.findByText("echo"));

    await user.type(screen.getByLabelText(/message/), "hi");
    await user.click(screen.getByRole("button", { name: "Call tool" }));

    await waitFor(() => expect(posted).toBeDefined());
    expect(JSON.parse(String(posted?.body))).toEqual({ arguments: { message: "hi" } });

    // N1: the result renders because the call happened, whatever it returned.
    expect(await screen.findByText("Result")).toBeInTheDocument();
    expect(screen.getByText("ok")).toBeInTheDocument();
  });

  it("refuses to call when a required argument is missing", async () => {
    const user = userEvent.setup();
    const calls: string[] = [];
    stubFetch((url) => {
      calls.push(url);
      return snapshot;
    });

    renderView();
    await user.click(await screen.findByText("echo"));
    await user.click(screen.getByRole("button", { name: "Call tool" }));

    expect(await screen.findByRole("status")).toHaveTextContent("message is required");
    expect(calls.some((url) => url.includes("/call"))).toBe(false);
  });

  it("reports invalid JSON rather than sending it", async () => {
    const user = userEvent.setup();
    renderView();
    await user.click(await screen.findByText("echo"));

    await user.click(screen.getByRole("button", { name: "Raw JSON" }));
    const editor = screen.getByLabelText("Arguments as JSON");
    await user.clear(editor);
    await user.type(editor, "{{not json");
    await user.click(screen.getByRole("button", { name: "Call tool" }));

    expect(await screen.findByText(/invalid JSON/)).toBeInTheDocument();
  });

  // A tool whose schema has nothing a form can render must not offer one.
  it("locks a schemaless tool into JSON mode", async () => {
    const user = userEvent.setup();
    renderView();
    await user.click(await screen.findByText("shout"));

    expect(await screen.findByLabelText("Arguments as JSON")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Form" })).toBeDisabled();
  });

  it("says when the selected tool has gone away", async () => {
    const user = userEvent.setup();
    renderView();
    await user.click(await screen.findByText("echo"));
    await screen.findByRole("heading", { name: "echo" });

    // The machine disconnects: the panel must notice rather than keep showing a
    // tool nobody can call.
    stubFetch(() => ({ connections: [] }) as Snapshot);
    await user.click(screen.getByRole("button", { name: "Refresh" }));

    expect(
      await screen.findByText(/no longer exposed by the hub/),
    ).toBeInTheDocument();
  });

  it("asks the hub to restart a server", async () => {
    const user = userEvent.setup();
    const seen: string[] = [];
    stubFetch((url) => {
      seen.push(url);
      return snapshot;
    });

    renderView();
    await user.click(await screen.findByText("echo"));
    await user.click(screen.getByRole("button", { name: "Restart server" }));

    await waitFor(() =>
      expect(seen.some((url) => url.endsWith("/servers/demo/restart"))).toBe(true),
    );
  });

  it("explains itself when nothing is connected", async () => {
    stubFetch(() => ({ connections: [] }) as Snapshot);
    renderView();

    const empty = await screen.findByText("Nothing has dialled in yet.");
    expect(within(empty.parentElement as HTMLElement).getByText(/Endpoints tab/)).toBeInTheDocument();
  });

  // The Calls view links here with the recorded arguments in the query string,
  // which is how a logged call is re-run with a tweak.
  it("opens prefilled from a link that carries arguments", async () => {
    const args = encodeURIComponent(JSON.stringify({ message: "from the log" }));
    renderView(`/connections?connection=c1&server=demo&tool=echo&args=${args}`);

    const field = (await screen.findByLabelText(/message/)) as HTMLInputElement;
    expect(field.value).toBe("from the log");
  });

  // Anything the form cannot represent must not be silently dropped on the way
  // in, so the panel opens in JSON mode instead.
  it("opens a prefill the form cannot represent in JSON mode", async () => {
    const args = encodeURIComponent(JSON.stringify({ message: "hi", extra: { deep: true } }));
    renderView(`/connections?connection=c1&server=demo&tool=echo&args=${args}`);

    const editor = (await screen.findByLabelText("Arguments as JSON")) as HTMLTextAreaElement;
    expect(JSON.parse(editor.value)).toEqual({ message: "hi", extra: { deep: true } });
  });
});

describe("Connections environment", () => {
  const withEnv: Snapshot = {
    connections: [
      {
        ...snapshot.connections[0],
        client: {
          ...snapshot.connections[0].client,
          environment: {
            kinds: ["devcontainer", "direnv"],
            project: "orbit",
            workspace: "/work/orbit",
          },
        },
      },
      {
        ...snapshot.connections[0],
        id: "c2",
        label: "oldbox",
        client: { ...snapshot.connections[0].client, label: "oldbox" },
        servers: [{ ...snapshot.connections[0].servers[0], name: "other" }],
      },
    ],
  };

  it("shows project and chips, and says so for old clients", async () => {
    stubFetch(() => withEnv);
    renderView();
    await screen.findAllByText("orbit");
    expect(screen.getByText("devcontainer")).toBeInTheDocument();
    expect(screen.getByText("direnv")).toBeInTheDocument();
    expect(screen.getByText(/environment not reported/)).toBeInTheDocument();
  });

  it.each(["devcontainer", "orbit", "/work/orbit"])("filters by %s", async (needle) => {
    const user = userEvent.setup();
    stubFetch(() => withEnv);
    renderView();
    await screen.findAllByText("orbit");
    await user.type(screen.getByPlaceholderText("filter tools, servers, machines"), needle);
    expect(screen.getByText("orbit")).toBeInTheDocument();
    expect(screen.queryByText("oldbox")).not.toBeInTheDocument();
  });

  it("shows the environment and workspace in the detail header", async () => {
    stubFetch(() => withEnv);
    renderView("/connections?connection=c1&server=demo&tool=echo");
    const header = await screen.findByTestId("connection-env");
    expect(within(header).getByText("devcontainer")).toBeInTheDocument();
    expect(within(header).getByText("/work/orbit")).toBeInTheDocument();
  });
});
