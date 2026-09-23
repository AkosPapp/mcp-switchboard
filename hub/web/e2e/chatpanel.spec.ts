import { expect, test, type Page } from "@playwright/test";

import { LABEL, makeChat, MOCK_MODEL } from "./fixtures";

/**
 * The Chat panel's model (docs/CHAT_MODEL_API.md, spec.md U37 and U50-U52):
 * client groups -> chats -> the chats they spawned; a chat has a title, a prompt
 * and one client, chosen in one dialog and changeable later from its header;
 * chats reach each other by injecting messages, which the thread renders apart
 * from what the human typed. Real hub and real client throughout (desktop and
 * phone); only the environment chips and the injected messages are mocked,
 * because the fixture's client has no dev container to detect and a chat only
 * injects into another when a model calls a hub tool.
 */

const run = Date.now().toString(36);
const model = { provider: "openai-compatible", model: MOCK_MODEL };

/** On a phone the list is a slide-over behind "Chats" once a chat is open. */
async function openList(page: Page) {
  const button = page.getByRole("button", { name: "Chats", exact: true });
  if (await button.isVisible().catch(() => false)) await button.click();
}

/** Closes the phone's slide-over by tapping the backdrop beside it (the thread's blank margin when it is already closed); nothing on desktop. */
async function closeList(page: Page) {
  const viewport = page.viewportSize()!;
  if (viewport.width < 768) await page.mouse.click(viewport.width - 4, viewport.height / 2);
}

const group = (page: Page, client: string) => page.locator(`[data-testid="client-group"][data-client="${client}"]`);
const chatNode = (page: Page, id: string) => page.locator(`[data-testid="chat-node"][data-chat="${id}"]`);

/** The body of the next POST /api/chats the page makes. */
function capturePost(page: Page) {
  const box: { body: Record<string, unknown> | null } = { body: null };
  void page.route("**/api/chats", (route) => {
    if (route.request().method() === "POST") box.body = route.request().postDataJSON();
    return route.continue();
  });
  return box;
}

async function makeProfile(request: import("@playwright/test").APIRequestContext, name: string, systemPrompt: string) {
  const made = await request.post("/api/profiles", { data: { name, systemPrompt, model, approval: "never" } });
  expect(made.status(), await made.text()).toBe(201);
  return ((await made.json()) as { id: string }).id;
}

test("New chat: a prompt and a client, chosen in one dialog", async ({ page, request }) => {
  const profileName = `Prof-${run}`;
  const marker = `PROFILE-PROMPT-${run}`;
  const profileId = await makeProfile(request, profileName, marker);
  const title = `pa-${run}`;
  const posted = capturePost(page);

  await page.goto("/chat");
  await page.getByRole("button", { name: "New chat", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "New chat" });
  await dialog.getByLabel("Title").fill(title);
  await dialog.getByLabel("System prompt").selectOption({ label: profileName });
  await expect(dialog.getByText(/Follows the prompt/)).toBeVisible();
  await dialog.getByRole("radio", { name: LABEL, exact: true }).check();
  await dialog.getByRole("button", { name: "Create chat" }).click();
  await expect(dialog).toBeHidden();
  expect(posted.body).toMatchObject({ title, clientLabel: LABEL, profileId });
  expect("systemPrompt" in posted.body!).toBe(false);

  // It opens at once, under its client, as one row with no agent anywhere.
  await expect(page).toHaveURL(/\/chat\/[^/]+$/);
  await expect(page.getByRole("heading", { name: title, exact: true })).toBeVisible();
  await openList(page);
  await expect(group(page, LABEL).getByText(title)).toBeVisible();
  await closeList(page);
  await expect(page.getByTestId("chat-chips").getByText(`prompt: ${profileName}`)).toBeVisible();
  await expect(page.getByTestId("chat-chips").getByTestId("client-badge")).toBeVisible();

  // The viewer shows what the model gets, and where it comes from.
  await page.keyboard.press("Escape");
  await page.getByRole("button", { name: "System prompt" }).click();
  const panel = page.getByRole("dialog", { name: "system prompt for this chat" });
  await expect(panel.getByTestId("prompt-source")).toHaveText(`from prompt ${profileName}`);
  await expect(panel.getByTestId("prompt-text")).toContainText(marker);
  await expect(panel.getByRole("link", { name: "Edit prompt" })).toHaveAttribute("href", `/prompts/${profileId}`);
  await expect(panel.getByText(/\d+ tools?/)).toBeVisible();
  await expect(panel.getByRole("button", { name: "Copy" })).toBeVisible();
});

