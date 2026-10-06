import { test } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import { mkdtemp } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import switchboardHub from "../plugin/switchboard-hub.js";

const send = (res, obj) => {
  res.writeHead(200, { "content-type": "application/json" });
  res.end(JSON.stringify(obj));
};

function stubHub() {
  const state = { chats: [], messages: [], appends: [], questions: [], posts: [] };
  const server = http.createServer((req, res) => {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => {
      const b = body ? JSON.parse(body) : {};
      const m = req.method + " " + req.url.split("?")[0];
      if (m === "POST /api/chats") {
        state.chats.push({ id: "c1", kind: b.kind, title: b.title });
        state.messages = [];
        return send(res, { id: "c1", kind: b.kind, title: b.title });
      }
      if (m === "GET /api/chats") return send(res, { chats: state.chats });
      const chat = m.match(/^\w+ \/api\/chats\/([^/]+)\/(.+)$/);
      if (chat) {
        const sub = chat[2];
        if (sub === "bridge/append") {
          state.appends.push(b);
          state.messages.push({
            role: b.role,
            sender: { chatId: "", chatTitle: b.source, senderName: b.source, kind: "bridge" },
            createdAt: new Date().toISOString(),
            content: [{ type: "text", text: b.text }],
          });
          return send(res, { id: "m-" + state.appends.length });
        }
        if (sub === "bridge/question") {
          state.questions.push(b);
          return send(res, { pushed: false });
        }
        if (sub.startsWith("messages") && req.method === "GET") return send(res, { messages: state.messages });
        if (sub === "messages") {
          state.posts.push({ chat: chat[1], ...b });
          return send(res, { messageID: "m-x" });
        }
      }
      res.writeHead(404);
      res.end("no route " + m);
    });
  });
  return new Promise((r) => server.listen(0, "127.0.0.1", () => r({ server, state, url: "http://127.0.0.1:" + server.address().port })));
}

async function waitFor(cond, ms = 5000) {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    if (await cond()) return;
    await new Promise((r) => setTimeout(r, 20));
  }
  throw new Error("timeout waiting");
}

test("mirror, remote prompt, permission verdict over one poller", async () => {
  const { server, state, url } = await stubHub();
  const dir = await mkdtemp(join(tmpdir(), "sb-plugin-"));
  const prompts = [];
  const client = {
    session: {
      list: async () => ({ data: [{ id: "ses_1", time: { updated: 2 } }] }),
      messages: async ({ path }) => ({
        data: [{ info: { id: "msg_a", role: "assistant" }, parts: [{ type: "text", text: "done: fixed the bug" }] }],
      }),
      prompt: async (o) => {
        prompts.push(o);
        return { data: {} };
      },
    },
  };
  const hooks = await switchboardHub({ client, directory: dir }, { hub: url, pollMs: 30 });
  try {

  // 1) assistant mirror
  await hooks.event({ event: { type: "message.updated", properties: { info: { id: "msg_a", role: "assistant", sessionID: "ses_1" } } } });
  assert.ok(state.appends.some((a) => a.role === "assistant" && a.text.includes("fixed the bug")));

  // 2) human typed in the hub console -> next opencode turn (and the mirror
  //    echo above, sender kind "bridge", must NOT be prompted back)
  state.messages.push({ role: "user", sender: null, createdAt: new Date(Date.now() + 10).toISOString(), content: [{ type: "text", text: "now add a regression test" }] });
  await waitFor(() => prompts.length === 1);
  assert.equal(prompts[0].body.parts[0].text, "now add a regression test");
  await new Promise((r) => setTimeout(r, 120));
  assert.equal(prompts.length, 1, "mirror echo or old message leaked into the session");

  // 3) permission -> mirror + push + console verdict
  const out = {};
  const decided = hooks["permission.ask"]({ id: "p1", title: "bash: rm -rf build/", time: { created: Date.now() } }, out);
  await waitFor(() => state.questions.length === 1);
  assert.match(state.appends.at(-1).text, /rm -rf build/); // wait: last append may race with mirrors
  state.messages.push({ role: "user", sender: null, createdAt: new Date(Date.now() + 3000).toISOString(), content: [{ type: "text", text: "no" }] });
  await decided;
  assert.equal(out.status, "deny");

  // 4) comms tool posts straight into a hub chat
  await hooks.tool.switchboard_message.execute({ text: "coordinating with the other agent", chat: "c9" }, { sessionID: "ses_1" });
  assert.equal(state.posts.at(-1).content, "coordinating with the other agent");
  assert.equal(state.posts.at(-1).chat, "c9");
  } finally {
    server.close();
  }
});
