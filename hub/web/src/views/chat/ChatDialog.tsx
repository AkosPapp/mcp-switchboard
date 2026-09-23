import { useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";

import { useConnections } from "../../api/queries";
import { useProfiles } from "../../api/resources";
import ClientBadge from "../../components/ClientBadge";
import { createChat, patchChat, useSystemPrompt, type PatchChat } from "./api";
import type { Chat, CreateChatInput } from "./types";

const CHIP = "rounded border border-border px-1.5 py-0.5 text-[11px] leading-none text-muted";
const FIELD =
  "min-h-[44px] w-full rounded border border-border bg-surface px-2 py-1.5 text-sm md:min-h-0";
const LEGEND = "text-xs font-semibold uppercase tracking-wide text-muted";

/** The prompt source is one select: "none", "custom", or "p:<profileId>". */
const NONE = "none";
const CUSTOM = "custom";

/**
 * The one form for a chat's title, prompt and client: "New chat" (creating) and
 * the settings of an open chat (editing) are the same component
 * (docs/CHAT_MODEL_API.md). The prompt is a profile by live reference, none, or
 * a custom text; the client is exactly one connected machine or none.
 */
export default function ChatDialog({
  chat,
  defaultClient = null,
  variant = "modal",
  onClose,
  onSaved,
}: {
  /** Set to edit; omit to create. */
  chat?: Chat;
  /** Preselected client when creating (a client group's "+ chat"). */
  defaultClient?: string | null;
  /**
   * "modal" (default): a fixed overlay dialog, used for editing an open
   * chat's settings. "panel": the same form filling its container with no
   * overlay/backdrop, used for the new-chat flow in the main content panel
   * (spec: creating a chat is "the actual conversation panel", not a popup).
   */
  variant?: "modal" | "panel";
  onClose: () => void;
  onSaved: (chat: Chat) => void;
}) {
  const profiles = useProfiles();
  const connections = useConnections();
  const list = useMemo(() => profiles.data ?? [], [profiles.data]);
  const conns = connections.data?.connections ?? [];
  // A chat without a profile has its own text or none; only the prompt endpoint says which.
  const own = useSystemPrompt(chat?.id ?? null, chat !== undefined && !chat.profileId);

  const [title, setTitle] = useState(chat?.title ?? "");
  const [choice, setChoice] = useState(chat?.profileId ? `p:${chat.profileId}` : "");
  const [custom, setCustom] = useState("");
  const [client, setClient] = useState<string | null>(chat ? (chat.clientLabel ?? null) : defaultClient);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const titleRef = useRef<HTMLInputElement>(null);
  useEffect(() => titleRef.current?.focus(), []);

  // Creating: preselect the default profile once the list is here, without overriding a later choice.
  useEffect(() => {
    if (chat || choice !== "" || profiles.isLoading) return;
    const preferred = list.find((p) => p.isDefault);
    setChoice(preferred ? `p:${preferred.id}` : NONE);
  }, [chat, choice, list, profiles.isLoading]);

  // Editing a chat with no profile: start from what it has now.
  const original = useRef<{ choice: string; text: string } | null>(null);
  useEffect(() => {
    if (!chat || chat.profileId || choice !== "") return;
    if (own.data) {
      const isOwn = own.data.source === "agent" && own.data.systemPrompt !== "";
      original.current = { choice: isOwn ? CUSTOM : NONE, text: isOwn ? own.data.systemPrompt : "" };
      setChoice(original.current.choice);
      setCustom(original.current.text);
    } else if (own.isError) {
      original.current = { choice: NONE, text: "" };
      setChoice(NONE);
    }
  }, [chat, choice, own.data, own.isError]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose]);

  const profile = choice.startsWith("p:") ? list.find((p) => p.id === choice.slice(2)) : undefined;
  const profileGone = choice.startsWith("p:") && profiles.data !== undefined && !profile;
  const canSave = choice !== "" && !busy;

  // The client choices: connected ones, plus the chat's current client when it is offline.
  const offline = chat?.clientLabel && !conns.some((c) => c.label === chat.clientLabel) ? chat.clientLabel : null;

  const profileId = choice.startsWith("p:") ? choice.slice(2) : null;

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!canSave) return;
    setError(null);
    setBusy(true);
    try {
      if (chat) {
        const body: PatchChat = {};
        if (title.trim() !== "" && title.trim() !== chat.title) body.title = title.trim();
        if (client !== (chat.clientLabel ?? null)) body.clientLabel = client;
        const before = chat.profileId ? { choice: `p:${chat.profileId}`, text: "" } : (original.current ?? { choice: NONE, text: "" });
        if (choice !== before.choice || (choice === CUSTOM && custom !== before.text)) {
          body.profileId = profileId;
          if (profileId === null) body.systemPrompt = choice === CUSTOM ? custom : "";
        }
        if (Object.keys(body).length === 0) {
          onClose();
          return;
        }
        onSaved(await patchChat(chat.id, body));
      } else {
        const body: CreateChatInput = {
          ...(title.trim() ? { title: title.trim() } : {}),
          // Explicit null (not omitted) is what means "no profile": omitted means the default.
          profileId,
          ...(choice === CUSTOM ? { systemPrompt: custom } : {}),
          clientLabel: client,
        };
        onSaved(await createChat(body));
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  const heading = chat ? "Chat settings" : "New chat";
  const panel = variant === "panel";
  const form = (
    <form
      role={panel ? undefined : "dialog"}
      aria-modal={panel ? undefined : "true"}
      aria-label={panel ? undefined : heading}
      data-testid={panel ? "new-chat-panel" : undefined}
      onSubmit={(e) => void submit(e)}
      className={
        panel
          ? "flex h-full min-h-0 w-full flex-col"
          : "relative flex max-h-[92dvh] w-full flex-col rounded-t-lg border border-border bg-surface md:max-w-lg md:rounded-lg"
      }
    >
        <header className="flex items-center gap-2 border-b border-border px-3 py-2">
          <h2 className="flex-1 text-sm font-semibold">{heading}</h2>
          <button
            type="button"
            onClick={onClose}
            className="min-h-[44px] rounded border border-border px-3 text-sm md:min-h-0 md:py-1"
          >
            Cancel
          </button>
        </header>

        <div className="min-h-0 flex-1 space-y-4 overflow-y-auto p-3">
          <div className="space-y-1">
            <label htmlFor="chat-title" className={LEGEND}>
              Title
            </label>
            <input
              id="chat-title"
              ref={titleRef}
              value={title}
              onChange={(e) => setTitle(e.target.value)}
              className={FIELD}
              autoComplete="off"
              placeholder={chat ? undefined : "optional; taken from the first message"}
            />
          </div>

          <div className="space-y-1">
            <label htmlFor="chat-prompt" className={LEGEND}>
              System prompt
            </label>
            <select id="chat-prompt" value={choice} onChange={(e) => setChoice(e.target.value)} className={FIELD}>
              {choice === "" ? <option value="">loading…</option> : null}
              <option value={NONE}>None (no system prompt)</option>
              {profileGone ? <option value={choice}>prompt removed</option> : null}
              {list.map((p) => (
                <option key={p.id} value={`p:${p.id}`}>
                  {p.name}
                  {p.isDefault ? " (default)" : ""}
                </option>
              ))}
              <option value={CUSTOM}>Custom…</option>
            </select>
            {profile ? (
              <div className="space-y-1">
                <p className="text-xs text-muted">
                  Follows the prompt: editing it in the Prompts tab changes this chat too.
                </p>
                {profile.description ? <p className="text-xs text-muted">{profile.description}</p> : null}
                <div className="flex flex-wrap gap-1">
                  {profile.capabilities.canSpawn ? <span className={CHIP}>can create sub-chats</span> : null}
                  {profile.capabilities.canMessage ? <span className={CHIP}>can message other chats</span> : null}
                  {!profile.capabilities.canSpawn && !profile.capabilities.canMessage ? (
                    <span className={CHIP}>no hub tools</span>
                  ) : null}
                  <span className={CHIP}>approval: {profile.approval}</span>
                </div>
                <Link to={`/prompts/${profile.id}`} className="text-xs underline">
                  View or edit this prompt
                </Link>
              </div>
            ) : null}
            {choice === CUSTOM ? (
              <textarea
                aria-label="Custom system prompt"
                value={custom}
                onChange={(e) => setCustom(e.target.value)}
                rows={6}
                className={`${FIELD} font-mono`}
                placeholder="You are…"
              />
            ) : null}
            {choice === NONE ? <p className="text-xs text-muted">No system prompt is sent to the model.</p> : null}
            {profiles.data && list.length === 0 ? (
              <p className="text-xs text-muted">
                There are no saved prompts yet.{" "}
                <Link to="/prompts" className="underline">
                  Create one in the Prompts tab
                </Link>
                .
              </p>
            ) : null}
          </div>

          <fieldset className="space-y-1">
            <legend className={LEGEND}>MCP client</legend>
            <label
              className={`flex min-h-[44px] cursor-pointer items-center gap-2 rounded border px-2 py-1 text-sm md:min-h-0 ${
                client === null ? "border-accent" : "border-border"
              }`}
            >
              <input type="radio" name="chat-client" checked={client === null} onChange={() => setClient(null)} />
              <span className="font-medium">None</span>
              <span className="text-xs text-muted">no client tools; hub tools only</span>
            </label>
            {connections.data && conns.length === 0 ? (
              <p className="text-xs text-muted">
                No MCP client is connected. Clients dial the hub, then show up here.
              </p>
            ) : null}
            {[
              ...conns.map((c) => ({ label: c.label, environment: c.client.environment ?? null, connected: true })),
              ...(offline ? [{ label: offline, environment: null, connected: false }] : []),
            ].map((c) => (
              <label
                key={c.label}
                className={`flex min-h-[44px] cursor-pointer items-center gap-2 rounded border px-2 py-1 text-sm md:min-h-0 ${
                  client === c.label ? "border-accent" : "border-border"
                } ${c.connected ? "" : "opacity-60"}`}
              >
                <input
                  type="radio"
                  name="chat-client"
                  value={c.label}
                  checked={client === c.label}
                  onChange={() => setClient(c.label)}
                  aria-label={c.label}
                />
                <span className="min-w-0 flex-1">
                  <ClientBadge label={c.label} environment={c.environment} connected={c.connected} />
                </span>
                <span className={CHIP}>{c.connected ? "connected" : "offline"}</span>
              </label>
            ))}
          </fieldset>

          {error ? (
            <p role="alert" className="text-sm text-danger">
              {error}
            </p>
          ) : null}
        </div>

        <footer className="border-t border-border p-3">
          <button
            type="submit"
            disabled={!canSave}
            className="min-h-[44px] w-full rounded bg-accent px-3 text-sm font-medium text-bg disabled:opacity-40 md:min-h-0 md:py-1.5"
          >
            {chat ? "Save" : "Create chat"}
          </button>
        </footer>
    </form>
  );

  if (panel) return form;

  return (
    <div className="fixed inset-0 z-50 flex items-end justify-center md:items-center" role="presentation">
      <div className="absolute inset-0 bg-text/40" onClick={onClose} aria-hidden="true" />
      {form}
    </div>
  );
}
