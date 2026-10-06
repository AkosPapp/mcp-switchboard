import { expect, test } from "@playwright/test";

const run = Date.now().toString(36);

test("bridge chat: typing does not run the hub agent; appended lines arrive", async ({ page, request }) => {
  const created = await request.post("/api/chats", { data: { title: `bridge-${run}`, kind: "bridge" } });
  expect(created.ok()).toBeTruthy();
  const chat = await created.json();
  expect(chat.kind).toBe("bridge");

  await page.goto(`/chat/${chat.id}`);
  await page.getByLabel("message", { exact: true }).fill("remote control: run the suite");
  await page.getByRole("button", { name: "Send" }).click();
  await expect(page.getByText("remote control: run the suite")).toBeVisible();

  // A bridge chat never shows run machinery: no running-tools, no failed line.
  await page.waitForTimeout(1500);
  await expect(page.getByTestId("running-tools")).toHaveCount(0);
  await expect(page.getByText("run failed:")).toHaveCount(0);

  // The external side mirrors a line in; a reload makes it visible.
  const app = await request.post(`/api/chats/${chat.id}/bridge/append`, {
    data: { role: "assistant", text: `mirrored-${run}: built, 14 tests pass`, source: "opencode" },
  });
  expect(app.ok()).toBeTruthy();
  await page.reload();
  await expect(page.getByText(`mirrored-${run}`)).toBeVisible();
});

test("bridge endpoints reject non-bridge chats", async ({ request }) => {
  const created = await request.post("/api/chats", { data: { title: `plain-${run}` } });
  const chat = await created.json();
  const res = await request.post(`/api/chats/${chat.id}/bridge/append`, { data: { role: "assistant", text: "nope" } });
  expect(res.status()).toBe(400);
});
