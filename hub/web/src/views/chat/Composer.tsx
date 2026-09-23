import { useRef, useState, type FormEvent } from "react";

import { useToast } from "../../components/Toast";
import { useDraft } from "./useDraft";
import { formatCost, formatTokens } from "./format";
import { ModelOptions, NoToolsWarning } from "../../lib/models";
import type { ContentBlock, ModelInfo } from "./types";

export interface Attachment {
  name: string;
  block: ContentBlock;
}

const MAX_ATTACHMENT_BYTES = 5 * 1024 * 1024;

function toBase64(buffer: ArrayBuffer): string {
  const bytes = new Uint8Array(buffer);
  let binary = "";
  for (let i = 0; i < bytes.length; i += 0x8000) {
    binary += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  }
  return btoa(binary);
}

/** A file as a content block: images as image blocks, text inline (U11). */
export async function fileToBlock(file: File): Promise<ContentBlock> {
  if (file.type.startsWith("image/")) {
    return { type: "image", media_type: file.type, data: toBase64(await file.arrayBuffer()) };
  }
  const text = await file.text();
  return { type: "text", text: `[file: ${file.name}]\n\`\`\`\n${text}\n\`\`\`` };
}

/** Message content: a plain string when there is nothing but text. */
export function buildContent(text: string, attachments: Attachment[]): string | ContentBlock[] {
  if (attachments.length === 0) return text;
  const blocks: ContentBlock[] = [];
  if (text.trim()) blocks.push({ type: "text", text });
  for (const a of attachments) blocks.push(a.block);
  return blocks;
}

function Meter({ label, used, max, format }: { label: string; used: number; max: number; format: (n: number) => string }) {
  const ratio = max > 0 ? Math.min(1, used / max) : 0;
  const colour = ratio > 0.9 ? "bg-danger" : ratio > 0.7 ? "bg-warn" : "bg-accent";
  return (
    <div className="min-w-0 flex-1" title={`${label}: ${format(used)} of ${format(max)}`}>
      <div className="flex justify-between text-[10px] text-muted">
        <span>{label}</span>
        <span>
          {format(used)}/{format(max)}
        </span>
      </div>
      <div
        role="meter"
        aria-label={label}
        aria-valuenow={used}
        aria-valuemax={max}
        className="h-1 overflow-hidden rounded bg-raised"
      >
        <div className={`h-full ${colour}`} style={{ width: `${ratio * 100}%` }} />
      </div>
    </div>
  );
}

const num = (v: unknown): number => (typeof v === "number" && Number.isFinite(v) ? v : 0);

/** The run's spend against its limits; renders nothing without a budget. */
export function BudgetMeter({
  budget,
  usage,
}: {
  budget: Record<string, unknown>;
  usage: Record<string, unknown>;
}) {
  const turns = num(budget.max_turns);
  const tokens = num(budget.max_tokens);
  const cost = num(budget.max_cost_micros);
  if (!turns && !tokens && !cost) return null;
  const usedTokens = num(usage.tokens) || num(usage.input_tokens) + num(usage.output_tokens);
  return (
    <div className="flex gap-3 px-1 pb-1" data-testid="budget-meter">
      {turns ? <Meter label="turns" used={num(usage.turns)} max={turns} format={String} /> : null}
      {tokens ? <Meter label="tokens" used={usedTokens} max={tokens} format={formatTokens} /> : null}
      {cost ? <Meter label="cost" used={num(usage.cost_micros)} max={cost} format={formatCost} /> : null}
    </div>
  );
}

/** Non-modal notice when the run's prompt filled the model's context window. */
export function TruncationWarning({ usage }: { usage?: Record<string, unknown> | null }) {
  if (!usage || usage.truncated !== true) return null;
  const window = num(usage.contextWindow ?? usage.context_window);
  const used = num(usage.input_tokens);
  const size = window ? ` (${window.toLocaleString("en-US")} tokens)` : "";
  return (
    <p
      role="alert"
      data-testid="truncation-warning"
      className="rounded border border-danger/50 bg-danger/10 px-3 py-2 text-sm text-danger"
    >
      The prompt filled the model&apos;s context window{size} and the provider dropped the start of it.
      Use a model or setting with a larger context, or give this chat fewer tools.
      {window && used ? ` Last prompt: ${used.toLocaleString("en-US")} tokens.` : ""}
    </p>
  );
}

interface Props {
  chatId: string;
  models: ModelInfo[];
  running: boolean;
  disabled?: boolean;
  budget?: Record<string, unknown>;
  usage?: Record<string, unknown>;
  onSend: (content: string | ContentBlock[], model: ModelInfo | null) => Promise<void>;
  onStop: () => void;
  /** Opens the mobile chat-list drawer; the "Chats" button lives in this row
   * on mobile instead of floating over it (there is no room for both). */
  onOpenList?: () => void;
}

