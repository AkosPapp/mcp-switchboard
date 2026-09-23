import { lazy, Suspense, useState } from "react";

import { Link } from "react-router-dom";

import CopyButton from "../../components/CopyButton";
import { siblingTarget } from "../../lib/dag";
import { draftText, type Draft } from "../../lib/streamAssembly";
import { formatCost, formatTokens, messageText, modelLabel } from "./format";
import ToolCard, { type ToolCardData } from "./ToolCard";
import type { ContentBlock, Message, MessageSender } from "./types";

// The markdown and highlighter are the bulk of the chat's bundle; the thread
// shows plain text for the instant it takes to load.
const Markdown = lazy(() => import("./Markdown"));

export interface MessageActions {
  select: (messageId: string) => void;
  regenerate: (message: Message) => void;
  edit: (message: Message, content: string) => void;
  branchHere: (message: Message) => void;
  busy: boolean;
}

const ACTION =
  "rounded px-2 py-1 text-xs text-muted transition-colors hover:bg-raised hover:text-text disabled:opacity-40";

function BranchNav({
  siblings,
  onSelect,
}: {
  siblings: { ids: string[]; index: number };
  onSelect: (id: string) => void;
}) {
  if (siblings.ids.length < 2) return null;
  const prev = siblingTarget(siblings, -1);
  const next = siblingTarget(siblings, 1);
  return (
    <span className="inline-flex items-center text-xs text-muted" data-testid="branch-nav">
      <button
        type="button"
        aria-label="previous version"
        disabled={prev === null}
        onClick={() => prev && onSelect(prev)}
        className={ACTION}
      >
        ‹
      </button>
      <span aria-label={`version ${siblings.index + 1} of ${siblings.ids.length}`}>
        {siblings.index + 1}/{siblings.ids.length}
      </span>
      <button
        type="button"
        aria-label="next version"
        disabled={next === null}
        onClick={() => next && onSelect(next)}
        className={ACTION}
      >
        ›
      </button>
    </span>
  );
}

function Blocks({ blocks, markdown }: { blocks: ContentBlock[]; markdown: boolean }) {
  return (
    <>
      {blocks.map((b, i) => {
        if (b.type === "text") {
          const text = b.text ?? "";
          return markdown ? (
            <Suspense key={i} fallback={<p className="whitespace-pre-wrap text-sm">{text}</p>}>
              <Markdown text={text} />
            </Suspense>
          ) : (
            <p key={i} className="whitespace-pre-wrap break-words text-sm">
              {text}
            </p>
          );
        }
        if (b.type === "image" && b.data) {
          return (
            <img
              key={i}
              alt="attachment"
              className="my-1 max-h-64 max-w-full rounded border border-border"
              src={`data:${b.media_type ?? "image/png"};base64,${b.data}`}
            />
          );
        }
        if (b.type === "thinking") {
          return (
            <details key={i} className="my-1 text-xs text-muted">
              <summary className="cursor-pointer">thinking</summary>
              <p className="whitespace-pre-wrap">{b.text}</p>
            </details>
          );
        }
        return null;
      })}
    </>
  );
}

const SENDER_KIND: Record<MessageSender["kind"], string> = {
  message: "message",
  reply: "reply",
  spawn: "task",
};

/**
 * The line the hub puts first so the model knows who is speaking
 * (`[Message from chat "x" (id …). Reply with …]`); the "from" chip says it
 * for the reader, so it is not repeated in the block.
 */
const PREAMBLE = /^\[Message from chat [^\n]*\]\n*/;

export function withoutPreamble(blocks: ContentBlock[]): ContentBlock[] {
  const first = blocks[0];
  if (!first || first.type !== "text" || !first.text || !PREAMBLE.test(first.text)) return blocks;
  return [{ ...first, text: first.text.replace(PREAMBLE, "") }, ...blocks.slice(1)];
}

/**
 * A user-role message another chat injected. It is not what the human typed, so
 * it has no Edit and no Regenerate; Branch here and the version arrows stay.
 */
