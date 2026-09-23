/**
 * Assembling a chat stream (spec.md N4-N7, U3).
 *
 * Pure reducer: frames in, view state out. Deltas are an optimisation for
 * perceived latency; `message_done` replaces whatever was assembled (N7).
 * Frames carry a per-run `seq`; the SSE id is `{runId}:{seq}` (N5).
 */

import { applyToPath } from "./dag";
import type { Message, PendingApproval, Question } from "../views/chat/types";

/** The hub sends `content: null` for a message that is only tool calls. */
export function normalizeMessage(m: Message): Message {
  return { ...m, content: m.content ?? [] };
}

export interface FrameBase {
  type: string;
  runId: string;
  seq: number;
  chatId?: string;
  [key: string]: unknown;
}

export interface Draft {
  messageId: string;
  runId: string;
  /** assembled text by content index */
  blocks: Record<number, string>;
  /** content indexes whose deltas were the model's reasoning, not its answer */
  thinking?: Record<number, true>;
}

export interface LiveTool {
  callId: string;
  name?: string;
  arguments?: unknown;
  messageId?: string;
  done: boolean;
  result?: unknown;
  error?: string | null;
}

export interface Approval extends PendingApproval {
  runId: string;
}

export interface StreamState {
  /** `{runId}:{seq}` of the last frame applied, for resume. */
  lastEventId: string | null;
  seqByRun: Record<string, number>;
  /** runs seen live and not yet done */
  activeRuns: string[];
  drafts: Draft[];
  /** persisted messages received on the stream, in arrival order */
  committed: Message[];
  tools: LiveTool[];
  approvals: Approval[];
  error: string | null;
  /** set by an overflow frame until the caller has refetched */
  overflow: string | null;
  /** count of frames that arrived with a hole before them */
  gaps: number;
  /** last finished run's status, for the UI */
  lastRun: { runId: string; status: string } | null;
}

export const initialStream: StreamState = {
  lastEventId: null,
  seqByRun: {},
  activeRuns: [],
  drafts: [],
  committed: [],
  tools: [],
  approvals: [],
  error: null,
  overflow: null,
  gaps: 0,
  lastRun: null,
};

export type StreamAction =
  | { type: "frame"; frame: FrameBase }
  | { type: "overflow"; reason: string }
  | { type: "reset" }
  | { type: "overflow_handled" }
  /** The fetched path now contains these ids: their local copies are redundant. */
  | { type: "synced"; ids: string[] };

/** A frame from already-parsed data; null for anything that is not one. */
export function toFrame(eventName: string, data: unknown): FrameBase | null {
  if (!data || typeof data !== "object" || Array.isArray(data)) return null;
  const frame = data as FrameBase;
  if (typeof frame.type !== "string") frame.type = eventName;
  return frame;
}

/** Parse an SSE event's data; null for anything that is not a frame. */
export function parseFrame(eventName: string, data: string): FrameBase | null {
  try {
    return toFrame(eventName, JSON.parse(data));
  } catch {
    return null;
  }
}

function upsertTool(tools: LiveTool[], callId: string, patch: Partial<LiveTool>): LiveTool[] {
  const i = tools.findIndex((t) => t.callId === callId);
  if (i < 0) return [...tools, { callId, done: false, ...patch }];
  const next = tools.slice();
  next[i] = { ...next[i], ...patch };
  return next;
}

