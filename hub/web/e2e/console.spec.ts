import { expect, test, type Locator } from "@playwright/test";

import { LABEL } from "./fixtures";

/**
 * Both layouts are in the DOM at once - Tailwind hides one - so a plain text
 * locator finds the hidden copy as readily as the shown one. Every assertion
 * that could match both asks for what is actually on screen.
 */
function shown(locator: Locator): Locator {
  return locator.filter({ visible: true }).first();
}

/**
 * What the console this replaces could do, done against the shipped binary.
 *
 * Every assertion here is about a capability rather than a layout: the point is
 * that the rewrite did not quietly drop something the old console had.
 */

test("shows what is connected, down to the tools", async ({ page }) => {
  await page.goto("/connections");

  await expect(page.getByText(LABEL)).toBeVisible();
  await expect(page.getByText("demo", { exact: true })).toBeVisible();
  await expect(page.getByRole("treeitem", { name: /echo/ })).toBeVisible();
});

test("filters the tree", async ({ page }) => {
  await page.goto("/connections");
  await expect(page.getByRole("treeitem", { name: /echo/ })).toBeVisible();

  await page.getByPlaceholder("filter tools, servers, machines").fill("boom");
  await expect(page.getByRole("treeitem", { name: /boom/ })).toBeVisible();
  await expect(page.getByRole("treeitem", { name: /^echo$/ })).toHaveCount(0);
});

test("calls a tool by hand and shows the result", async ({ page }) => {
  await page.goto("/connections");
  await page.getByRole("treeitem", { name: /echo/ }).first().click();

  await expect(page.getByRole("heading", { name: "echo" })).toBeVisible();
  await expect(page.getByText(`${LABEL}__demo__echo`)).toBeVisible();

  await page.getByLabel(/message/).fill("hello from playwright");
  await page.getByRole("button", { name: "Call tool" }).click();

  await expect(page.getByRole("heading", { name: "Result" })).toBeVisible();
  await expect(page.getByText("echo: hello from playwright")).toBeVisible();
});

// N1 from the outside: a tool that fails is a call that happened, and the
// console has to show the error rather than a blank pane.
test("shows a tool error as a result", async ({ page }) => {
  await page.goto("/connections");
  await page.getByRole("treeitem", { name: /boom/ }).first().click();
  await page.getByRole("button", { name: "Call tool" }).click();

  await expect(page.getByRole("heading", { name: "Result" })).toBeVisible();
  await expect(page.getByText("boom, as requested").first()).toBeVisible();
});

test("logs the calls it made", async ({ page }) => {
  // Make one first, so the log has something in it whatever order tests ran in.
  await page.goto("/connections");
  await page.getByRole("treeitem", { name: /echo/ }).first().click();
  await page.getByLabel(/message/).fill("for the log");
  await page.getByRole("button", { name: "Call tool" }).click();
  await expect(page.getByRole("heading", { name: "Result" })).toBeVisible();

  await page.goto("/calls");
  await expect(shown(page.getByText("echo"))).toBeVisible();

  // Opening a row shows the arguments that were sent.
  await shown(page.getByText("echo")).click();
  await expect(page.getByLabel("Call detail", { exact: true })).toBeVisible();
  await expect(
    page.getByLabel("Call detail", { exact: true }).getByText(/for the log/).first(),
  ).toBeVisible();
});

test("lists the MCP endpoints", async ({ page }) => {
  await page.goto("/endpoints");

  await expect(shown(page.getByText(/\/mcp$/))).toBeVisible();
  await expect(shown(page.getByText(new RegExp(`/mcp/host/${LABEL}$`)))).toBeVisible();
  // No public URL is configured, so the install command must not be invented.
  await expect(page.getByText("MCP_SWITCHBOARD_PUBLIC_URL")).toBeVisible();
});

test("navigates between views and survives a reload", async ({ page }) => {
  await page.goto("/connections");
  await page.getByRole("link", { name: "Calls" }).click();
  await expect(page).toHaveURL(/\/calls/);

  await page.getByRole("link", { name: "Endpoints" }).click();
  await expect(page).toHaveURL(/\/endpoints/);

  // A deep link is served by the hub's fallback, not by the router alone.
  await page.reload();
  await expect(page).toHaveURL(/\/endpoints/);
  await expect(shown(page.getByText(/\/mcp$/))).toBeVisible();
});

test("keeps the live indicator green", async ({ page }) => {
  await page.goto("/connections");
  // The event stream is what keeps every view current; if it never connects the
  // console is silently stale.
  await expect(page.getByTitle("event stream")).toBeVisible();
});

test("has no horizontal overflow", async ({ page }) => {
  await page.goto("/connections");
  await expect(page.getByText(LABEL)).toBeVisible();

  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth > document.documentElement.clientWidth + 1,
  );
  expect(overflow).toBe(false);
});
