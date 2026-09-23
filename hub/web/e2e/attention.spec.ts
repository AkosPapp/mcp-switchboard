import { expect, test, type Page } from "@playwright/test";

/**
 * Approval card removal and the nested chat tree, against a mocked /api (page.route):
 * what is under test is the console's reaction, not the hub's. Desktop + phone.
 */

const agent = (id: string, parentId: string | null, status = "idle") => ({
  id, parentId, name: id, description: "", project: null, model: { provider: "p", model: "m" }, status, budget: {}, deletedAt: null,
  origin: parentId ? "spawn" : "manual", clientLabel: "box", profileId: null, createdAt: "2026-01-01T00:00:00Z",
  lastActivityAt: "2026-01-01T00:00:00Z",
});
const chat = (id: string, agentId: string, updatedAt: string, over: Record<string, unknown> = {}) => ({
  id, agentId, peerAgentId: null, title: `chat-${id}`, kind: "human", activeLeafId: null, tags: [], tokenTotal: 0,
  costTotalMicros: 0, createdAt: updatedAt, updatedAt, archivedAt: null, clientLabel: "box", profileId: null, parentChatId: null, ...over,
});

interface Mock {
  approvals: unknown[];
  postStatus: number;
  postDelay: number;
  posts: number;
}

async function mockHub(page: Page, m: Mock, agents: unknown[], chats: unknown[]) {
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    const path = url.pathname.replace(/^\/api\//, "");
    const method = route.request().method();
    const json = (body: unknown, status = 200) =>
      route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
    if (path === "events" || path.endsWith("/stream"))
      return route.fulfill({ status: 200, contentType: "text/event-stream", body: ": ok\n\n" });
    if (path === "models") return json({ models: [] });
    if (path === "connections") return json({ connections: [] });
    if (path === "profiles") return json({ profiles: [] });
    if (path === "agents") return json({ agents });
    if (path === "chats") return json({ chats, limit: 500, offset: 0 });
    if (path === "approvals") return json({ approvals: m.approvals });
    if (path === "chats/c1") return json(chats[0]);
    if (path === "chats/c1/messages") return json({ chatId: "c1", activeLeafId: null, tree: false, messages: [{
        id: "m1", chatId: "c1", parentId: null, role: "assistant", content: [{ type: "text", text: "working" }], toolCalls: null,
        toolResults: null, tokenInput: 0, tokenOutput: 0, costMicros: 0, latencyMs: 0, model: null, finishReason: null,
        runId: "r1", lastActiveChildId: null, createdAt: "2026-01-01T00:00:00Z", siblings: { ids: ["m1"], index: 0 },
      }] });
    if (path === "agents/a1") return json(agents[0]);
    if (path === "runs/r1")
      return json({
        id: "r1", agentId: "a1", chatId: "c1", status: m.approvals.length ? "waiting" : "done", budgetSnapshot: {}, usage: {},
        error: null, finishReason: null,
        pendingApprovals: m.approvals.length ? [{ callId: "k1", tool: "run_command", arguments: { cmd: "ls" }, expiresAt: new Date(Date.now() + 60000).toISOString() }] : [],
      });
    if (path === "runs/r1/approvals/k1" && method === "POST") {
      m.posts++;
      await new Promise((r) => setTimeout(r, m.postDelay));
      if (m.postStatus === 200) m.approvals = [];
      return json(m.postStatus === 200 ? {} : { detail: "boom" }, m.postStatus);
    }
    return json({ detail: "not found" }, 404);
  });
}

const pending = (): unknown => ({
  runId: "r1", chatId: "c1", agentId: "a1", agentName: "a1", chatTitle: "chat-c1", callId: "k1", tool: "run_command",
  arguments: { cmd: "ls" }, expiresAt: new Date(Date.now() + 60000).toISOString(),
});
const chats = [chat("c1", "a1", "2026-01-02T00:00:00Z")];
const agents = [agent("a1", null)];

test("approval card names the tool and disappears the moment Approve is clicked", async ({ page }) => {
  const m: Mock = { approvals: [pending()], postStatus: 200, postDelay: 1500, posts: 0 };
  await mockHub(page, m, agents, chats);
  await page.goto("/chat/c1");
  const card = page.getByTestId("approval-card");
  await expect(card.getByText("run_command", { exact: true })).toBeVisible();
  await expect(card).toContainText("Approve run_command?");
  await card.getByRole("button", { name: "Approve" }).click();
  // The POST is still in flight (1.5 s): the card is already gone.
  await expect(card).toBeHidden({ timeout: 700 });
  await expect.poll(() => m.posts).toBe(1);
});

test("a failed decision brings the card back with an error toast", async ({ page }) => {
  const m: Mock = { approvals: [pending()], postStatus: 500, postDelay: 200, posts: 0 };
  await mockHub(page, m, agents, chats);
  await page.goto("/chat/c1");
  await page.getByTestId("approval-card").getByRole("button", { name: "Deny" }).click();
  await expect(page.getByRole("status").filter({ hasText: /boom/ })).toBeVisible();
  await expect(page.getByTestId("approval-card")).toBeVisible();
});

test("chat list nests sub-chats, collapses, and flags a chat that needs approval", async ({ page }) => {
  const m: Mock = { approvals: [pending()], postStatus: 200, postDelay: 0, posts: 0 };
  const tree = [agent("a1", null, "running"), agent("kid", "a1"), agent("grand", "kid")];
  await mockHub(page, m, tree, [
    chat("c1", "a1", "2026-01-01T00:00:00Z"),
    chat("c2", "kid", "2026-01-02T00:00:00Z", { parentChatId: "c1" }),
    chat("c3", "grand", "2026-01-03T00:00:00Z", { parentChatId: "c2" }),
  ]);
  await page.goto("/chat");
  await expect(page.getByTestId("client-group")).toHaveCount(1);
  const nodes = page.getByTestId("chat-node");
  await expect(nodes).toHaveCount(3);
  await expect(nodes.nth(0)).toHaveAttribute("data-chat", "c1");
  await expect(nodes.nth(1)).toHaveAttribute("data-depth", "1");
  await expect(nodes.nth(2)).toHaveAttribute("data-depth", "2");
  await expect(page.getByTestId("attention-marker")).toHaveText("needs approval");
  await expect(page.getByTestId("chat-attention-badge")).toHaveText("1");

  // Rows are compact with a mouse, and 44px tap targets on touch (spec.md U2).
  if (await page.evaluate(() => matchMedia("(pointer: coarse)").matches)) {
    for (const row of await page.getByTestId("chat-row").all()) {
      expect((await row.boundingBox())!.height).toBeGreaterThanOrEqual(44);
    }
  }
  await page.getByRole("button", { name: "collapse sub-chats of chat-c2" }).click();
  await expect(page.getByText("chat-c3")).toBeHidden();
  await expect(page.getByText("chat-c2")).toBeVisible();
  await expect(page.getByTestId("child-count")).toHaveText("1");
  await page.reload();
  await expect(page.getByText("chat-c3")).toBeHidden();
  // A collapsed client group carries its attention count, and how many chats it holds.
  await page.getByTestId("client-group").getByRole("button", { name: /box/, expanded: true }).first().click();
  await expect(page.getByTestId("group-attention")).toHaveText("1");
  await expect(page.getByTestId("group-count")).toHaveText("3");
  await page.getByTestId("client-group").getByRole("button", { expanded: false }).first().click();
  // Nothing scrolls sideways however deep the tree is.
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
});
