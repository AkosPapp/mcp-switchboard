import { expect, test, type APIRequestContext, type Locator } from "@playwright/test";

import { makeChat as makeChatWith } from "./fixtures";

/**
 * One chat, end to end, against the shipped hub and a mock OpenAI-compatible
 * server (spec.md V7). Runs at desktop and phone width; on the phone the chat
 * list is a drawer, which the flow has to open to pick a chat.
 */

const run = Date.now().toString(36);

const makeChat = (request: APIRequestContext, _name: string, title: string, approval = "never") =>
  makeChatWith(request, title, { approval });

function composer(page: import("@playwright/test").Page): Locator {
  return page.getByLabel("message", { exact: true });
}

test("a chat: streamed answer, regenerate, sibling navigation", async ({ page, request }) => {
  const chat = await makeChat(request, "chatty", `flow-${run}`);

  await page.goto(`/chat/${chat.id}`);
  await expect(page.getByRole("heading", { name: `flow-${run}` })).toBeVisible();

  await composer(page).fill("hello there");
  await page.getByRole("button", { name: "Send", exact: true }).click();

  const thread = page.getByTestId("thread");
  await expect(thread.getByText("hello there")).toBeVisible();
  // The mock numbers its answers across the whole run, so compare, not count.
  const answer = thread.getByText(/Mock answer \d+/);
  // Regenerate only exists on the persisted message, so the stream is done.
  await expect(thread.getByRole("button", { name: "Regenerate" })).toBeVisible();
  const first = await answer.innerText();
  // Markdown was rendered and the code block is copyable.
  await expect(thread.locator("strong", { hasText: "hello" })).toBeVisible();
  await expect(thread.getByRole("button", { name: "Copy code" })).toBeVisible();

  await thread.getByRole("button", { name: "Regenerate" }).click();
  await expect(thread.getByLabel("version 2 of 2")).toBeVisible();
  await expect(answer).toBeVisible();
  expect(await answer.innerText()).not.toBe(first);

  await thread.getByLabel("previous version").click();
  await expect(thread.getByLabel("version 1 of 2")).toBeVisible();
  await expect(answer).toHaveText(first);

  // The composer stays on screen at the bottom of the viewport.
  const box = await composer(page).boundingBox();
  const viewport = page.viewportSize()!;
  expect(box!.y + box!.height).toBeLessThanOrEqual(viewport.height);
});

test("the list finds the chat by title and opens it", async ({ page, request }) => {
  const title = `findme-${run}`;
  const chat = await makeChat(request, "lister", title);

  await page.goto("/chat");
  await page.getByLabel("search chats").fill(title);
  await page.getByRole("link", { name: new RegExp(title) }).click();
  await expect(page).toHaveURL(new RegExp(`/chat/${chat.id}$`));
  await expect(page.getByRole("heading", { name: title })).toBeVisible();
});

test("shows a tool call as a card that expands to the raw JSON", async ({ page, request }) => {
  const chat = await makeChat(request, "tooler", `tool-${run}`);

  await page.goto(`/chat/${chat.id}`);
  await composer(page).fill("please use the tool");
  await page.getByRole("button", { name: "Send", exact: true }).click();

  const card = page.getByTestId("tool-card").first();
  await expect(card).toBeVisible({ timeout: 20_000 });
  await card.getByRole("button", { name: /echo/ }).click();
  await expect(card.getByText(/"message": "from the model"/)).toBeVisible();
  await expect(card.getByText("echo: from the model").first()).toBeVisible();
  // The call row exists, so the card resolves where it ran and links to it.
  await expect(card.getByRole("link", { name: "open in Calls" })).toHaveAttribute("href", /\/calls\?call=/);
  await expect(page.getByTestId("thread").getByText("The tool answered.")).toBeVisible({ timeout: 20_000 });
});

test("a blocked tool call shows an approval card; approving lets the run finish", async ({ page, request }) => {
  const chat = await makeChat(request, "careful", `approve-${run}`, "always");

  await page.goto(`/chat/${chat.id}`);
  await composer(page).fill("please use the tool");
  await page.getByRole("button", { name: "Send", exact: true }).click();

  const approval = page.getByTestId("approval-card");
  await expect(approval).toBeVisible({ timeout: 20_000 });
  await expect(approval.getByText(/auto-deny in/)).toBeVisible();
  await expect(approval.getByText(/from the model/)).toBeVisible();
  await approval.getByRole("button", { name: "Approve" }).click();

  await expect(approval).toHaveCount(0);
  await expect(page.getByTestId("thread").getByText("The tool answered.")).toBeVisible({ timeout: 20_000 });
});

test("has no horizontal page scroll", async ({ page, request }) => {
  const chat = await makeChat(request, "narrow", `narrow-${run}`);
  await page.goto(`/chat/${chat.id}`);
  await expect(page.getByTestId("thread")).toBeVisible();
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  );
  expect(overflow).toBeLessThanOrEqual(0);
});

test("the Tools panel lists the chat's tools", async ({ page, request }) => {
  const chat = await makeChat(request, "tools", `tools-${run}`);
  await page.route(`**/api/chats/${chat.id}/tools`, (route) =>
    route.fulfill({
      json: {
        clientLabel: "e2ebox",
        clientConnected: false,
        tools: [{ name: "switchboard.chat.spawn", description: "Spawn a child", origin: "hub", inputSchema: { type: "object" } }],
      },
    }),
  );
  await page.goto(`/chat/${chat.id}`);
  await page.getByRole("button", { name: "Tools", exact: true }).click();
  const panel = page.getByRole("dialog", { name: "tools this chat can use" });
  await expect(panel.getByText(/client offline/i)).toBeVisible();
  await expect(panel.getByText("switchboard.chat.spawn")).toBeVisible();
  await panel.getByRole("button", { name: "Show input schema" }).click();
  await expect(panel.getByText(/"type": "object"/)).toBeVisible();
});
