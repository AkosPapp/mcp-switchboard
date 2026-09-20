import { defineConfig, devices } from "@playwright/test";

/**
 * End-to-end coverage of the console, against a real hub.
 *
 * The web server here is the compiled hub binary serving the built console out
 * of its own embed.FS, not `vite dev`: what is under test is the thing that
 * ships, including the routes the Go side serves and the fallback that makes a
 * deep link work. e2e/fixtures.ts brings up a real client behind it, so the
 * views have a real machine, server and tools to show.
 */
export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false,
  // Serial, because every test shares one hub and one client process; parallel
  // workers would each want their own and the setup dominates the runtime.
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? "line" : "list",
  timeout: 30_000,
  expect: { timeout: 10_000 },

  globalSetup: "./e2e/global-setup.ts",
  globalTeardown: "./e2e/global-teardown.ts",

  use: {
    baseURL: process.env.CONSOLE_URL ?? "http://127.0.0.1:18399",
    trace: "retain-on-failure",
  },

  projects: [
    { name: "desktop", use: { ...devices["Desktop Chrome"] } },
    // The console is meant to be usable from a phone (spec.md U2), so the whole
    // suite runs at that width too rather than trusting a media query by eye.
    { name: "mobile", use: { ...devices["Pixel 7"] } },
  ],
});
