import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import HubTools from "../HubTools";
import { renderView, stubHub } from "./helpers";

const tools = {
  tools: [
    {
      name: "switchboard.chat.spawn",
      description: "Create a child chat",
      inputSchema: { type: "object", properties: { name: { type: "string" } } },
      annotations: { destructiveHint: false, openWorldHint: true, idempotentHint: true },
      requires: "canSpawn",
    },
    {
      name: "switchboard.graph.read",
      description: "Read the graph",
      inputSchema: {},
      annotations: { readOnlyHint: true },
      requires: "always",
    },
  ],
};

afterEach(() => vi.unstubAllGlobals());

describe("HubTools", () => {
  it("renders nothing and does not fetch when the orchestrator is off", () => {
    const fetchMock = stubHub({});
    renderView(<HubTools enabled={false} />);
    expect(screen.queryByText("Hub tools")).toBeNull();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("lists tools with chips and an expandable schema", async () => {
    stubHub({ "hub-tools": tools });
    const user = userEvent.setup();
    renderView(<HubTools enabled={true} />);
    await user.click(screen.getByRole("button", { name: /Hub tools/ }));
    expect(await screen.findByText("switchboard.chat.spawn")).toBeInTheDocument();
    expect(screen.getByText("open-world")).toBeInTheDocument();
    expect(screen.getByText("idempotent")).toBeInTheDocument();
    expect(screen.getByText("read-only")).toBeInTheDocument();
    expect(screen.getByText("requires: canSpawn")).toBeInTheDocument();
    expect(screen.queryByText(/"properties"/)).toBeNull();
    await user.click(screen.getAllByText("Show input schema")[0]);
    expect(screen.getByText(/"properties"/)).toBeInTheDocument();
  });
});
