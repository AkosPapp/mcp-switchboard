import { expect, test, type Page } from "@playwright/test";

/**
 * The Prompts tab and the hub-tools section, against mocked profile/hub-tools
 * routes (the contract is docs/PROFILES_API.md), so they do not depend on the
 * hub build serving them.
 */

const profile = (over: Record<string, unknown>) => ({
  id: "p1",
  name: "Assistant",
  description: "",
  systemPrompt: "You are a helpful assistant.",
  model: null,
  capabilities: { canSpawn: false, canMessage: false },
  approval: "destructive",
  budget: {},
  isDefault: true,
  createdAt: "2026-09-20T10:00:00+00:00",
  updatedAt: "2026-09-20T10:00:00+00:00",
  ...over,
});

async function mock(page: Page) {
  const profiles = [profile({}), profile({ id: "p2", name: "Researcher", isDefault: false })];
  await page.route("**/api/models", (r) =>
    r.fulfill({ json: { models: [{ provider: "fake", model: "echo" }] } }),
  );
  await page.route("**/api/hub-tools", (r) =>
    r.fulfill({
      json: {
        tools: [
          {
            name: "switchboard.chat.spawn",
            description: "Create a child chat",
            inputSchema: { type: "object", properties: { name: { type: "string" } } },
            annotations: { openWorldHint: true },
            requires: "canSpawn",
          },
        ],
      },
    }),
  );
  await page.route("**/api/profiles", (r) => r.fulfill({ json: { profiles } }));
  await page.route("**/api/profiles/p1", (r) => {
    if (r.request().method() === "DELETE")
      return r.fulfill({ status: 409, json: { error: "cannot delete the default profile" } });
    return r.fulfill({ json: profiles[0] });
  });
}

test("prompts tab lists prompts and shows the delete-default 409", async ({ page }) => {
  await mock(page);
  await page.goto("/prompts");
  await expect(page.getByText("Researcher")).toBeVisible();
  await page.getByText("Assistant").first().click();
  await expect(page.getByRole("form", { name: "Prompt" })).toBeVisible();
  await expect(page.getByText("Can create sub-chats").first()).toBeVisible();
  await page.getByRole("button", { name: "Delete" }).click();
  await page.getByRole("button", { name: "Confirm delete" }).click();
  await expect(page.getByRole("alert")).toHaveText("cannot delete the default profile");
});

test("hub tools section expands with a schema", async ({ page }) => {
  await mock(page);
  await page.goto("/connections");
  await page.getByRole("button", { name: /Hub tools/ }).click();
  await expect(page.getByText("switchboard.chat.spawn")).toBeVisible();
  await expect(page.getByText("requires: canSpawn")).toBeVisible();
  await page.getByText("Show input schema").click();
  await expect(page.getByText('"properties"')).toBeVisible();
});

test("model picker shows context window, tool support and discovered grouping", async ({ page }) => {
  await mock(page);
  await page.route("**/api/models", (r) =>
    r.fulfill({
      json: {
        models: [
          { provider: "fake", model: "echo" },
          { provider: "ollama", model: "gpt-oss:20b", contextWindow: 32768, supportsTools: true, discovered: true },
          { provider: "ollama", model: "tiny", contextWindow: 131072, supportsTools: false, discovered: true },
        ],
      },
    }),
  );
  await page.goto("/prompts");
  await page.getByText("Assistant").first().click();
  const form = page.getByRole("form", { name: "Prompt" });
  const select = form.getByLabel("Default model");
  await expect(select.locator("option", { hasText: "ollama/gpt-oss:20b · 32k ctx" })).toHaveCount(1);
  await expect(select.locator("option", { hasText: "ollama/tiny · 128k ctx · no tool support" })).toHaveCount(1);
  await expect(select.locator("optgroup[label=Discovered] option")).toHaveCount(2);
  await expect(form.getByTestId("no-tools-warning")).toHaveCount(0);
  await select.selectOption("ollama/tiny");
  await expect(form.getByText("this model does not advertise tool calling")).toBeVisible();
});