function InjectedMessage({ message, sender, actions }: { message: Message; sender: MessageSender; actions: MessageActions }) {
  const blocks = withoutPreamble(message.content);
  const text = messageText(blocks);
  const siblings = message.siblings ?? { ids: [message.id], index: 0 };
  return (
    <article
      className="flex flex-col items-start"
      data-testid="message-injected"
      data-message-id={message.id}
      data-sender-kind={sender.kind}
    >
      <div className="max-w-full rounded-lg border border-l-4 border-border border-l-accent bg-raised px-3 py-2 sm:max-w-[85%]">
        <p className="mb-1 flex flex-wrap items-center gap-1.5 text-xs text-muted">
          <span aria-hidden="true">↳</span>
          <span>from</span>
          <Link
            to={`/chat/${sender.chatId}`}
            data-testid="sender-chip"
            className="max-w-[16rem] truncate rounded border border-border bg-surface px-1.5 py-0.5 font-medium text-text hover:underline"
          >
            {sender.chatTitle || "Untitled chat"}
          </Link>
          <span className="rounded border border-border px-1.5 py-0.5 text-[11px] leading-none" data-testid="sender-kind">
            {SENDER_KIND[sender.kind] ?? sender.kind}
          </span>
        </p>
        <Blocks blocks={blocks} markdown />
      </div>
      <div className="mt-0.5 flex flex-wrap items-center gap-x-1 text-xs text-muted">
        <BranchNav siblings={siblings} onSelect={actions.select} />
        <button type="button" className={ACTION} onClick={() => actions.branchHere(message)}>
          Branch here
        </button>
        {text ? <CopyButton value={text} className="border-0" /> : null}
      </div>
    </article>
  );
}

interface Props {
  message: Message;
  tools: ToolCardData[];
  actions: MessageActions;
}

export default function MessageView(props: Props) {
  const sender = props.message.role === "user" ? props.message.sender : null;
  return sender ? (
    <InjectedMessage message={props.message} sender={sender} actions={props.actions} />
  ) : (
    <HumanOrAssistantMessage {...props} />
  );
}

function HumanOrAssistantMessage({ message, tools, actions }: Props) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState("");
  const isUser = message.role === "user";
  const text = messageText(message.content);
  const siblings = message.siblings ?? { ids: [message.id], index: 0 };

  return (
    <article
      className={`flex flex-col ${isUser ? "items-end" : "items-start"}`}
      data-testid={`message-${message.role}`}
      data-message-id={message.id}
    >
      <div
        className={`max-w-full rounded-lg px-3 py-2 sm:max-w-[85%] ${
          isUser ? "bg-raised" : "border border-border bg-surface"
        }`}
      >
        {editing ? (
          <form
            onSubmit={(e) => {
              e.preventDefault();
              if (draft.trim()) actions.edit(message, draft);
              setEditing(false);
            }}
          >
            <textarea
              aria-label="edit message"
              autoFocus
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              rows={4}
              className="w-full min-w-[16rem] rounded border border-border bg-surface p-2 text-sm"
            />
            <div className="mt-1 flex gap-2">
              <button type="submit" className="rounded bg-accent px-3 py-1.5 text-sm text-bg">
                Save &amp; resend
              </button>
              <button type="button" className={ACTION} onClick={() => setEditing(false)}>
                Cancel
              </button>
            </div>
          </form>
        ) : (
          <Blocks blocks={message.content} markdown={!isUser} />
        )}
        {message.toolCalls?.length
          ? tools.map((t) => <ToolCard key={t.callId} tool={t} />)
          : null}
        {message.finishReason && message.finishReason !== "stop" && message.finishReason !== "tool_use" ? (
          <p className="mt-1 text-xs text-warn">stopped: {message.finishReason}</p>
        ) : null}
      </div>

      <div className="mt-0.5 flex flex-wrap items-center gap-x-1 text-xs text-muted">
        <BranchNav siblings={siblings} onSelect={actions.select} />
        {isUser ? (
          <button
            type="button"
            className={ACTION}
            disabled={actions.busy}
            onClick={() => {
              setDraft(text);
              setEditing(true);
            }}
          >
            Edit
          </button>
        ) : (
          <button
            type="button"
            className={ACTION}
            disabled={actions.busy}
            onClick={() => actions.regenerate(message)}
          >
            Regenerate
          </button>
        )}
        <button type="button" className={ACTION} onClick={() => actions.branchHere(message)}>
          Branch here
        </button>
        {text ? <CopyButton value={text} className="border-0" /> : null}
        {!isUser && (message.tokenOutput || message.tokenInput) ? (
          <span className="px-1">
            {modelLabel(message.model)} · {formatTokens(message.tokenInput)}→
            {formatTokens(message.tokenOutput)} · {formatCost(message.costMicros)}
          </span>
        ) : null}
      </div>
    </article>
  );
}

/** An assistant message still assembling from deltas. */
export function DraftView({ draft }: { draft: Draft }) {
  const text = draftText(draft);
  return (
    <article className="flex flex-col items-start" data-testid="message-streaming">
      <div className="max-w-full rounded-lg border border-border bg-surface px-3 py-2 sm:max-w-[85%]">
        <Suspense fallback={<p className="whitespace-pre-wrap text-sm">{text}</p>}>
          <Markdown text={text} />
        </Suspense>
        <span className="inline-block h-3 w-1.5 animate-pulse bg-muted align-middle" aria-hidden="true" />
      </div>
    </article>
  );
}