export default function Composer({
  chatId,
  models,
  running,
  disabled,
  budget,
  usage,
  onSend,
  onStop,
  onOpenList,
}: Props) {
  const toast = useToast();
  const draft = useDraft(chatId);
  const { text, setText } = draft;
  const [attachments, setAttachments] = useState<Attachment[]>([]);
  const [modelKey, setModelKey] = useState("");
  const [sending, setSending] = useState(false);
  const fileRef = useRef<HTMLInputElement>(null);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if ((!text.trim() && attachments.length === 0) || sending) return;
    const model = models.find((m) => `${m.provider}/${m.model}` === modelKey) ?? null;
    setSending(true);
    try {
      // No debounced save of this text may fire, or land after the server
      // has cleared the draft on accepting the message.
      await draft.beforeSend();
      await onSend(buildContent(text, attachments), model);
      draft.cleared();
      setAttachments([]);
    } catch (error) {
      toast.show(error instanceof Error ? error.message : "could not send");
    } finally {
      setSending(false);
    }
  };

  const attach = async (files: FileList | null) => {
    if (!files) return;
    const added: Attachment[] = [];
    for (const file of Array.from(files)) {
      if (file.size > MAX_ATTACHMENT_BYTES) {
        toast.show(`${file.name} is over 5 MB`);
        continue;
      }
      added.push({ name: file.name, block: await fileToBlock(file) });
    }
    setAttachments((current) => [...current, ...added]);
    if (fileRef.current) fileRef.current.value = "";
  };

  return (
    <form
      onSubmit={submit}
      className="shrink-0 border-t border-border bg-surface px-3 pt-2"
      // Sits above the keyboard (interactive-widget) and the home indicator.
      style={{ paddingBottom: "max(0.5rem, env(safe-area-inset-bottom))" }}
    >
      {budget && usage ? <BudgetMeter budget={budget} usage={usage} /> : null}

      {attachments.length > 0 ? (
        <ul className="mb-1 flex flex-wrap gap-1">
          {attachments.map((a, i) => (
            <li key={i} className="flex items-center gap-1 rounded border border-border bg-raised px-2 py-0.5 text-xs">
              {a.name}
              <button
                type="button"
                aria-label={`remove ${a.name}`}
                className="px-1 text-muted"
                onClick={() => setAttachments((c) => c.filter((_, j) => j !== i))}
              >
                ×
              </button>
            </li>
          ))}
        </ul>
      ) : null}

      <div className="flex items-end gap-2">
        <textarea
          aria-label="message"
          value={text}
          rows={2}
          disabled={disabled}
          placeholder="Message"
          onChange={(e) => setText(e.target.value)}
          onFocus={draft.onFocus}
          onBlur={draft.onBlur}
          onKeyDown={(e) => {
            // Enter sends on a keyboard; on a phone Enter is a newline.
            if (e.key === "Enter" && !e.shiftKey && !window.matchMedia("(pointer: coarse)").matches) {
              e.preventDefault();
              e.currentTarget.form?.requestSubmit();
            }
          }}
          className="max-h-40 min-h-[44px] min-w-0 flex-1 resize-none rounded border border-border bg-bg p-2 text-sm placeholder:text-muted focus:border-accent"
        />
        {running ? (
          <button
            type="button"
            onClick={onStop}
            className="min-h-[44px] shrink-0 rounded border border-danger px-3 text-sm font-medium text-danger"
          >
            Stop
          </button>
        ) : null}
        <button
          type="submit"
          disabled={disabled || sending || (!text.trim() && attachments.length === 0)}
          className="min-h-[44px] shrink-0 rounded bg-accent px-4 text-sm font-medium text-bg disabled:opacity-40"
        >
          Send
        </button>
      </div>

      <div className="mt-1 text-xs">
        <NoToolsWarning models={models} value={modelKey} />
      </div>
      <div className="mt-1 flex items-center gap-2 text-xs text-muted">
        {onOpenList ? (
          <button
            type="button"
            onClick={onOpenList}
            className="shrink-0 rounded border border-border px-2 py-1 text-xs text-text md:hidden"
          >
            Chats
          </button>
        ) : null}
        <input
          ref={fileRef}
          type="file"
          multiple
          hidden
          aria-label="attach files"
          onChange={(e) => void attach(e.target.files)}
        />
        <button
          type="button"
          onClick={() => fileRef.current?.click()}
          className="rounded px-2 py-1 hover:bg-raised hover:text-text"
        >
          Attach
        </button>
        <select
          aria-label="model for the next turn"
          value={modelKey}
          onChange={(e) => setModelKey(e.target.value)}
          className="min-w-0 rounded border border-border bg-surface px-1 py-1 text-xs"
        >
          <option value="">chat default</option>
          <ModelOptions models={models} />
        </select>
        {/* Always rendered so the row never changes height. */}
        <span
          role="status"
          aria-live="polite"
          data-testid="draft-status"
          className="ml-auto min-h-4 shrink-0 text-[11px] leading-4"
        >
          {draft.status === "saving"
            ? "saving…"
            : draft.status === "saved"
              ? "draft saved"
              : draft.status === "error"
                ? "draft not saved"
                : ""}
        </span>
      </div>
    </form>
  );
}
