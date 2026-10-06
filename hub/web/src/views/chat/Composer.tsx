import { useEffect, useRef, useState, type FormEvent } from "react";

import { useToast } from "../../components/Toast";
import { useDraft } from "./useDraft";
import { formatCost, formatTokens } from "./format";
import { ModelPicker, NoToolsWarning } from "../../lib/models";
import { modelLabel } from "./format";
import type { ContentBlock, ModelInfo } from "./types";
import { useSkills } from "../prompts/skills";
import SlashMenu, { matchSkills } from "./SlashMenu";

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

/** One quiet figure; it only takes a colour once the limit is close. */
function Meter({
  label,
  used,
  max,
  format,
  hint,
}: {
  label: string;
  used: number;
  max: number;
  format: (n: number) => string;
  hint?: string;
}) {
  const ratio = max > 0 ? used / max : 0;
  const colour = ratio > 0.9 ? "text-danger" : ratio > 0.7 ? "text-warn" : "";
  return (
    <span
      role="meter"
      aria-label={label}
      aria-valuenow={used}
      aria-valuemax={max}
      title={`${label}: ${format(used)} of ${format(max)}${hint ? ` - ${hint}` : ""}`}
      className={colour}
    >
      {label} {format(used)}/{format(max)}
    </span>
  );
}

const num = (v: unknown): number => (typeof v === "number" && Number.isFinite(v) ? v : 0);

/** The first present, finite number among the keys (the hub writes camelCase;
 * older runs and tests used snake_case). */
const pick = (o: Record<string, unknown>, ...keys: string[]): number => {
  for (const k of keys) if (typeof o[k] === "number" && Number.isFinite(o[k] as number)) return o[k] as number;
  return 0;
};

/**
 * The run's spend against its limits. "context" is how full the model's window
 * is right now (the last prompt); "tokens" is the run's running total against
 * its budget, which counts the whole conversation once per turn because every
 * turn resends it. Renders nothing when there is nothing to measure against.
 */
export function BudgetMeter({
  budget,
  usage,
}: {
  budget: Record<string, unknown>;
  usage: Record<string, unknown>;
}) {
  const tokens = num(budget.max_tokens);
  const cost = num(budget.max_cost_micros);
  const window = pick(usage, "contextWindow", "context_window");
  if (!tokens && !cost && !window) return null;
  const usedTokens = pick(usage, "tokens") || pick(usage, "inputTokens", "input_tokens") + pick(usage, "outputTokens", "output_tokens");
  return (
    <div className="flex flex-wrap gap-x-3 px-1 pb-1 text-[11px] text-muted" data-testid="budget-meter">
      {window ? (
        <Meter
          label="context"
          used={pick(usage, "contextTokens", "context_tokens")}
          max={window}
          format={formatTokens}
          hint="the last prompt against the model's context window"
        />
      ) : null}
      {tokens ? (
        <Meter
          label="budget"
          used={usedTokens}
          max={tokens}
          format={formatTokens}
          hint="everything this run has processed; each turn resends the conversation, so it grows faster than the chat"
        />
      ) : null}
      {cost ? <Meter label="cost" used={pick(usage, "costMicros", "cost_micros")} max={cost} format={formatCost} /> : null}
    </div>
  );
}

/** A finished run's outcome, from whichever source knows best: the live
 * run_done frame while it is fresh, else the fetched run row. Errors are left
 * to the "run failed" line; this is for the quiet stops (budget, cancel). */
export interface RunOutcome {
  status: string;
  finishReason: string;
  limit: string;
}

export function stoppedRunNotice(
  lastRun: { runId: string; status: string; finishReason: string; limit: string } | null,
  fetched: { id: string; status: string; finishReason: string | null; usage: Record<string, unknown> } | null | undefined,
): RunOutcome | null {
  if (lastRun) return { status: lastRun.status, finishReason: lastRun.finishReason, limit: lastRun.limit };
  if (fetched && fetched.status !== "queued" && fetched.status !== "running" && fetched.status !== "waiting") {
    const limit = typeof fetched.usage.limit === "string" ? fetched.usage.limit : "";
    return { status: fetched.status, finishReason: fetched.finishReason ?? "", limit };
  }
  return null;
}

/** Why the run stopped when the model itself didn't end the turn, or null. */
export function runStoppedText(o: RunOutcome, budget: Record<string, unknown> | undefined): string | null {
  const b = budget ?? {};
  const cap = (key: string, fmt: (n: number) => string): string => {
    const v = num(b[key]);
    return v > 0 ? ` (${fmt(v)})` : "";
  };
  if (o.status === "error") return null;
  if (o.finishReason === "budget") {
    const keepGoing = ` Send "continue" to keep going, or raise the budget in chat settings.`;
    switch (o.limit) {
      case "max_cost_micros":
        return `stopped: cost budget${cap("max_cost_micros", formatCost)} spent.${keepGoing}`;
      case "max_turns":
        return `stopped: the run hit its turn limit${cap("max_turns", String)}.${keepGoing}`;
      case "max_tool_calls":
        return `stopped: the run hit its tool-call limit${cap("max_tool_calls", String)}.${keepGoing}`;
      case "max_wall_seconds":
        return `stopped: the run hit its time limit${cap("max_wall_seconds", (n) => `${n}s`)}.${keepGoing}`;
      case "max_lifetime_cost_micros":
        return "stopped: this agent's lifetime cost budget is spent; the hub operator must raise or reset it.";
      default:
        return `stopped: token budget${cap("max_tokens", formatTokens)} spent — the whole conversation is re-sent every turn, so this grows much faster than the chat itself.${keepGoing}`;
    }
  }
  if (o.status === "cancelled" || o.finishReason === "cancelled") return "stopped: run cancelled.";
  if (o.finishReason === "interrupted") return `stopped: ${o.status === "interrupted" ? "the hub restarted while this run was in progress" : "interrupted"}.`;
  return null;
}

