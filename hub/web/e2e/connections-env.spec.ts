import { expect, test, type Page } from "@playwright/test";

const server = (name: string) => ({
  name,
  project: null,
  command: "x",
  state: "running",
  error: null,
  exitCode: null,
  toolCount: 1,
  tools: [
    { name: "echo", exposedName: `${name}__echo`, title: null, description: null, inputSchema: { type: "object" } },
  ],
});

const snapshot = {
  connections: [
    {
      id: "c1",
      label: "legion5",
      client: {
        name: "mcp-switchboard-client",
        version: "0.3.0",
        instance: "i1",
        label: "legion5",
        environment: {
          kinds: ["devcontainer", "direnv", "nix-shell", "venv", "container"],
          project: "a-really-long-project-name-that-keeps-going-and-going-and-going",
          workspace: "/home/akos/Projects/a-really-long-project-name-that-keeps-going-and-going",
          details: { nixShell: "impure" },
        },
      },
      connectedAt: "2026-01-02T03:04:05+00:00",
      servers: [server("demo")],
    },
    {
      id: "c2",
      label: "oldbox",
      client: { name: "mcp-switchboard-client", version: "0.1.0", instance: "i2", label: "oldbox" },
      connectedAt: "2026-01-02T03:04:05+00:00",
      servers: [server("legacy")],
    },
  ],
};

async function mock(page: Page) {
  await page.route("**/api/connections", (route) => route.fulfill({ json: snapshot }));
}

async function noOverflow(page: Page) {
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  );
  expect(overflow).toBeLessThanOrEqual(0);
}

// Runs in both the desktop and the mobile project (playwright.config.ts).
test("badge, chips, filter and detail", async ({ page }) => {
    await mock(page);
    await page.goto("/connections");

    const badges = page.getByRole("tree").getByTestId("client-badge");
    await expect(badges).toHaveCount(2);
    await expect(badges.first()).toContainText("legion5");
    await expect(badges.first()).toContainText("a-really-long-project-name");
    await expect(badges.first().locator('[data-kind="devcontainer"]')).toBeVisible();
    await expect(badges.first().locator('[data-kind="nix-shell"]')).toHaveText("nix-shell (impure)");
    await expect(page.getByText(/environment not reported/)).toBeVisible();
    await noOverflow(page);

    const filter = page.getByPlaceholder("filter tools, servers, machines");
    await filter.fill("devcontainer");
    await expect(badges).toHaveCount(1);
    await filter.fill("a-really-long");
    await expect(badges).toHaveCount(1);
    await filter.fill("");

    await page.getByRole("treeitem", { name: /echo/ }).first().click();
    const header = page.getByTestId("connection-env");
    await expect(header).toContainText("/home/akos/Projects/a-really-long");
    await expect(header.locator('[data-kind="direnv"]')).toBeVisible();
    await noOverflow(page);
  });