test("New chat with None and None: a No client group, no system prompt", async ({ page }) => {
  const title = `bare-${run}`;
  const posted = capturePost(page);
  await page.goto("/chat");
  await page.getByRole("button", { name: "New chat", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "New chat" });
  await dialog.getByLabel("Title").fill(title);
  await dialog.getByLabel("System prompt").selectOption({ label: "None (no system prompt)" });
  await dialog.getByRole("radio", { name: /^None/ }).check();
  await dialog.getByRole("button", { name: "Create chat" }).click();
  await expect(dialog).toBeHidden();
  // Explicit nulls, present: omitted would mean the default prompt and no client.
  expect(posted.body).toEqual({ title, profileId: null, clientLabel: null });

  await expect(page).toHaveURL(/\/chat\/[^/]+$/);
  await expect(page.getByTestId("chat-chips").getByText("no client")).toBeVisible();
  await expect(page.getByTestId("chat-chips").getByText("no prompt")).toBeVisible();
  await openList(page);
  await expect(group(page, "").getByText("No client", { exact: true })).toBeVisible();
  await expect(group(page, "").getByText(title)).toBeVisible();
  await closeList(page);

  await page.keyboard.press("Escape");
  await page.getByRole("button", { name: "System prompt" }).click();
  const panel = page.getByRole("dialog", { name: "system prompt for this chat" });
  await expect(panel.getByTestId("prompt-source")).toHaveText("none — no system prompt is sent");
});

test("New chat with a Custom prompt sends its own text and no profile", async ({ page }) => {
  const title = `custom-${run}`;
  const text = `Answer in haiku ${run}.`;
  const posted = capturePost(page);
  await page.goto("/chat");
  await page.getByRole("button", { name: "New chat", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "New chat" });
  await dialog.getByLabel("Title").fill(title);
  await dialog.getByLabel("System prompt").selectOption({ label: "Custom…" });
  await dialog.getByLabel("Custom system prompt").fill(text);
  await dialog.getByRole("radio", { name: LABEL, exact: true }).check();
  await dialog.getByRole("button", { name: "Create chat" }).click();
  await expect(dialog).toBeHidden();
  expect(posted.body).toEqual({ title, profileId: null, systemPrompt: text, clientLabel: LABEL });

  await expect(page.getByTestId("chat-chips").getByText("custom prompt")).toBeVisible();
  await page.getByRole("button", { name: "System prompt" }).click();
  const panel = page.getByRole("dialog", { name: "system prompt for this chat" });
  await expect(panel.getByTestId("prompt-source")).toHaveText("this chat's own prompt");
  await expect(panel.getByTestId("prompt-text")).toHaveText(text);
});

test("a client group's + chat preselects that client", async ({ page, request }) => {
  await makeChat(request, `grp-${run}`);
  await page.goto("/chat");
  await group(page, LABEL).getByRole("button", { name: `new chat for ${LABEL}` }).click();
  const dialog = page.getByRole("dialog", { name: "New chat" });
  await expect(dialog.getByRole("radio", { name: LABEL, exact: true })).toBeChecked();
  await expect(dialog.getByLabel("System prompt")).not.toHaveValue("");
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await expect(dialog).toBeHidden();
});

