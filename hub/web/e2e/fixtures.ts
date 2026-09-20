import { spawn, type ChildProcess } from "node:child_process";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test as base } from "@playwright/test";

/**
 * A real hub with a real client behind it.
 *
 * Nothing here is stubbed: the binary under test serves the console from its
 * own embed.FS, and a mcp-switchboard-client process tunnels the repo's fake
 * MCP server to it. That is the point of this suite - the component tests
 * already cover the views against a fake fetch, so what is left to prove is
 * that the console works against the thing that ships.
 */

const REPO = new URL("../../../", import.meta.url).pathname;

export const TOKEN = "console-e2e-token";
export const LABEL = "e2ebox";

const HUB_BINARY = process.env.MCP_SWITCHBOARD_HUB_BINARY ?? join(REPO, "hub", "hub");
const PYTHON = process.env.E2E_PYTHON ?? join(REPO, ".venv", "bin", "python");

const PRIVATE_PORT = Number(process.env.CONSOLE_PORT ?? 18399);
const TUNNEL_PORT = PRIVATE_PORT + 1;

interface Harness {
  hub: ChildProcess;
  client: ChildProcess;
}

let harness: Harness | null = null;

async function waitFor(what: string, check: () => Promise<boolean>, timeoutMs = 45_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (await check()) return;
    await new Promise((resolve) => setTimeout(resolve, 200));
  }
  throw new Error(`timed out waiting for ${what}`);
}

export async function start(): Promise<Harness> {
  if (harness !== null) return harness;

  const dir = mkdtempSync(join(tmpdir(), "mcp-console-e2e-"));

  const hub = spawn(HUB_BINARY, [], {
    // From the data directory, not the repo: the hub reads ./.env by default
    // and this checkout has one, so starting it here would pull in whatever
    // settings the developer happens to have.
    cwd: dir,
    env: {
      ...process.env,
      MCP_SWITCHBOARD_TUNNEL_HOST: "127.0.0.1",
      MCP_SWITCHBOARD_TUNNEL_PORT: String(TUNNEL_PORT),
      MCP_SWITCHBOARD_TUNNEL_TOKEN: TOKEN,
      MCP_SWITCHBOARD_PRIVATE_HOST: "127.0.0.1",
      MCP_SWITCHBOARD_PRIVATE_PORT: String(PRIVATE_PORT),
      MCP_SWITCHBOARD_DATA_DIR: join(dir, "state"),
      MCP_SWITCHBOARD_LOG_LEVEL: "WARN",
    },
    stdio: "pipe",
  });
  hub.stderr?.on("data", (chunk) => process.stderr.write(`[hub] ${chunk}`));

  const base = `http://127.0.0.1:${PRIVATE_PORT}`;
  await waitFor("the hub to answer", async () => {
    try {
      return (await fetch(`${base}/health`)).ok;
    } catch {
      return false;
    }
  });

  const config = {
    mcpServers: {
      demo: { command: PYTHON, args: [join(REPO, "tests", "fake_mcp_server.py"), "demo"] },
    },
  };
  const configPath = join(dir, "mcp.json");
  writeFileSync(configPath, JSON.stringify(config));

  const client = spawn(
    PYTHON,
    [
      "-m",
      "mcp_switchboard_client",
      "--hub-url",
      `ws://127.0.0.1:${TUNNEL_PORT}`,
      "--token",
      TOKEN,
      "--label",
      LABEL,
      "--config",
      configPath,
      "--no-harness",
    ],
    { cwd: dir, env: { ...process.env, PYTHONUNBUFFERED: "1" }, stdio: "pipe" },
  );
  client.stderr?.on("data", (chunk) => process.stderr.write(`[client] ${chunk}`));

  // The views have nothing to show until the tunnel is up and the hub has
  // listed the server's tools, so every test would otherwise race the setup.
  await waitFor("the demo server's tools", async () => {
    try {
      const response = await fetch(`${base}/api/connections`);
      if (!response.ok) return false;
      const body = (await response.json()) as {
        connections: { servers: { name: string; state: string; toolCount: number }[] }[];
      };
      return body.connections.some((connection) =>
        connection.servers.some(
          (server) => server.name === "demo" && server.state === "running" && server.toolCount > 0,
        ),
      );
    } catch {
      return false;
    }
  });

  harness = { hub, client };
  return harness;
}

export function stop() {
  if (harness === null) return;
  harness.client.kill("SIGTERM");
  harness.hub.kill("SIGTERM");
  harness = null;
}

export const test = base.extend<Record<string, never>>({});
export { expect } from "@playwright/test";
