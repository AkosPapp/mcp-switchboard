// Switchboard: selection to chat. One command, no build step, no hub change:
// it quotes the active selection into a chat's *draft* through the hub's
// draft API (PUT /api/chats/{id}/draft publishes a TypeDraft event, so an
// open console tab shows the citation live). The draft is the composer text,
// so the citation is explicit and attributable — it joins the next message
// the user actually sends, not silently the system prompt.
const vscode = require("vscode");

const MAX_SNIPPET_LINES = 200;

function config() {
  const cfg = vscode.workspace.getConfiguration("switchboard");
  return {
    hubUrl: (cfg.get("hubUrl") || "http://127.0.0.1:8099").replace(/\/$/, ""),
    token: cfg.get("token") || "",
  };
}

async function hubFetch(path, init) {
  const { hubUrl, token } = config();
  const headers = Object.assign({ "Content-Type": "application/json" }, init && init.headers);
  if (token) headers["Authorization"] = "Bearer " + token;
  const res = await fetch(hubUrl + path, Object.assign({}, init, { headers }));
  if (!res.ok) throw new Error(`hub replied ${res.status} for ${path}`);
  return res.json();
}

async function pickChat(globalState) {
  const data = await hubFetch("/api/chats?limit=100");
  const chats = (data.chats || []).filter((c) => (c.archivedAt ?? null) === null);
  if (chats.length === 0) throw new Error("the hub has no chats to send to");
  const lastId = globalState.get("switchboard.lastChat");
  if (chats.length === 1) return chats[0];
  const items = chats.map((c) => ({
    label: c.title || c.id,
    description: new Date(c.updatedAt).toLocaleString(),
    chat: c,
    picked: c.id === lastId,
  }));
  items.sort((a, b) => (b.picked ? 1 : 0) - (a.picked ? 1 : 0));
  const picked = await vscode.window.showQuickPick(items, {
    placeHolder: "Which Switchboard chat should receive the selection?",
  });
  if (!picked) return null;
  await globalState.update("switchboard.lastChat", picked.chat.id);
  return picked.chat;
}

function citation(editor) {
  const sel = editor.selection;
  const rel = vscode.workspace.asRelativePath(editor.document.uri, false);
  const root = vscode.workspace.getWorkspaceFolder(editor.document.uri);
  const label = root ? root.name : rel.split("/")[0];
  const start = sel.active.start.line;
  const end = sel.active.end.line;
  const [from, to] = start <= end ? [start, end] : [end, start];
  const text = editor.document.getText(
    new vscode.Range(from, editor.document.lineAt(from).firstNonWhitespaceCharacterIndex, to, editor.document.lineAt(to).text.length)
  );
  const lines = text.split("\n");
  const truncated = lines.length > MAX_SNIPPET_LINES;
  const body = truncated ? lines.slice(0, MAX_SNIPPET_LINES).join("\n") : text;
  const lang = editor.document.languageId === "plaintext" ? "" : editor.document.languageId;
  const range = `${label}:${rel}:${from + 1}-${to + 1}`;
  const fence = "```" + lang + " " + range + "\n" + body + (truncated ? "\n…" : "") + "\n```";
  return `${range}\n${fence}`;
}

async function sendSelection(globalState) {
  const editor = vscode.window.activeTextEditor;
  if (!editor || editor.selection.isEmpty) {
    vscode.window.showWarningMessage("Switchboard: select some code first.");
    return;
  }
  let chat;
  try {
    chat = await pickChat(globalState);
  } catch (err) {
    vscode.window.showErrorMessage(`Switchboard: ${err.message}`);
    return;
  }
  if (!chat) return;
  try {
    const current = await hubFetch(`/api/chats/${chat.id}/draft`, { method: "GET" });
    const draft = current.draft ? current.draft + "\n\n" : "";
    const next = draft + citation(editor);
    await hubFetch(`/api/chats/${chat.id}/draft`, {
      method: "PUT",
      body: JSON.stringify({ draft: next }),
    });
    vscode.window.showInformationMessage(`Switchboard: selection quoted into “${chat.title || chat.id}”’s draft.`);
  } catch (err) {
    vscode.window.showErrorMessage(`Switchboard: could not reach the hub (${err.message}). Is it on ${config().hubUrl}?`);
  }
}

function activate(context) {
  context.subscriptions.push(
    vscode.commands.registerCommand("switchboard.sendSelectionToChat", () =>
      sendSelection(context.globalState)
    )
  );
}

function deactivate() {}

module.exports = { activate, deactivate };
