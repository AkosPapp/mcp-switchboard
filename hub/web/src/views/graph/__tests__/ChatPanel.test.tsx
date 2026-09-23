import { afterEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { renderView } from "../../__tests__/helpers";
import ChatPanel from "../ChatPanel";
import { agentFixture, chatGraphFixture, graphFixture, grantFixture, serverFixture } from "../fixtures";
import { stubGraphHub } from "./helpers";

afterEach(() => vi.unstubAllGlobals());

const noop = () => {};

function panel(graph = chatGraphFixture(), agentId = "a2") {
  return renderView(
    <ChatPanel graph={graph} agentId={agentId} onClose={noop} onSpawnChild={noop} onDeleted={noop} />,
  );
}

describe("ChatPanel permissions", () => {
  it("lists live servers with source badges and greys offline grants", () => {
    const graph = chatGraphFixture({
      servers: [serverFixture(), serverFixture({ server: "git" })],
      grants: [
        ...graphFixture().grants,
        grantFixture({ agentId: "a2", server: "gone", connected: false, source: "human", orphaned: true }),
      ],
    });
    stubGraphHub(graph);
    panel(graph);

    const files = screen.getByTestId(/perm-laptop.*files/);
    expect(within(files).getByText("inherited")).toBeTruthy();
    expect(within(files).getByRole("switch").getAttribute("aria-checked")).toBe("true");

    const gone = screen.getByTestId(/perm-laptop.*gone/);
    expect(within(gone).getByText("not connected")).toBeTruthy();
    expect(within(gone).getByText("human")).toBeTruthy();
    expect(within(gone).getByText("orphaned by policy")).toBeTruthy();
    expect(gone.className).toContain("opacity-60");
  });

  it("shows the approval mode", () => {
    stubGraphHub(graphFixture());
    panel(chatGraphFixture({ agents: [agentFixture(), agentFixture({ id: "a2", parentId: "a1", name: "worker", approval: "destructive" })] }));
    expect(screen.getAllByText("destructive").length).toBeGreaterThan(0);
  });

  it("writes a narrowing toggle straight away", async () => {
    const { calls } = stubGraphHub(graphFixture());
    panel();
    await userEvent.click(screen.getByRole("switch", { name: "Allow laptop/files" }));
    await waitFor(() => expect(calls.some((c) => c.method === "PUT")).toBe(true));
    const put = calls.find((c) => c.method === "PUT")!;
    expect(put.path).toBe("agents/a2/grants");
    expect(put.body).toEqual({ grants: [{ label: "laptop", project: "", server: "files", allowed: false }] });
  });

  it("warns before widening beyond the parent, then writes on confirm", async () => {
    // parent holds files only; git is live but the parent lacks it
    const graph = chatGraphFixture({
      grants: [
        grantFixture({ agentId: "a1", server: "files", source: "explicit" }),
        grantFixture({ agentId: "a2", server: "files" }),
      ],
      servers: [serverFixture(), serverFixture({ server: "git" })],
    });
    const { calls } = stubGraphHub(graph);
    panel(graph);

    await userEvent.click(screen.getByRole("switch", { name: "Allow laptop/git" }));
    const warning = await screen.findByRole("alertdialog", { name: "Widen permissions" });
    expect(warning.textContent).toMatch(/human/);
    expect(calls.filter((c) => c.method === "PUT")).toHaveLength(0);

    await userEvent.click(within(warning).getByRole("button", { name: "Allow anyway" }));
    await waitFor(() => expect(calls.some((c) => c.method === "PUT")).toBe(true));
    expect(calls.find((c) => c.method === "PUT")!.body).toEqual({
      grants: [{ label: "laptop", project: "", server: "git", allowed: true }],
    });
  });

  it("does not warn for a root agent", async () => {
    const { calls } = stubGraphHub(graphFixture());
    panel(chatGraphFixture(), "a1");
    await userEvent.click(screen.getByRole("switch", { name: "Allow laptop/git" }));
    expect(screen.queryByRole("alertdialog")).toBeNull();
    await waitFor(() => expect(calls.some((c) => c.method === "PUT")).toBe(true));
  });
});

describe("ChatPanel settings and delete", () => {
  it("patches only changed run settings (the title is the chat's own setting)", async () => {
    const { calls } = stubGraphHub(graphFixture());
    panel();
    expect(screen.queryByLabelText("Name")).toBeNull();
    await userEvent.click(screen.getByLabelText(/auto-wake/));
    await userEvent.click(screen.getByRole("button", { name: "Save settings" }));
    await waitFor(() => expect(calls.some((c) => c.method === "PATCH")).toBe(true));
    expect(calls.find((c) => c.method === "PATCH")!.body).toEqual({ autoWake: true });
  });

  it("titles the panel with the chat and links to it", () => {
    stubGraphHub(graphFixture());
    panel();
    expect(screen.getByRole("heading", { name: "worker" })).toBeTruthy();
    expect(screen.getByRole("link", { name: "Open chat" }).getAttribute("href")).toBe("/chat/c-a2");
    expect(screen.getByRole("button", { name: "Spawn sub-chat" })).toBeTruthy();
  });

  it("confirms deletion, mentions sub-chats and deletes through the chat", async () => {
    const { calls } = stubGraphHub(graphFixture());
    panel(chatGraphFixture(), "a1");
    await userEvent.click(screen.getByRole("button", { name: "Delete chat…" }));
    const confirm = screen.getByRole("alertdialog", { name: "Confirm delete" });
    expect(confirm.textContent).toMatch(/1 sub-chat/);
    expect(calls.some((c) => c.method === "DELETE")).toBe(false);
    await userEvent.click(within(confirm).getByRole("button", { name: "Confirm delete" }));
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.path === "chats/c-a1")).toBe(true));
  });
});
