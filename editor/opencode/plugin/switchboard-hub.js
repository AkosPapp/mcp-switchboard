// Switchboard hub bridge for OpenCode.
//
// This runs INSIDE the opencode process (@opencode-ai/plugin) and pairs one
// opencode project with one hub *bridge chat* (kind "bridge"; hub side:
// internal/agents/bridge.go), which makes the hub console a remote control:
//
//   • assistant replies and finished tool calls are mirrored into the bridge
//     chat with POST /api/chats/{id}/bridge/append;
//   • a message a human posts in that chat is fed to opencode as the next user
//     turn through the SDK session.prompt;
//   • a permission prompt (or any tool wanting approval) is mirrored into the
//     chat AND sent to POST /bridge/question - a phone push with the hub's own
//     question sound; answering "yes"/"no" in the console decides it;
//   • switchboard_chats / _read / _message tools let the model talk to any hub
//     chat, which is the agent-communication the whole setup is for.
//
// One poller owns the chat cursor (no racing readers: whoever owns the cursor
// dispatches every new human message exactly once - to a waiting permission
// verdict first, else to the session).
//
// Config (opencode.json "plugin": [["<path to this file>", { hub, token? }]]):
//   hub     base URL of the hub's console API (the installer derives it from
//           --hub-url: loopback -> http://127.0.0.1:8099, anything else the
//           same origin)                      or MCP_SWITCHBOARD_HUB_URL
//   token   private-listener bearer token      or MCP_SWITCHBOARD_PRIVATE_TOKEN
//   label   display name for this machine's chats (default: directory basename)
//   pollMs  inbox poll interval, default 1500

import { mkdir, readFile, writeFile } from "node:fs/promises";
import { join, dirname, basename } from "node:path";
import { tool } from "@opencode-ai/plugin";

const log = (...a) => console.error("[switchboard]", ...a);
const trim = (x) => (typeof x === "string" ? x.trim() : "");

export function textOf(parts) {
  if (!Array.isArray(parts)) return "";
  return parts
    .filter((p) => p && p.type === "text" && typeof p.text === "string")
    .map((p) => p.text)
    .join("\n\n")
    .trim();
}

// "yes/allow/…" vs "no/deny/…", first word wins.
export function parsesVerdict(text) {
  const t = trim(text).toLowerCase();
  if (!t) return null;
  if (/^(y|yes|ok|allow|approve|proceed|do it)\b/.test(t)) return "allow";
  if (/^(n|no|deny|reject|stop|cancel|nevermind)\b/.test(t)) return "deny";
  return null;
}

function statePath(directory, label) {
  return join(directory, ".harness", `switchboard-${label}.json`);
}
async function loadState(directory, label) {
  try {
    return JSON.parse(await readFile(statePath(directory, label), "utf8"));
  } catch {
    return {};
  }
}
async function saveState(directory, label, obj) {
  try {
    const p = statePath(directory, label);
    await mkdir(dirname(p), { recursive: true });
    await writeFile(p, JSON.stringify(obj), { mode: 0o600 });
    return true;
  } catch (e) {
    log("state write failed:", e?.message || e);
    return false;
  }
}

