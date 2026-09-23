import { expect, test, type APIRequestContext } from "@playwright/test";

import { makeChat as makeChatWith } from "./fixtures";

/**
 * The browser remembers where you were under each tab (localStorage only), so
 * looking at a connection and coming back reopens the same chat.
 */

const run = Date.now().toString(36);

const makeChat = (request: APIRequestContext, title: string) => makeChatWith(request, title);

const tab = (page: import("@playwright/test").Page, name: string) =>
  page.getByRole("navigation").getByRole("link", { name, exact: true });

test("chat and tool selection are each remembered across tabs", async ({ page, request }) => {
  const title = `memo-${run}`;
  const chat = await makeChat(request, title);

  await page.goto(`/chat/${chat.id}`);
  await expect(page.getByRole("heading", { name: title, exact: true })).toBeVisible();

  await tab(page, "Connections").click();
  await expect(page).toHaveURL(/\/connections$/);
  await page.getByRole("treeitem", { name: /echo/ }).click();
  await expect(page).toHaveURL(/tool=echo/);

  // Back to the chat that was open.
  await tab(page, "Chat").click();
  await expect(page).toHaveURL(new RegExp(`/chat/${chat.id}$`));
  await expect(page.getByRole("heading", { name: title, exact: true })).toBeVisible();

  // And the same tool is still selected.
  await tab(page, "Connections").click();
  await expect(page).toHaveURL(/connection=.*server=demo.*tool=echo/);

  // Clicking the tab you are on deselects.
  await tab(page, "Connections").click();
  await expect(page).toHaveURL(/\/connections$/);
});

test("reload on / restores the last place", async ({ page, request }) => {
  const chat = await makeChat(request, `reload-${run}`);
  await page.goto(`/chat/${chat.id}`);
  await expect(page.getByRole("heading", { name: `reload-${run}`, exact: true })).toBeVisible();

  await page.goto("/");
  await expect(page).toHaveURL(new RegExp(`/chat/${chat.id}$`));

  // A deep link typed into the bar wins over memory.
  await page.goto("/calls");
  await expect(page).toHaveURL(/\/calls$/);
});

test("a remembered chat that was deleted falls back to the bare tab", async ({ page, request }) => {
  const chat = await makeChat(request, `gone-${run}`);
  const removed = await request.delete(`/api/chats/${chat.id}`);
  expect(removed.ok()).toBeTruthy();

  // What the console would have written when the chat was last open.
  await page.goto("/calls");
  await page.evaluate((id) => {
    const route = { tabs: { "/chat": `/chat/${id}` }, last: `/chat/${id}` };
    localStorage.setItem("mcpsb.ui.v1.route", JSON.stringify({ v: 1, d: route }));
  }, chat.id);
  await page.goto("/calls");

  await tab(page, "Chat").click();
  await expect(page).toHaveURL(/\/chat$/);

  // The memory was dropped, so the next visit does not bounce again.
  const stored = await page.evaluate(() => localStorage.getItem("mcpsb.ui.v1.route"));
  expect(stored ?? "").not.toContain(chat.id);
  await tab(page, "Calls").click();
  await tab(page, "Chat").click();
  await expect(page).toHaveURL(/\/chat$/);
});

test("the chat list filter is remembered", async ({ page, request }) => {
  await makeChat(request, `filter-${run}`);
  await page.goto("/chat");
  const search = page.getByRole("searchbox").first();
  await search.fill(`filter-${run}`);
  await tab(page, "Calls").click();
  await tab(page, "Chat").click();
  await expect(page.getByRole("searchbox").first()).toHaveValue(`filter-${run}`);
});

test("hover archives a chat at once; the archived view offers unarchive", async ({ page, request }) => {
  const title = `arch-${run}`;
  const chat = await makeChat(request, title);
  await page.goto(`/chat/${chat.id}`);
  await expect(page.getByRole("heading", { name: title, exact: true })).toBeVisible();

  const drawer = page.getByRole("button", { name: "Chats", exact: true });
  // On a phone the list is a slide-over behind a button in the thread header.
  if (await drawer.first().isVisible().catch(() => false)) await drawer.first().click();

  // The shared hub accumulates chats from every spec in the whole run (one hub
  // for the whole suite - see fixtures.ts), so by the time this test runs the
  // list can be long enough that the row sits well below the fold of the
  // phone's slide-over; filter down to the one chat this test cares about
  // rather than lean on scroll-into-view finding it in a long list.
  await page.getByLabel("search chats").fill(title);
  const row = page.getByTestId("chat-row").filter({ hasText: title });
  await row.hover();
  await row.getByRole("button", { name: "Archive", exact: true }).click(); // no confirm dialog

  // It was the open chat: back to the bare tab, and it is gone from the list.
  await expect(page).toHaveURL(/\/chat$/);
  await expect(page.getByTestId("chat-list").getByText(title)).toHaveCount(0);
  const stored = await page.evaluate(() => localStorage.getItem("mcpsb.ui.v1.route"));
  expect(stored ?? "").not.toContain(chat.id);

  // Show archived chats, and bring it back.
  await page.getByLabel("archived").check();
  await row.hover();
  await row.getByRole("button", { name: "Unarchive", exact: true }).click();
  await page.getByLabel("archived").uncheck();
  await expect(page.getByTestId("chat-list").getByText(title, { exact: true })).toBeVisible();
});

test("the old /agents links land on Prompts, and a remembered /agents location is migrated", async ({ page }) => {
  await page.route("**/api/profiles", (r) =>
    r.fulfill({ json: { profiles: [{ id: "p1", name: "Assistant", description: "", systemPrompt: "hi", model: null, capabilities: { canSpawn: false, canMessage: false }, approval: "destructive", budget: {}, isDefault: true, createdAt: "", updatedAt: "" }] } }),
  );
  await page.route("**/api/models", (r) => r.fulfill({ json: { models: [] } }));

  await page.goto("/agents");
  await expect(page).toHaveURL(/\/prompts$/);
  await page.goto("/agents/p1");
  await expect(page).toHaveURL(/\/prompts\/p1$/);
  await expect(page.getByRole("form", { name: "Prompt" })).toBeVisible();
  await expect(tab(page, "Prompts")).toBeVisible();
  await expect(tab(page, "Agents")).toHaveCount(0);

  // What an older console wrote: the Agents tab remembered under its old root.
  await page.goto("/calls");
  await page.evaluate(() => {
    const route = { tabs: { "/agents": "/agents/p1" }, last: "/agents/p1" };
    localStorage.setItem("mcpsb.ui.v1.route", JSON.stringify({ v: 1, d: route }));
  });
  await page.goto("/");
  await expect(page).toHaveURL(/\/prompts\/p1$/);
  await tab(page, "Calls").click();
  await tab(page, "Prompts").click();
  await expect(page).toHaveURL(/\/prompts\/p1$/);
  const stored = await page.evaluate(() => localStorage.getItem("mcpsb.ui.v1.route"));
  expect(stored ?? "").not.toContain("/agents");
});
