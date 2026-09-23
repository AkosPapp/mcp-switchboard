import { spawn, type ChildProcess } from "node:child_process";
import { createServer, type Server } from "node:http";
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
  llm: Server;
}

export const MOCK_MODEL = "mock-1";
const LLM_PORT = PRIVATE_PORT + 2;

/**
 * A tiny OpenAI-compatible chat-completions server. It streams a canned answer
 * in word-sized chunks (numbered, so a regenerate is visibly a different
 * message), and calls the demo `echo` tool once when the user asks for it.
 */
function startMockLLM(): Promise<Server> {
  let answers = 0;
  const server = createServer((req, res) => {
    if (req.method !== "POST" || !req.url?.endsWith("/chat/completions")) {
      res.writeHead(404).end();
      return;
    }
    let raw = "";
    req.on("data", (c) => (raw += c));
    req.on("end", async () => {
      const body = JSON.parse(raw) as {
        messages: { role: string; content: unknown }[];
        tools?: { function: { name: string } }[];
      };
      const last = body.messages[body.messages.length - 1];
      const lastUser = [...body.messages].reverse().find((m) => m.role === "user");
      const wantsTool = JSON.stringify(lastUser?.content ?? "").includes("use the tool");
      const echo = body.tools?.find((t) => t.function.name.endsWith("echo"))?.function.name;

      res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
      const send = (delta: object, finish: string | null = null, extra: object = {}) =>
        res.write(`data: ${JSON.stringify({ choices: [{ index: 0, delta, finish_reason: finish }], ...extra })}\n\n`);
      const pause = () => new Promise((r) => setTimeout(r, 40));

      if (wantsTool && echo && last.role !== "tool") {
        send({ role: "assistant", content: "" });
        send({
          tool_calls: [
            { index: 0, id: "call_1", type: "function", function: { name: echo, arguments: JSON.stringify({ message: "from the model" }) } },
          ],
        });
        send({}, "tool_calls");
      } else {
        answers += 1;
        const text = last.role === "tool" ? "The tool answered." : `Mock answer ${answers}: **hello** from the model.\n\n\`\`\`js\nconsole.log(${answers});\n\`\`\``;
        send({ role: "assistant", content: "" });
        for (const word of text.split(/(?<= )/)) {
          send({ content: word });
          await pause();
        }
        send({}, "stop");
      }
      res.write(`data: ${JSON.stringify({ choices: [], usage: { prompt_tokens: 10, completion_tokens: 5 } })}\n\n`);
      res.write("data: [DONE]\n\n");
      res.end();
    });
  });
  return new Promise((resolve) => server.listen(LLM_PORT, "127.0.0.1", () => resolve(server)));
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
  const llm = await startMockLLM();
  const modelsPath = join(dir, "models.json");
  writeFileSync(
    modelsPath,
    JSON.stringify({ models: [{ provider: "openai-compatible", model: MOCK_MODEL }] }),
  );

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
      // The graph and chat views only exist when the orchestrator does.
      MCP_SWITCHBOARD_AGENTS_ENABLED: "true",
      MCP_SWITCHBOARD_LLM_OPENAI_COMPATIBLE_BASE_URL: `http://127.0.0.1:${LLM_PORT}`,
      MCP_SWITCHBOARD_LLM_MODELS: modelsPath,
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

  harness = { hub, client, llm };
  return harness;
}

export function stop() {
  if (harness === null) return;
  harness.client.kill("SIGTERM");
  harness.hub.kill("SIGTERM");
  harness.llm.close();
  harness = null;
}

export const test = base.extend<Record<string, never>>({});
export { expect } from "@playwright/test";

/** A prompt (profile) per approval mode, made once per run: chats follow it by live reference. */
const profiles = new Map<string, Promise<string>>();
export function profileFor(request: import("@playwright/test").APIRequestContext, approval: string): Promise<string> {
  let made = profiles.get(approval);
  if (!made) {
    made = (async () => {
      const response = await request.post("/api/profiles", {
        data: {
          name: `e2e-${approval}-${Date.now().toString(36)}`,
          systemPrompt: "You are a test chat.",
          model: { provider: "openai-compatible", model: MOCK_MODEL },
          approval,
          capabilities: { canSpawn: true, canMessage: true },
        },
      });
      if (response.status() !== 201) throw new Error(`create profile: ${response.status()} ${await response.text()}`);
      return ((await response.json()) as { id: string }).id;
    })();
    profiles.set(approval, made);
  }
  return made;
}

export interface ChatOptions {
  approval?: string;
  /** a client label, or null for none */
  clientLabel?: string | null;
  parentChatId?: string;
  /** own prompt text; the chat then follows no profile */
  systemPrompt?: string;
}

/**
 * Creates a chat the way the console does (docs/CHAT_MODEL_API.md): one POST
 * /api/chats with its title, prompt and client. Real hub calls, no mocks. The
 * default prompt is a shared one with the mock model and approval "never".
 */
export async function makeChat(
  request: import("@playwright/test").APIRequestContext,
  title: string,
  options: ChatOptions = {},
) {
  const { approval = "never", clientLabel = LABEL, parentChatId, systemPrompt } = options;
  const data: Record<string, unknown> = {
    title,
    clientLabel,
    ...(systemPrompt !== undefined
      ? { profileId: null, systemPrompt, model: { provider: "openai-compatible", model: MOCK_MODEL } }
      : { profileId: await profileFor(request, approval) }),
    ...(parentChatId ? { parentChatId } : {}),
  };
  const response = await request.post("/api/chats", { data });
  if (response.status() !== 201) throw new Error(`create chat: ${response.status()} ${await response.text()}`);
  return (await response.json()) as { id: string; agentId: string; parentChatId: string | null; title: string };
}