test("the header Settings change a chat's prompt, client and title with the same form", async ({ page, request }) => {
  const chat = await makeChat(request, `set-${run}`, { clientLabel: null });
  const profileName = `SetProf-${run}`;
  const profileId = await makeProfile(request, profileName, `SET-PROMPT-${run}`);
  await page.goto(`/chat/${chat.id}`);
  await expect(page.getByTestId("chat-chips").getByText("no client")).toBeVisible();

  await page.getByRole("button", { name: "Settings", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Chat settings" });
  await dialog.getByLabel("Title").fill(`set-renamed-${run}`);
  await dialog.getByLabel("System prompt").selectOption({ label: profileName });
  await dialog.getByRole("radio", { name: LABEL, exact: true }).check();
  const [patch] = await Promise.all([
    page.waitForResponse((r) => r.url().endsWith(`/api/chats/${chat.id}`) && r.request().method() === "PATCH"),
    dialog.getByRole("button", { name: "Save" }).click(),
  ]);
  expect(patch.ok()).toBe(true);
  expect(patch.request().postDataJSON()).toEqual({ title: `set-renamed-${run}`, clientLabel: LABEL, profileId });
  await expect(dialog).toBeHidden();
  await expect(page.getByRole("heading", { name: `set-renamed-${run}`, exact: true })).toBeVisible();
  await expect(page.getByTestId("chat-chips").getByText(`prompt: ${profileName}`)).toBeVisible();
  await expect(page.getByTestId("chat-chips").getByTestId("client-badge")).toBeVisible();
});

test("child chats nest under their parent, collapse, and old peer chats stay ordinary rows", async ({ page, request }) => {
  const parent = await makeChat(request, `parent-${run}`);
  const kid = await makeChat(request, `kid-${run}`, { parentChatId: parent.id });
  const grand = await makeChat(request, `grand-${run}`, { parentChatId: kid.id });
  // The old creation form (an agent id) still works for API callers; such a chat is just a chat.
  const agent = await request.post("/api/agents", { data: { name: `old-${run}`, clientLabel: LABEL, model, approval: "never" } });
  expect(agent.status(), await agent.text()).toBe(201);
  const old = await request.post("/api/chats", { data: { agentId: ((await agent.json()) as { id: string }).id, title: `oldchat-${run}` } });
  expect(old.status(), await old.text()).toBe(201);
  const oldId = ((await old.json()) as { id: string }).id;

  await page.goto("/chat");
  const client = group(page, LABEL);
  const p = client.locator(`[data-testid="chat-node"][data-chat="${parent.id}"]`);
  await expect(p).toHaveAttribute("data-depth", "0");
  const k = p.locator(`[data-testid="chat-node"][data-chat="${kid.id}"]`);
  await expect(k).toHaveAttribute("data-depth", "1");
  await expect(k.getByText(`kid-${run}`)).toBeVisible();
  await expect(k.locator(`[data-testid="chat-node"][data-chat="${grand.id}"]`)).toHaveAttribute("data-depth", "2");
  // The legacy chat is a root of its own, with no special node.
  await expect(chatNode(page, oldId)).toHaveAttribute("data-depth", "0");
  await expect(page.getByText("Older chats")).toHaveCount(0);
  await expect(page.getByTestId("chat-list").getByText(/agent/i)).toHaveCount(0);

  // Collapse: the children are hidden and counted; the choice is remembered.
  await page.getByRole("button", { name: `collapse sub-chats of parent-${run}` }).click();
  await expect(page.getByText(`kid-${run}`)).toHaveCount(0);
  await expect(p.getByTestId("child-count").first()).toHaveText("2 sub-chats");
  await page.reload();
  await expect(page.getByText(`kid-${run}`)).toHaveCount(0);
  await page.getByRole("button", { name: `expand sub-chats of parent-${run}` }).click();
  await expect(page.getByText(`grand-${run}`)).toBeVisible();

  // Opening a nested chat shows it and keeps its path open.
  await page.getByText(`grand-${run}`).click();
  await expect(page).toHaveURL(new RegExp(`/chat/${grand.id}$`));
});

test("a search finds a chat by title", async ({ page, request }) => {
  const chat = await makeChat(request, `needle-${run}`);
  await page.goto("/chat");
  await page.getByLabel("search chats").fill(`needle-${run}`);
  const row = page.getByRole("link", { name: new RegExp(`needle-${run}`) });
  await expect(row).toBeVisible();
  await expect(page.getByTestId("client-group")).toHaveCount(1);
  await row.click();
  await expect(page).toHaveURL(new RegExp(`/chat/${chat.id}$`));
});

test.describe("injected messages", () => {
  const msg = (over: Record<string, unknown>) => ({
    chatId: "x", parentId: null, role: "user", toolCalls: null, toolResults: null, tokenInput: 0, tokenOutput: 0,
    costMicros: 0, latencyMs: 0, model: null, finishReason: null, runId: null, lastActiveChildId: null,
    createdAt: "2026-01-01T00:00:00+00:00", ...over,
  });

  test("a message from another chat is a distinct block, linked, and not editable", async ({ page, request }) => {
    const chat = await makeChat(request, `inj-${run}`);
    const other = await makeChat(request, `sender-${run}`);
    const text = (t: string) => [{ type: "text", text: t }];
    const messages = [
      msg({ id: "u1", chatId: chat.id, content: text("typed by me"), siblings: { ids: ["u1"], index: 0 } }),
      msg({
        id: "i1", chatId: chat.id, parentId: "u1",
        content: text(`[Message from chat "sender-${run}" (id ${other.id}). Reply with switchboard.chat.send to that id.]\n\nfound **it**`),
        sender: { chatId: other.id, chatTitle: `sender-${run}`, kind: "message" },
        siblings: { ids: ["i1"], index: 0 },
      }),
      msg({
        id: "i2", chatId: chat.id, parentId: "i1", content: text("the answer"),
        sender: { chatId: other.id, chatTitle: `sender-${run}`, kind: "reply" },
        siblings: { ids: ["i2"], index: 0 },
      }),
    ];
    await page.route(`**/api/chats/${chat.id}/messages`, (route) =>
      route.fulfill({ json: { chatId: chat.id, activeLeafId: "i2", tree: false, messages } }),
    );
    await page.goto(`/chat/${chat.id}`);

    const thread = page.getByTestId("thread");
    const blocks = thread.getByTestId("message-injected");
    await expect(blocks).toHaveCount(2);
    const first = blocks.first();
    await expect(first.getByTestId("sender-chip")).toHaveText(`sender-${run}`);
    await expect(first.getByTestId("sender-chip")).toHaveAttribute("href", `/chat/${other.id}`);
    await expect(first.getByTestId("sender-kind")).toHaveText("message");
    await expect(blocks.nth(1).getByTestId("sender-kind")).toHaveText("reply");
    await expect(first.locator("strong", { hasText: "it" })).toBeVisible();
    await expect(first).not.toContainText("[Message from chat");
    // Not the human's: no Edit, no Regenerate; Branch here stays.
    await expect(first.getByRole("button", { name: "Edit" })).toHaveCount(0);
    await expect(first.getByRole("button", { name: "Regenerate" })).toHaveCount(0);
    await expect(first.getByRole("button", { name: "Branch here" })).toBeVisible();
    // The human's own message keeps Edit, and is a different kind of block.
    await expect(thread.getByTestId("message-user")).toHaveCount(1);
    await expect(thread.getByTestId("message-user").getByRole("button", { name: "Edit" })).toBeVisible();

    // The chip goes to the sender.
    await first.getByTestId("sender-chip").click();
    await expect(page).toHaveURL(new RegExp(`/chat/${other.id}$`));
    // No horizontal scroll from the wide block.
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    expect(overflow).toBeLessThanOrEqual(0);
  });
});

test.describe("easy delete", () => {
  test("a chat row arms inline, cancels, and deletes on the check mark, with no modal", async ({ page, request }) => {
    const chat = await makeChat(request, `deleteme-${run}`);
    let modal = false;
    page.on("dialog", (d) => {
      modal = true;
      void d.dismiss();
    });
    await page.goto("/chat");
    const row = page.getByTestId("chat-row").filter({ hasText: `deleteme-${run}` });
    await row.hover();
    await row.getByRole("button", { name: "Delete", exact: true }).click();
    await expect(row.getByText("Delete?")).toBeVisible();
    await row.getByRole("button", { name: "Cancel delete" }).click();
    await expect(row.getByText("Delete?")).toHaveCount(0);
    expect((await request.get(`/api/chats/${chat.id}`)).status()).toBe(200);

    await row.hover();
    await row.getByRole("button", { name: "Delete", exact: true }).click();
    await row.getByRole("button", { name: "Confirm delete" }).click();
    await expect(page.getByText(`deleteme-${run}`)).toHaveCount(0);
    await expect.poll(async () => (await request.get(`/api/chats/${chat.id}`)).status()).toBe(404);
    expect(modal).toBe(false);
  });

  test("deleting a parent says its sub-chats go too, and they all go", async ({ page, request }) => {
    const parent = await makeChat(request, `cascade-${run}`);
    const kid = await makeChat(request, `cascadekid-${run}`, { parentChatId: parent.id });
    await page.goto("/chat");
    const row = page.getByTestId("chat-row").filter({ hasText: `cascade-${run}` }).first();
    await row.hover();
    await row.getByRole("button", { name: "Delete", exact: true }).click();
    await expect(row.getByText("Delete + 1 sub-chat?")).toBeVisible();
    await row.getByRole("button", { name: "Confirm delete" }).click();
    await expect(page.getByText("2 chats deleted")).toBeVisible();
    await expect(page.getByText(`cascadekid-${run}`)).toHaveCount(0);
    await expect.poll(async () => (await request.get(`/api/chats/${parent.id}`)).status()).toBe(404);
    await expect.poll(async () => (await request.get(`/api/chats/${kid.id}`)).status()).toBe(404);
  });

  test("an armed row disarms by itself", async ({ page, request }) => {
    await makeChat(request, `armed-${run}`);
    await page.goto("/chat");
    const row = page.getByTestId("chat-row").filter({ hasText: `armed-${run}` });
    await row.hover();
    await row.getByRole("button", { name: "Delete", exact: true }).click();
    await expect(row.getByText("Delete?")).toBeVisible();
    await expect(row.getByText("Delete?")).toHaveCount(0, { timeout: 8_000 });
  });

  test("deleting the open chat from the header menu names the sub-chats and returns to the bare tab", async ({ page, request }) => {
    const chat = await makeChat(request, `header-${run}`);
    await page.goto(`/chat/${chat.id}`);
    await expect(page.getByRole("heading", { name: `header-${run}`, exact: true })).toBeVisible();
    await page.getByRole("button", { name: "chat menu" }).click();
    await page.getByRole("menuitem", { name: "Delete chat…" }).click();
    const confirm = page.getByRole("group", { name: "confirm delete chat" });
    await expect(confirm).toContainText("Delete this chat and its sub-chats?");
    await confirm.getByRole("button", { name: "Delete", exact: true }).click();
    await expect(page).toHaveURL(/\/chat$/);
    await expect.poll(async () => (await request.get(`/api/chats/${chat.id}`)).status()).toBe(404);
    const stored = await page.evaluate(() => localStorage.getItem("mcpsb.ui.v1.route"));
    expect(stored ?? "").not.toContain(chat.id);
  });

  test("the delete target is at least 44px on a touch screen", async ({ page, request, isMobile }) => {
    test.skip(!isMobile, "touch only");
    await makeChat(request, `tappable-${run}`);
    await page.goto("/chat");
    const box = await page.getByTestId("chat-row").filter({ hasText: `tappable-${run}` }).getByRole("button", { name: "Delete", exact: true }).boundingBox();
    expect(box!.width).toBeGreaterThanOrEqual(44);
    expect(box!.height).toBeGreaterThanOrEqual(44);
  });
});

test("the client shows its project and environment in the group header and the picker", async ({ page, request }) => {
  await makeChat(request, `envchat-${run}`);
  const environment = { kinds: ["devcontainer", "direnv"], project: `proj-${run}`, workspace: "/work/proj", details: { image: "node" } };
  await page.route("**/api/connections", async (route) => {
    const real = await route.fetch();
    const body = (await real.json()) as { connections: { label: string; client: Record<string, unknown> }[] };
    for (const c of body.connections) if (c.label === LABEL) c.client.environment = environment;
    await route.fulfill({ response: real, json: body });
  });
  await page.goto("/chat");
  const head = group(page, LABEL).getByTestId("client-badge").first();
  await expect(head).toContainText(`${LABEL}`);
  await expect(head).toContainText(`proj-${run}`);
  await expect(head.getByText("devcontainer")).toBeVisible();
  await expect(head).toHaveAttribute("title", /workspace: \/work\/proj/);

  await page.getByRole("button", { name: "New chat", exact: true }).click();
  const option = page.getByRole("dialog", { name: "New chat" }).getByRole("radio", { name: LABEL, exact: true }).locator("xpath=ancestor::label");
  await expect(option).toContainText(`proj-${run}`);
  await expect(option.getByText("direnv")).toBeVisible();
  await expect(option.getByText("connected", { exact: true })).toBeVisible();
});

test("has no horizontal page scroll with the dialog and the prompt panel open", async ({ page, request }) => {
  const chat = await makeChat(request, `wide-${run}`);
  await page.goto(`/chat/${chat.id}`);
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  await expect(page.getByRole("dialog", { name: "Chat settings" })).toBeVisible();
  let overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  expect(overflow).toBeLessThanOrEqual(0);
  await page.keyboard.press("Escape");
  await page.getByRole("button", { name: "System prompt" }).click();
  await expect(page.getByRole("dialog", { name: "system prompt for this chat" })).toBeVisible();
  overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  expect(overflow).toBeLessThanOrEqual(0);
});
