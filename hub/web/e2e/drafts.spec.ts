import { expect, test, type APIRequestContext, type Page } from "@playwright/test";

import { makeChat as makeChatWith } from "./fixtures";

/**
 * Composer drafts. The chats are real; the draft routes are mocked with
 * page.route so the spec pins the client's behaviour (debounce, flush, no
 * cross-chat mixing) whatever the hub build behind it does.
 */

const run = Date.now().toString(36);

const makeChat = (request: APIRequestContext, title: string) => makeChatWith(request, title);

interface Drafts {
  store: Map<string, string>;
  puts: { id: string; draft: string }[];
}

async function mockDrafts(page: Page, initial: Record<string, string> = {}, putDelayMs = 0): Promise<Drafts> {
  const drafts: Drafts = { store: new Map(Object.entries(initial)), puts: [] };
  await page.route("**/api/chats/*/draft", async (route) => {
    const request = route.request();
    const id = decodeURIComponent(new URL(request.url()).pathname.split("/")[3]);
    if (request.method() === "PUT") {
      const { draft } = request.postDataJSON() as { draft: string };
      drafts.puts.push({ id, draft });
      if (putDelayMs) await new Promise((r) => setTimeout(r, putDelayMs));
      if (draft.trim()) drafts.store.set(id, draft);
      else drafts.store.delete(id);
    }
    const draft = drafts.store.get(id) ?? "";
    await route.fulfill({ json: { draft, updatedAt: draft ? new Date().toISOString() : null } });
  });
  return drafts;
}

const box = (page: Page) => page.getByLabel("message", { exact: true });

/** An in-app route change, without a reload. */
const navigate = (page: Page, path: string) =>
  page.evaluate((p) => {
    history.pushState({}, "", p);
    window.dispatchEvent(new PopStateEvent("popstate"));
  }, path);

test("loads a saved draft, saves typing after a pause, survives a reload", async ({ page, request }) => {
  const chat = await makeChat(request, `load-${run}`);
  const drafts = await mockDrafts(page, { [chat.id]: "unfinished thought" });

  await page.goto(`/chat/${chat.id}`);
  await expect(box(page)).toHaveValue("unfinished thought");

  await box(page).fill("unfinished thought, continued");
  await expect(page.getByTestId("draft-status")).toHaveText("draft saved");
  expect(drafts.store.get(chat.id)).toBe("unfinished thought, continued");

  await page.reload();
  await expect(box(page)).toHaveValue("unfinished thought, continued");
});

test("a pending edit is flushed on leaving the page", async ({ page, request }) => {
  const chat = await makeChat(request, `leave-${run}`);
  const drafts = await mockDrafts(page);
  await page.goto(`/chat/${chat.id}`);
  await expect(page.getByRole("heading", { name: `leave-${run}`, exact: true })).toBeVisible();

  await box(page).fill("typed and gone");
  // Inside the debounce window: nothing sent yet.
  expect(drafts.puts).toHaveLength(0);
  await page.goto("about:blank");
  await expect.poll(() => drafts.store.get(chat.id)).toBe("typed and gone");
});

test("drafts stay with their chat when switching before the save resolves", async ({ page, request }) => {
  const a = await makeChat(request, `A-${run}`);
  const b = await makeChat(request, `B-${run}`);
  const drafts = await mockDrafts(page, { [b.id]: "B's own draft" }, 400);

  await page.goto(`/chat/${a.id}`);
  await expect(page.getByRole("heading", { name: `A-${run}`, exact: true })).toBeVisible();
  await box(page).fill("for A only");
  await navigate(page, `/chat/${b.id}`);

  await expect(page.getByRole("heading", { name: `B-${run}`, exact: true })).toBeVisible();
  await expect(box(page)).toHaveValue("B's own draft");
  await expect.poll(() => drafts.store.get(a.id)).toBe("for A only");
  await expect(box(page)).toHaveValue("B's own draft");
  expect(drafts.store.get(b.id)).toBe("B's own draft");

  await navigate(page, `/chat/${a.id}`);
  await expect(box(page)).toHaveValue("for A only");
});

test("sending clears the box and does not re-save the sent text", async ({ page, request }) => {
  const chat = await makeChat(request, `send-${run}`);
  const drafts = await mockDrafts(page);
  let putsAtSend = -1;
  // What the hub does on accepting a message.
  await page.route("**/api/chats/*/messages", async (route) => {
    if (route.request().method() === "POST") {
      putsAtSend = drafts.puts.length;
      drafts.store.delete(chat.id);
    }
    await route.continue();
  });
  await page.goto(`/chat/${chat.id}`);
  await expect(page.getByRole("heading", { name: `send-${run}`, exact: true })).toBeVisible();

  await box(page).fill("ship it");
  await page.getByRole("button", { name: "Send", exact: true }).click();
  await expect(box(page)).toHaveValue("");
  await page.waitForTimeout(1200);
  expect(drafts.store.has(chat.id)).toBe(false);
  // Clicking Send blurs the box, which may flush one save first; none after.
  expect(putsAtSend).toBeGreaterThanOrEqual(0);
  expect(drafts.puts).toHaveLength(putsAtSend);
});

test("a failed save keeps the text and says so once", async ({ page, request }) => {
  const chat = await makeChat(request, `fail-${run}`);
  await page.route("**/api/chats/*/draft", (route) =>
    route.request().method() === "PUT"
      ? route.fulfill({ status: 500, json: { detail: "disk full" } })
      : route.fulfill({ json: { draft: "", updatedAt: null } }),
  );
  await page.goto(`/chat/${chat.id}`);
  await expect(page.getByRole("heading", { name: `fail-${run}`, exact: true })).toBeVisible();
  await box(page).fill("still here");
  await expect(page.getByTestId("draft-status")).toHaveText("draft not saved");
  await expect(box(page)).toHaveValue("still here");
});
