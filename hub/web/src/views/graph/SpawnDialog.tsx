import { useEffect, useState, type FormEvent } from "react";

import { ModelOptions, NoToolsWarning } from "../../lib/models";
import { useToast } from "../../components/Toast";
import { createChat, useInvalidateChat } from "../chat/api";
import type { GraphChat } from "./chatNodes";
import { messageOf, useModels } from "./hooks";
import { buttonClass, fieldClass } from "./ui";

/**
 * U18: create a sub-chat of an existing chat (docs/CHAT_MODEL_API.md): POST
 * /api/chats with `parentChatId`. The hub gives it the parent's client, grants,
 * capabilities and approval mode, so only a title, an optional prompt of its own
 * and a model are asked for. There is no way to create a root chat here: the
 * Chat panel does that.
 */
export default function SpawnDialog({
  parent,
  onClose,
  onCreated,
}: {
  parent: GraphChat;
  onClose: () => void;
  /** the new chat's execution-record id, which the Graph route is keyed by */
  onCreated: (agentId: string) => void;
}) {
  const toast = useToast();
  const invalidateChats = useInvalidateChat();
  const models = useModels();
  const [title, setTitle] = useState("");
  const [systemPrompt, setSystemPrompt] = useState("");
  const [modelKey, setModelKey] = useState(`${parent.model.provider ?? ""}/${parent.model.model ?? ""}`);
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState(false);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setError(null);
    setPending(true);
    try {
      const [provider, ...rest] = modelKey.split("/");
      const chat = await createChat({
        parentChatId: parent.chatId,
        clientLabel: parent.clientLabel ?? null,
        ...(title.trim() ? { title: title.trim() } : {}),
        ...(systemPrompt.trim() ? { profileId: null, systemPrompt } : {}),
        ...(modelKey ? { model: { provider, model: rest.join("/") } } : {}),
      });
      invalidateChats();
      toast.show(`Created ${chat.title || "sub-chat"}`);
      onCreated(chat.agentId);
    } catch (e) {
      setError(messageOf(e));
    } finally {
      setPending(false);
    }
  };

  const modelOptions = models.data ?? [];

  return (
    <div className="fixed inset-0 z-40 flex items-end justify-center bg-black/40 md:items-center">
      <form
        role="dialog"
        aria-modal="true"
        aria-label={`Spawn sub-chat of ${parent.name}`}
        onSubmit={(e) => void submit(e)}
        className="flex max-h-[92dvh] w-full flex-col overflow-hidden rounded-t-lg border border-border bg-surface shadow-2xl md:max-w-lg md:rounded-lg"
      >
        <header className="flex items-center border-b border-border px-4 py-2">
          <h2 className="flex-1 text-base font-semibold">Spawn sub-chat of {parent.name}</h2>
          <button
            type="button"
            onClick={onClose}
            aria-label="Close dialog"
            className="flex min-h-[44px] min-w-[44px] items-center justify-center rounded text-lg hover:bg-raised"
          >
            ×
          </button>
        </header>

        <div className="space-y-3 overflow-y-auto px-4 py-3">
          <label className="block text-xs text-muted">
            Title
            <input className={fieldClass()} value={title} onChange={(e) => setTitle(e.target.value)} autoFocus />
          </label>
          <label className="block text-xs text-muted">
            Model (the parent&apos;s)
            <select className={fieldClass()} value={modelKey} onChange={(e) => setModelKey(e.target.value)}>
              {modelKey !== "" && !modelOptions.some((m) => `${m.provider}/${m.model}` === modelKey) && (
                <option value={modelKey}>{modelKey}</option>
              )}
              <ModelOptions models={modelOptions} />
            </select>
            <NoToolsWarning models={modelOptions} value={modelKey} />
          </label>
          <label className="block text-xs text-muted">
            System prompt (optional)
            <textarea
              className={fieldClass()}
              rows={3}
              value={systemPrompt}
              onChange={(e) => setSystemPrompt(e.target.value)}
              placeholder="Empty follows the parent's prompt"
            />
          </label>
          <p className="text-xs text-muted">
            The sub-chat gets {parent.name}&apos;s client and permissions, and is shown under it in the Chat panel.
          </p>

          {error && (
            <p role="alert" className="text-sm text-danger">
              {error}
            </p>
          )}
        </div>

        <footer className="flex justify-end gap-2 border-t border-border px-4 py-2">
          <button type="button" className={buttonClass()} onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className={buttonClass("primary")} disabled={pending}>
            Create
          </button>
        </footer>
      </form>
    </div>
  );
}