function applyFrame(state: StreamState, frame: FrameBase): StreamState {
  switch (frame.type) {
    case "run_started":
      return state.activeRuns.includes(frame.runId)
        ? state
        : { ...state, activeRuns: [...state.activeRuns, frame.runId], error: null };
    case "delta": {
      const messageId = String(frame.messageId ?? "");
      if (!messageId) return state;
      // A delta for a message already persisted is a late straggler.
      if (state.committed.some((m) => m.id === messageId)) return state;
      const index = Number(frame.contentIndex ?? 0);
      const text = String(frame.text ?? "");
      const isThinking = frame.kind === "thinking";
      const i = state.drafts.findIndex((d) => d.messageId === messageId);
      const drafts = state.drafts.slice();
      if (i < 0) {
        drafts.push({ messageId, runId: frame.runId, blocks: { [index]: text }, ...(isThinking ? { thinking: { [index]: true as const } } : {}) });
      } else {
        const d = drafts[i];
        drafts[i] = {
          ...d,
          blocks: { ...d.blocks, [index]: (d.blocks[index] ?? "") + text },
          ...(isThinking ? { thinking: { ...d.thinking, [index]: true as const } } : {}),
        };
      }
      return { ...state, drafts };
    }
    case "tool_call":
      return {
        ...state,
        tools: upsertTool(state.tools, String(frame.callId), {
          name: frame.name as string | undefined,
          arguments: frame.arguments,
          messageId: frame.messageId as string | undefined,
        }),
      };
    case "tool_result":
      return {
        ...state,
        tools: upsertTool(state.tools, String(frame.callId), {
          done: true,
          result: frame.result,
          error: (frame.error as string | null | undefined) ?? null,
        }),
        // The call ran, so whatever approval it waited on is settled (U31).
        approvals: state.approvals.filter((a) => a.callId !== String(frame.callId)),
      };
    case "approval_required": {
      const callId = String(frame.callId);
      const approval: Approval = {
        runId: frame.runId,
        callId,
        tool: String(frame.tool ?? frame.name ?? ""),
        arguments: frame.arguments,
        expiresAt: String(frame.expiresAt ?? ""),
        ...(Array.isArray(frame.questions) ? { questions: frame.questions as Question[] } : {}),
      };
      return {
        ...state,
        approvals: [...state.approvals.filter((a) => a.callId !== callId), approval],
      };
    }
    case "message_done": {
      const raw = frame.message as Message | undefined;
      if (!raw) return state;
      const message = normalizeMessage(raw);
      return {
        ...state,
        // N7: the persisted message replaces the assembled one wholesale.
        drafts: state.drafts.filter((d) => d.messageId !== message.id),
        committed: [...state.committed.filter((m) => m.id !== message.id), message],
      };
    }
    case "run_done":
      return {
        ...state,
        activeRuns: state.activeRuns.filter((r) => r !== frame.runId),
        approvals: state.approvals.filter((a) => a.runId !== frame.runId),
        lastRun: { runId: frame.runId, status: String(frame.status ?? "") },
      };
    case "error":
      return { ...state, error: String(frame.message ?? "stream error") };
    default:
      return state;
  }
}

export function streamReducer(state: StreamState, action: StreamAction): StreamState {
  switch (action.type) {
    case "reset":
      return initialStream;
    case "overflow_handled":
      return { ...state, overflow: null };
    case "synced": {
      const have = new Set(action.ids);
      const committed = state.committed.filter((m) => !have.has(m.id));
      const drafts = state.drafts.filter((d) => !have.has(d.messageId));
      if (committed.length === state.committed.length && drafts.length === state.drafts.length) {
        return state;
      }
      return { ...state, committed, drafts };
    }
    case "overflow":
      // The hub ended the stream: what was assembled may have a hole, and the
      // resume position is meaningless. The refetch is the source of truth.
      return { ...state, overflow: action.reason, drafts: [], activeRuns: [], lastEventId: null };
    case "frame": {
      const { frame } = action;
      const last = state.seqByRun[frame.runId] ?? 0;
      // Duplicate (resume replay) or older than what we have: drop.
      if (typeof frame.seq === "number" && frame.seq <= last) return state;
      const gap = typeof frame.seq === "number" && last > 0 && frame.seq > last + 1;
      const next = applyFrame(state, frame);
      return {
        ...next,
        gaps: gap ? next.gaps + 1 : next.gaps,
        seqByRun: { ...next.seqByRun, [frame.runId]: frame.seq },
        lastEventId: `${frame.runId}:${frame.seq}`,
      };
    }
  }
}

function draftParts(draft: Draft, thinking: boolean): string {
  return Object.keys(draft.blocks)
    .map(Number)
    .filter((i) => Boolean(draft.thinking?.[i]) === thinking)
    .sort((a, b) => a - b)
    .map((i) => draft.blocks[i])
    .join("\n\n");
}

/** The answer assembled so far, without the model's reasoning. */
export function draftText(draft: Draft): string {
  return draftParts(draft, false);
}

/** The reasoning assembled so far. */
export function draftThinking(draft: Draft): string {
  return draftParts(draft, true);
}

/**
 * The thread as shown: the fetched active path, plus what the stream has
 * committed since, plus drafts still assembling (not on the path yet).
 */
export function overlayThread(
  path: Message[],
  stream: Pick<StreamState, "committed" | "drafts">,
): { messages: Message[]; drafts: Draft[] } {
  let messages = path;
  for (const m of stream.committed) messages = applyToPath(messages, m);
  const onPath = new Set(messages.map((m) => m.id));
  return { messages, drafts: stream.drafts.filter((d) => !onPath.has(d.messageId)) };
}