/** Non-modal notice when the run's prompt filled the model's context window. */
export function TruncationWarning({ usage }: { usage?: Record<string, unknown> | null }) {
  if (!usage || usage.truncated !== true) return null;
  const window = pick(usage, "contextWindow", "context_window");
  const used = pick(usage, "contextTokens", "context_tokens");
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
  /** The model the chat uses when none is picked, named on the "default" choice. */
  defaultModel?: { provider?: string; model?: string } | null;
  /** The chat's reasoning effort ("", low, medium, high, none) (I12). */
  chatEffort?: string;
  onEffortChange?: (effort: string) => void;
}

/** Hub slash commands the composer offers beside the user's skills. */
export const CHAT_COMMANDS = [
  {
    id: "hub:compact",
    name: "compact",
    description: "Compress the older context into a digest (runs on the hub, no model turn)",
    body: "",
    auto: false,
    createdAt: "",
    updatedAt: "",
  },
  {
    id: "hub:optimize_skills",
    name: "optimize_skills",
    description: "Unlock the self-review tools (prompts & skills editing) for this chat; \"/optimize_skills off\" locks them again",
    body: "",
    auto: false,
    createdAt: "",
    updatedAt: "",
  },
];

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
  defaultModel,
  chatEffort = "",
  onEffortChange,
}: Props) {
  const toast = useToast();
  const draft = useDraft(chatId);
  const { text, setText } = draft;
  const [attachments, setAttachments] = useState<Attachment[]>([]);
  const [modelKey, setModelKey] = useState("");
  const [sending, setSending] = useState(false);
  const fileRef = useRef<HTMLInputElement>(null);
  const boxRef = useRef<HTMLTextAreaElement>(null);
  // I6: the box grows with the message (up to the cap its class sets).
  useEffect(() => {
    const el = boxRef.current;
    if (!el) return;
    el.style.height = "auto";
    el.style.height = `${Math.min(el.scrollHeight, 160)}px`;
  }, [text]);
  // "/" at the start opens the skills menu (Prompts tab -> Skills).
  const skills = useSkills();
  const [slashActive, setSlashActive] = useState(0);
  const [dismissedFor, setDismissedFor] = useState<string | null>(null);
  const slashMatches = dismissedFor === text ? [] : matchSkills([...(skills.data ?? []), ...CHAT_COMMANDS], text);
  const slashOpen = slashMatches.length > 0;
  const pickSkill = (name: string) => {
    setText(`/${name} `);
    setSlashActive(0);
  };

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
    // The model that will actually see the message, for the vision check.
    const target =
      models.find((m) => `${m.provider}/${m.model}` === modelKey) ??
      models.find((m) => defaultModel?.provider === m.provider && defaultModel?.model === m.model) ??
      null;
    for (const file of Array.from(files)) {
      if (file.size > MAX_ATTACHMENT_BYTES) {
        toast.show(`${file.name} is over 5 MB`);
        continue;
      }
      if (file.type.startsWith("image/") && target && target.supportsImages === false) {
        toast.show(`the picked model cannot see images — ${file.name} not attached`);
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

      <div className="relative flex items-end gap-2">
        {slashOpen ? (
          <SlashMenu skills={slashMatches} active={Math.min(slashActive, slashMatches.length - 1)} onPick={(k) => pickSkill(k.name)} onHover={setSlashActive} />
        ) : null}
        <textarea
          ref={boxRef}
          aria-label="message"
          value={text}
          rows={2}
          disabled={disabled}
          placeholder="Message"
          onChange={(e) => {
            setText(e.target.value);
            setSlashActive(0);
          }}
          onFocus={draft.onFocus}
          onBlur={draft.onBlur}
          onKeyDown={(e) => {
            if (slashOpen) {
              const at = Math.min(slashActive, slashMatches.length - 1);
              const exact = text === `/${slashMatches[at].name}`;
              if (e.key === "ArrowDown" || e.key === "ArrowUp") {
                e.preventDefault();
                const n = slashMatches.length;
                setSlashActive((at + (e.key === "ArrowDown" ? 1 : n - 1)) % n);
                return;
              }
              if (e.key === "Tab" || (e.key === "Enter" && !e.shiftKey && !exact)) {
                e.preventDefault();
                pickSkill(slashMatches[at].name);
                return;
              }
              if (e.key === "Escape") {
                e.preventDefault();
                setDismissedFor(text);
                return;
              }
            }
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
        <ModelPicker
          models={models}
          value={modelKey}
          onChange={setModelKey}
          emptyLabel={defaultModel?.model ? `${modelLabel(defaultModel)} (chat default)` : "chat default"}
          ariaLabel="model for the next turn"
          className="flex min-w-0 max-w-[16rem] items-center gap-1 rounded border border-border bg-surface px-2 py-1 text-xs hover:bg-raised"
        />
        {onEffortChange ? (
          <label className="flex items-center gap-1 rounded border border-border bg-surface px-2 py-1 text-xs" title="reasoning effort for this chat's turns">
            effort
            <select
              value={chatEffort}
              onChange={(e) => onEffortChange(e.target.value)}
              className="rounded border border-border bg-surface px-1 py-0.5 text-xs text-text"
              aria-label="reasoning effort"
            >
              <option value="">default</option>
              <option value="low">low</option>
              <option value="medium">medium</option>
              <option value="high">high</option>
              <option value="none">none</option>
            </select>
          </label>
        ) : null}
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
