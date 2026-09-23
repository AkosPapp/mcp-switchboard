import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { Route, Routes } from "react-router-dom";

import { renderView } from "../../__tests__/helpers";
import { initialValues, toInput } from "../form";
import type { Profile } from "../api";
import PromptsView from "../PromptsView";

const base: Profile = {
  id: "p1",
  name: "Assistant",
  description: "general",
  systemPrompt: "You are a helpful assistant.",
  model: null,
  capabilities: { canSpawn: false, canMessage: false },
  approval: "destructive",
  budget: {},
  isDefault: true,
  createdAt: "2026-09-20T10:00:00+00:00",
  updatedAt: "2026-09-20T10:00:00+00:00",
};
const second: Profile = {
  ...base,
  id: "p2",
  name: "Researcher",
  isDefault: false,
  capabilities: { canSpawn: true, canMessage: true },
  model: { provider: "anthropic", model: "sonnet" },
};

interface Call {
  method: string;
  path: string;
  body?: Record<string, unknown>;
}

function stub(opts: { deleteError?: string } = {}) {
  const calls: Call[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input).replace(/^api\//, "");
      const method = init?.method ?? "GET";
      const body = init?.body ? JSON.parse(String(init.body)) : undefined;
      calls.push({ method, path, body });
      const ok = (payload: unknown, status = 200) =>
        ({ ok: true, status, statusText: "OK", json: async () => payload }) as unknown as Response;
      if (path === "profiles" && method === "GET") return ok({ profiles: [base, second] });
      if (path === "models")
        return ok({ models: [{ provider: "anthropic", model: "sonnet" }] });
      if (path === "profiles" && method === "POST") return ok({ ...base, id: "p3", ...body }, 201);
      if (method === "PATCH") return ok({ ...base });
      if (method === "DELETE" && opts.deleteError)
        return {
          ok: false,
          status: 409,
          statusText: "Conflict",
          json: async () => ({ error: opts.deleteError }),
        } as unknown as Response;
      throw new Error(`no fixture for ${method} ${path}`);
    }),
  );
  return calls;
}

afterEach(() => vi.unstubAllGlobals());

describe("toInput", () => {
  it("requires a name and a system prompt", () => {
    const r = toInput({ ...initialValues(null) }, null);
    expect(r.errors).toEqual({
      name: "Name is required",
      systemPrompt: "System prompt is required",
    });
  });

  it("rejects a budget that is not a JSON object of numbers", () => {
    const v = { ...initialValues(base) };
    expect(toInput({ ...v, budget: "[1]" }, base).errors?.budget).toBeTruthy();
    expect(toInput({ ...v, budget: "{" }, base).errors?.budget).toBeTruthy();
    expect(toInput({ ...v, budget: '{"a":"x"}' }, base).errors?.budget).toBeTruthy();
    expect(toInput({ ...v, budget: "" }, base).input?.budget).toEqual({});
  });

  it("maps the model select to null or provider/model", () => {
    const v = initialValues(base);
    expect(toInput(v, base).input?.model).toBeNull();
    expect(toInput({ ...v, modelKey: "openai/gpt/4" }, base).input?.model).toEqual({
      provider: "openai",
      model: "gpt/4",
    });
  });
});

/** The view reads :profileId, so it is rendered under the real routes. */
const promptsRoutes = () => (
  <Routes>
    <Route path="/" element={<PromptsView />} />
    <Route path="/prompts" element={<PromptsView />} />
    <Route path="/prompts/:profileId" element={<PromptsView />} />
  </Routes>
);

describe("PromptsView", () => {
  it("lists profiles with chips and the default badge", async () => {
    stub();
    renderView(promptsRoutes());
    expect(await screen.findByText("Researcher")).toBeInTheDocument();
    expect(screen.getByText("default")).toBeInTheDocument();
    expect(screen.getByText("anthropic/sonnet")).toBeInTheDocument();
    expect(screen.getByText("can create sub-chats")).toBeInTheDocument();
    expect(screen.getByText("can message chats")).toBeInTheDocument();
  });

  it("shows validation errors and does not post an invalid form", async () => {
    const calls = stub();
    const user = userEvent.setup();
    renderView(promptsRoutes());
    await user.click(await screen.findByRole("button", { name: "New prompt" }));
    await user.click(screen.getByRole("button", { name: "Create prompt" }));
    expect(screen.getByText("Name is required")).toBeInTheDocument();
    expect(calls.some((c) => c.method === "POST")).toBe(false);
  });

  it("creates a profile with the chosen hub tools", async () => {
    const calls = stub();
    const user = userEvent.setup();
    renderView(promptsRoutes());
    await user.click(await screen.findByRole("button", { name: "New prompt" }));
    await user.type(screen.getByLabelText(/^Name/), "Coder");
    await user.type(screen.getByLabelText(/^System prompt/), "Write code");
    await user.click(screen.getByLabelText(/Can create sub-chats/));
    await user.click(screen.getByRole("button", { name: "Create prompt" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST")).toBe(true));
    const post = calls.find((c) => c.method === "POST")!;
    expect(post.body).toMatchObject({
      name: "Coder",
      systemPrompt: "Write code",
      capabilities: { canSpawn: true, canMessage: false },
      model: null,
    });
  });

  it("makes a profile the default", async () => {
    const calls = stub();
    const user = userEvent.setup();
    renderView(promptsRoutes());
    await user.click(await screen.findByText("Researcher"));
    await user.click(screen.getByRole("button", { name: "Make default" }));
    await waitFor(() =>
      expect(calls.find((c) => c.method === "PATCH")).toMatchObject({
        path: "profiles/p2",
        body: { isDefault: true },
      }),
    );
  });

  it("surfaces the 409 message when deleting the default", async () => {
    stub({ deleteError: "cannot delete the default profile" });
    const user = userEvent.setup();
    renderView(promptsRoutes());
    await user.click(await screen.findByText("Assistant"));
    expect(screen.queryByRole("button", { name: "Make default" })).toBeNull();
    await user.click(screen.getByRole("button", { name: "Delete" }));
    await user.click(screen.getByRole("button", { name: "Confirm delete" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("cannot delete the default profile");
  });
});