export async function switchboardHub({ client, directory }, opts = {}) {
  const cfg = opts || {};
  const hub = (trim(cfg.hub) || trim(process.env.MCP_SWITCHBOARD_HUB_URL) || "").replace(/\/+$/, "");
  if (!hub) {
    log("no hub configured; plugin idle");
    return {};
  }
  const token = trim(cfg.token) || trim(process.env.MCP_SWITCHBOARD_PRIVATE_TOKEN);
  const label = trim(cfg.label) || basename(directory) || "session";
  const pollMs = Number(cfg.pollMs) > 0 ? Number(cfg.pollMs) : 1500;
  const headers = { "content-type": "application/json", ...(token ? { authorization: "Bearer " + token } : {}) };

  const api = async (path, method, body) => {
    const res = await fetch(hub + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
    if (!res.ok) throw new Error(`hub ${method} ${path} -> ${res.status} ${(await res.text().catch(() => "")).slice(0, 160)}`);
    return res.status === 204 ? null : res.json();
  };

  const state = await loadState(directory, label);
  let chatId = trim(state.chatId);
  async function ensureChat() {
    if (chatId) return chatId;
    const title = `opencode · ${label}`;
    const list = await api("/api/chats?kind=bridge&limit=200", "GET");
    const found = (list.chats || []).find((c) => c.title === title && !c.archivedAt);
    chatId = found ? found.id : (await api("/api/chats", "POST", { title, kind: "bridge" })).id;
    await saveState(directory, label, { chatId });
    return chatId;
  }

  async function mirror(role, text) {
    const t = trim(text);
    if (!t) return;
    const id = await ensureChat();
    await api(`/api/chats/${id}/bridge/append`, "POST", { role, text: t.length > 4000 ? t.slice(0, 4000) + "\n…" : t, source: "opencode" });
  }

  // ---- outgoing mirror (one pump per assistant message / finished tool) ----
  const seenMessages = new Set();
  const seenParts = new Set();
  const MAX_SEEN = 5000;
  const remember = (set, id) => {
    set.add(id);
    if (set.size > MAX_SEEN) set.delete(set.values().next().value);
  };

  async function onMessageUpdated(info) {
    if (!info || info.role !== "assistant" || seenMessages.has(info.id)) return;
    remember(seenMessages, info.id);
    try {
      const { data } = await client.session.messages({ path: { id: info.sessionID } });
      const entry = (data || []).find((x) => x?.info?.id === info.id);
      const body = (entry?.parts || [])
        .filter((p) => p && p.type === "text" && trim(p.text))
        .map((p) => trim(p.text))
        .join("\n\n");
      if (body) await mirror("assistant", body);
    } catch (e) {
      log("assistant mirror failed:", e?.message || e);
    }
  }

  function toolLine(p) {
    const st = p.state || {};
    const out = trim(typeof st.output === "string" ? st.output : textOf(st.output?.parts));
    return `↳ ${p.tool} [${st.status || "?"}]${out ? "\n" + out.split("\n").slice(0, 4).join("\n") : ""}`;
  }
  async function onPartUpdated(part) {
    if (!part || part.type !== "tool") return;
    const st = part.state || {};
    if (st.status === "pending" || st.status === "running" || seenParts.has(part.id)) return;
    remember(seenParts, part.id);
    try {
      await mirror("assistant", toolLine(part));
    } catch (e) {
      log("tool mirror failed:", e?.message || e);
    }
  }

  // ---- incoming: one poller, verdicts first, then prompts -----------------
  const waiters = []; // {resolve, timer} waiting for the next verdict from the hub
  let cursor = Number(state.cursor || 0);
  let activeSession = trim(state.sessionID);
  let pumping = false;

  function nextVerdict(timeoutMs) {
    return new Promise((resolve) => {
      const w = { resolve };
      w.timer = setTimeout(() => {
        const i = waiters.indexOf(w);
        if (i >= 0) waiters.splice(i, 1);
        resolve(null);
      }, timeoutMs);
      if (w.timer.unref) w.timer.unref();
      waiters.push(w);
    });
  }

  async function pump() {
    const id = await ensureChat();
    const r = await api(`/api/chats/${id}/messages?limit=50`, "GET");
    // The API lists newest-last; walk oldest→newest, keep a timestamp cursor.
    for (const m of r.messages || []) {
      if (m.role !== "user") continue;
      if (m.sender && m.sender.kind === "bridge") continue; // a mirror echo, not human input
      const ms = Date.parse(m.createdAt || "") || 0;
      if (ms <= cursor) continue;
      cursor = Math.max(cursor, ms);
      await saveState(directory, label, { chatId: id, cursor, sessionID: activeSession });
      const text = textOf(m.content);
      if (!trim(text)) continue;
      const verdict = parsesVerdict(text);
      const w = verdict ? waiters.shift() : null;
      if (w) {
        clearTimeout(w.timer);
        w.resolve(verdict);
        continue;
      }
      if (!activeSession) activeSession = await firstSession();
      if (!activeSession) {
        log("no session yet; keeping message unread for the next poll");
        cursor = ms - 1;
        return;
      }
      try {
        await client.session.prompt({ path: { id: activeSession }, body: { parts: [{ type: "text", text }] } });
      } catch (e) {
        log("prompt failed:", e?.message || e);
      }
    }
  }

  async function firstSession() {
    try {
      const { data } = await client.session.list({ query: { directory } });
      const arr = Array.isArray(data) ? data : [];
      return arr.sort((a, b) => (b.time?.updated || 0) - (a.time?.updated || 0))[0]?.id || "";
    } catch {
      return "";
    }
  }

  let timer = null;
  function startPump() {
    if (timer) return;
    timer = setInterval(() => {
      if (pumping) return;
      pumping = true;
      ensureChat()
        .then(pump)
        .catch((e) => log("poll failed:", e?.message || e))
        .finally(() => {
          pumping = false;
        });
    }, pollMs);
    if (timer.unref) timer.unref();
  }

  // ---- permission relay ----------------------------------------------------
  async function onPermissionAsk(perm, output) {
    try {
      const id = await ensureChat();
      const q = [trim(perm.title), trim(typeof perm.pattern === "string" ? perm.pattern : (perm.pattern || []).join(" "))].filter(Boolean).join(" — ") || "opencode needs permission";
      await mirror("user", `⚠ ${q}`); // visible in the console thread
      await api(`/api/chats/${id}/bridge/question`, "POST", { text: q.slice(0, 480) }); // phone push
      const verdict = await nextVerdict(Number(cfg.permissionTimeoutMs) || 900000);
      if (verdict) {
        output.status = verdict;
        await mirror("assistant", `answered ${verdict} from the hub`);
      }
    } catch (e) {
      log("permission relay failed:", e?.message || e);
    }
  }

  // ---- the hooks -----------------------------------------------------------
  startPump(); // begin listening immediately; ensureChat retries inside
  return {
    tool: {
      switchboard_chats: tool({
        description: "List Switchboard hub chats (id, kind, title) to find another agent or chat to talk to.",
        args: {},
        async execute() {
          const r = await api("/api/chats?limit=100", "GET");
          return (r.chats || []).filter((c) => !c.archivedAt).map((c) => `${c.id}\t${c.kind}\t${c.title || "Untitled chat"}`).join("\n");
        },
      }),
      switchboard_read: tool({
        description: "Read the recent tail of a Switchboard hub chat.",
        args: { chat: tool.schema.string().describe("hub chat id") },
        async execute(args) {
          const r = await api(`/api/chats/${encodeURIComponent(args.chat)}/messages?limit=25`, "GET");
          return (r.messages || []).map((m) => `${m.role}: ${textOf(m.content)}`).join("\n") || "(empty)";
        },
      }),
      switchboard_message: tool({
        description: "Post a message to a Switchboard hub chat (agent-to-agent or to a human). Omit `chat` to post into your own bridge chat.",
        args: {
          text: tool.schema.string().describe("the message"),
          chat: tool.schema.string().optional().describe("hub chat id; defaults to this session's bridge chat"),
        },
        async execute(args) {
          const id = trim(args.chat) || (await ensureChat());
          await api(`/api/chats/${encodeURIComponent(id)}/messages`, "POST", { content: args.text });
          return `sent to ${id}`;
        },
      }),
    },

    async event({ event }) {
      const p = event?.properties || {};
      if (trim(p.sessionID) && !activeSession) {
        activeSession = p.sessionID;
        saveState(directory, label, { chatId, cursor, sessionID: activeSession });
      }
      if (event?.type === "message.updated") await onMessageUpdated(p.info);
      else if (event?.type === "message.part.updated") await onPartUpdated(p.part);
    },

    async "permission.ask"(input, output) {
      await onPermissionAsk(input, output);
    },
  };
}

export default switchboardHub;
export const server = switchboardHub;
