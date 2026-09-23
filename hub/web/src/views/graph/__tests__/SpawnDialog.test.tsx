import { afterEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { renderView } from "../../__tests__/helpers";
import { chatGraphFixture, graphFixture } from "../fixtures";
import SpawnDialog from "../SpawnDialog";
import { stubGraphHub } from "./helpers";

afterEach(() => vi.unstubAllGlobals());

const parent = chatGraphFixture().agents[0];

describe("SpawnDialog", () => {
  it("creates a sub-chat through POST /api/chats with parentChatId and the parent's model", async () => {
    const { calls } = stubGraphHub(graphFixture());
    const created = vi.fn();
    renderView(<SpawnDialog parent={parent} onClose={() => {}} onCreated={created} />);

    expect(screen.getByLabelText("Model").textContent).toContain("sonnet");
    await userEvent.type(screen.getByLabelText("Title"), "kid");
    await userEvent.click(screen.getByRole("button", { name: "Create" }));

    await waitFor(() => expect(created).toHaveBeenCalledWith("new"));
    const post = calls.find((c) => c.method === "POST")!;
    expect(post.path).toBe("chats");
    expect(post.body).toEqual({
      parentChatId: "c-a1",
      clientLabel: null,
      title: "kid",
      model: { provider: "anthropic", model: "sonnet" },
    });
  });

  it("sends its own prompt only when one is typed, and never creates a root", async () => {
    const { calls } = stubGraphHub(graphFixture());
    renderView(<SpawnDialog parent={parent} onClose={() => {}} onCreated={() => {}} />);
    await userEvent.type(screen.getByLabelText(/System prompt/), "Be brief.");
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST")).toBe(true));
    const body = calls.find((c) => c.method === "POST")!.body as Record<string, unknown>;
    expect(body).toMatchObject({ parentChatId: "c-a1", profileId: null, systemPrompt: "Be brief." });
  });
});
