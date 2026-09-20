import { afterEach, describe, expect, it, vi } from "vitest";
import { screen, within } from "@testing-library/react";

import EndpointsView from "../Endpoints";
import { endpointsFixture, renderView, stubHub } from "./helpers";

afterEach(() => vi.unstubAllGlobals());

describe("Endpoints", () => {
  it("shows the install command and why it is only shown here", async () => {
    stubHub({
      endpoints: endpointsFixture({
        publicUrl: "https://hub.example.ts.net",
        installCommand: "uvx mcp-switchboard-client --url https://hub.example.ts.net --token abc123",
      }),
    });
    renderView(<EndpointsView />, "/endpoints");

    expect(
      await screen.findByText(
        "uvx mcp-switchboard-client --url https://hub.example.ts.net --token abc123",
      ),
    ).toBeTruthy();
    expect(screen.getByText(/carries the tunnel token/)).toBeTruthy();
  });

  it("explains the unset public URL when there is no install command", async () => {
    stubHub({ endpoints: endpointsFixture() });
    renderView(<EndpointsView />, "/endpoints");

    expect(await screen.findByText("MCP_SWITCHBOARD_PUBLIC_URL")).toBeTruthy();
    expect(screen.getByText(/externally-reachable address/)).toBeTruthy();
    expect(screen.queryByText(/carries the tunnel token/)).toBeNull();
  });

  it("composes each row's URL from the local base URL and the path", async () => {
    stubHub({ endpoints: endpointsFixture() });
    renderView(<EndpointsView />, "/endpoints");

    const table = within(await screen.findByRole("table"));
    expect(table.getByText("http://127.0.0.1:8099/mcp")).toBeTruthy();
    expect(table.getByText("http://127.0.0.1:8099/mcp/host/laptop")).toBeTruthy();
    expect(table.getByText("laptop__files__read_file")).toBeTruthy();
    expect(table.getByText("Every connected machine, names prefixed by label.")).toBeTruthy();
  });

  it("shows the hub's message when the endpoints fail to load", async () => {
    stubHub({}, { errors: { endpoints: { status: 503, detail: "registry is starting" } } });
    renderView(<EndpointsView />, "/endpoints");

    expect(await screen.findByText(/registry is starting/)).toBeTruthy();
  });

  it("says so when no scope is reachable", async () => {
    stubHub({ endpoints: endpointsFixture({ rows: [] }) });
    renderView(<EndpointsView />, "/endpoints");

    expect(await screen.findByText("No endpoints are reachable yet.")).toBeTruthy();
  });
});
