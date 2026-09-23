/** Chat-side REST calls and TanStack bindings (docs/API.md). */

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useState } from "react";

import { useToast } from "../../components/Toast";

import { ApiError, request } from "../../api/client";
import { normalizeMessage } from "../../lib/streamAssembly";
import type {
  ApprovalItem,
  Chat,
  ChatList,
  ChatToolsResponse,
  ContentBlock,
  CreateChatInput,
  MessageList,
  ModelInfo,
  Run,
  SystemPromptInfo,
} from "./types";
import { ACTIVE_RUN } from "./types";

const JSON_HEADERS = { "Content-Type": "application/json" };

export interface ChatFilters {  kind?: string;
  tag?: string;
  q?: string;
  includeArchived?: boolean;
}

function qs(params: Record<string, string | number | boolean | undefined>): string {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== "" && v !== false) q.set(k, String(v === true ? 1 : v));
  }
  const s = q.toString();
  return s ? `?${s}` : "";
}

export const chatKeys = {
  models: ["models"] as const,
  chats: (f: ChatFilters) => ["chats", f] as const,
  chat: (id: string) => ["chat", id] as const,
  messages: (id: string) => ["chatMessages", id] as const,
  chatTools: (id: string) => ["chatTools", id] as const,
  systemPrompt: (id: string) => ["chatSystemPrompt", id] as const,
  run: (id: string) => ["run", id] as const,
  approvals: ["approvals"] as const,
};

export const getModels = () => request<{ models: ModelInfo[] }>("models");
export const getChats = (f: ChatFilters) =>
  request<ChatList>(`chats${qs({ ...f, limit: 500 })}`);
export const getChat = (id: string) => request<Chat>(`chats/${encodeURIComponent(id)}`);
export const getMessages = async (id: string): Promise<MessageList> => {
  const list = await request<MessageList>(`chats/${encodeURIComponent(id)}/messages`);
  return { ...list, messages: (list.messages ?? []).map(normalizeMessage) };
};
/** The whole message DAG, not just the active path (spec: ?tree=1). */
export const getMessageTree = async (id: string): Promise<MessageList> => {
  const list = await request<MessageList>(`chats/${encodeURIComponent(id)}/messages?tree=1`);
  return { ...list, messages: (list.messages ?? []).map(normalizeMessage) };
};
export const getRun = (id: string) => request<Run>(`runs/${encodeURIComponent(id)}`);

export const getSystemPrompt = (id: string) =>
  request<SystemPromptInfo>(`chats/${encodeURIComponent(id)}/system-prompt`);
export const getChatTools = (id: string) =>
  request<ChatToolsResponse>(`chats/${encodeURIComponent(id)}/tools`);

/** Creates a chat with its prompt and client together (docs/CHAT_MODEL_API.md). */
export const createChat = (input: CreateChatInput) =>
  request<Chat>("chats", {
    method: "POST",
    headers: JSON_HEADERS,
    body: JSON.stringify(input),
  });

export interface Draft {
  draft: string;
  updatedAt: string | null;
}

export const getDraft = (id: string) =>
  request<Draft>(`chats/${encodeURIComponent(id)}/draft`);

/** An empty draft deletes it server-side. `keepalive` lets it outlive a closing page. */
export const putDraft = (id: string, draft: string, keepalive = false) =>
  request<Draft>(`chats/${encodeURIComponent(id)}/draft`, {
    method: "PUT",
    headers: JSON_HEADERS,
    body: JSON.stringify({ draft }),
    ...(keepalive ? { keepalive: true } : {}),
  });

export const draftKey = (id: string) => ["chatDraft", id] as const;

export interface PatchChat {
  title?: string;
  /** A profile id, or null to leave the profile (then `systemPrompt` is the chat's own). */
  profileId?: string | null;
  systemPrompt?: string;
  clientLabel?: string | null;
  tags?: string[];
  archived?: boolean;
  activeLeafId?: string;
  selectMessageId?: string;
}

export const patchChat = (id: string, body: PatchChat) =>
  request<Chat>(`chats/${encodeURIComponent(id)}`, {
    method: "PATCH",
    headers: JSON_HEADERS,
    body: JSON.stringify(body),
  });

/** Permanent; also deletes every child chat, and says how many chats went. */
export async function deleteChat(id: string): Promise<{ deletedChats: number }> {
  const body = await request<{ deletedChats?: number } | undefined>(`chats/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
  return { deletedChats: body?.deletedChats ?? 1 };
}

export interface PostResult {
  messageId: string;
  runId: string;
  deduplicated?: true;
}

export function postMessage(
  chatId: string,
  body: { content: string | ContentBlock[]; parentId?: string; model?: object },
  idempotencyKey: string,
) {
  return request<PostResult>(`chats/${encodeURIComponent(chatId)}/messages`, {
    method: "POST",
    headers: { ...JSON_HEADERS, "Idempotency-Key": idempotencyKey },
    body: JSON.stringify(body),
  });
}

export const branchChat = (
  chatId: string,
  body: { fromMessageId: string; content?: string | ContentBlock[]; model?: object },
) =>
  request<PostResult>(`chats/${encodeURIComponent(chatId)}/branch`, {
    method: "POST",
    headers: JSON_HEADERS,
    body: JSON.stringify(body),
  });

export const cancelRun = (runId: string) =>
  request<void>(`runs/${encodeURIComponent(runId)}/cancel`, { method: "POST" });

export const decideApproval = (
  runId: string,
  callId: string,
  approved: boolean,
  reason?: string,
) =>
  request<void>(`runs/${encodeURIComponent(runId)}/approvals/${encodeURIComponent(callId)}`, {
    method: "POST",
    headers: JSON_HEADERS,
    body: JSON.stringify({ approved, reason }),
  });

/** One answer per question, in order: the chosen option labels and/or the user's own words. */
export const answerQuestion = (runId: string, callId: string, answers: string[][]) =>
  request<void>(`runs/${encodeURIComponent(runId)}/questions/${encodeURIComponent(callId)}`, {
    method: "POST",
    headers: JSON_HEADERS,
    body: JSON.stringify({ answers: answers.map((a) => ({ answers: a })) }),
  });

export function exportUrl(chatId: string, format: "json" | "markdown"): string {
  return `api/chats/${encodeURIComponent(chatId)}/export?format=${format}`;
}

export function useModels() {
  return useQuery({ queryKey: chatKeys.models, queryFn: getModels, retry: false });
}

export function useChats(filters: ChatFilters, enabled = true) {
  return useQuery({
    queryKey: chatKeys.chats(filters),
    queryFn: () => getChats(filters),
    enabled,
    placeholderData: (previous) => previous,
  });
}

export function useChat(id: string | null) {
  return useQuery({
    queryKey: chatKeys.chat(id ?? ""),
    queryFn: () => getChat(id as string),
    enabled: id !== null,
  });
}

export function useMessages(id: string | null) {
  return useQuery({
    queryKey: chatKeys.messages(id ?? ""),
    queryFn: () => getMessages(id as string),
    enabled: id !== null,
  });
}

/** Keyed under the chat's messages, so every chat invalidation refreshes it too. */
export function useMessageTree(id: string | null) {
  return useQuery({
    queryKey: [...chatKeys.messages(id ?? ""), "tree"],
    queryFn: () => getMessageTree(id as string),
    enabled: id !== null,
  });
}

/** The exact system prompt the next turn sends; refetched on every open. */
export function useSystemPrompt(id: string | null, enabled = true) {
  return useQuery({
    queryKey: chatKeys.systemPrompt(id ?? ""),
    queryFn: () => getSystemPrompt(id as string),
    enabled: id !== null && enabled,
    staleTime: 0,
  });
}

export function useChatTools(id: string | null, enabled = true) {
  return useQuery({
    queryKey: chatKeys.chatTools(id ?? ""),
    queryFn: () => getChatTools(id as string),
    enabled: id !== null && enabled,
  });
}

export function useRun(id: string | null) {
  return useQuery({
    queryKey: chatKeys.run(id ?? ""),
    queryFn: () => getRun(id as string),
    enabled: id !== null,
    // The run is the only place a pending approval and its usage are reported
    // for a page that missed the stream frame, so poll while it is live.
    refetchInterval: (q) =>
      q.state.data && ACTIVE_RUN.includes(q.state.data.status) ? 3000 : false,
  });
}

/** Refetch everything a chat change touches. */
export function useInvalidateChat() {
  const client = useQueryClient();
  return (chatId?: string) => {
    client.invalidateQueries({ queryKey: ["chats"] });
    if (chatId) {
      client.invalidateQueries({ queryKey: ["chat", chatId] });
      client.invalidateQueries({ queryKey: ["chatMessages", chatId] });
    }
  };
}

export function useApiMutation<A, R>(fn: (arg: A) => Promise<R>, onDone?: (r: R) => void) {
  return useMutation({ mutationFn: fn, onSuccess: onDone });
}

/**
 * Runs waiting on a human, across every chat. A hub that predates the endpoint
 * (404) or a transient failure reads as "none": attention is a convenience.
 */
export const getApprovals = async (): Promise<ApprovalItem[]> => {
  try {
    return (await request<{ approvals: ApprovalItem[] }>("approvals")).approvals ?? [];
  } catch {
    return [];
  }
};

export function useApprovals(enabled = true) {
  return useQuery({ queryKey: chatKeys.approvals, queryFn: getApprovals, enabled, retry: false });
}

/**
 * Approve or deny with the card gone at once (U31). The approval leaves the run
 * and the global approvals caches before the POST is sent; a failure puts it
 * back and says why, except 409 (already decided elsewhere or timed out), where
 * gone is the truth.
 */
export function useDecideApproval() {
  const client = useQueryClient();
  const toast = useToast();
  const [dismissed, setDismissed] = useState<ReadonlySet<string>>(new Set());
  const [inflight, setInflight] = useState<ReadonlySet<string>>(new Set());
  const flag = (set: (f: (s: ReadonlySet<string>) => ReadonlySet<string>) => void, id: string, on: boolean) =>
    set((s) => {
      const next = new Set(s);
      if (on) next.add(id);
      else next.delete(id);
      return next;
    });

  const resolve = useCallback(
    async (a: { runId: string; callId: string }, send: () => Promise<void>) => {
      if (inflight.has(a.callId)) return;
      const runKey = chatKeys.run(a.runId);
      const before = { run: client.getQueryData<Run>(runKey), approvals: client.getQueryData<ApprovalItem[]>(chatKeys.approvals) };
      flag(setInflight, a.callId, true);
      flag(setDismissed, a.callId, true);
      client.setQueryData<Run>(runKey, (r) =>
        r ? { ...r, pendingApprovals: (r.pendingApprovals ?? []).filter((p) => p.callId !== a.callId) } : r,
      );
      client.setQueryData<ApprovalItem[]>(chatKeys.approvals, (l) => l?.filter((p) => p.callId !== a.callId));
      try {
        await send();
      } catch (error) {
        if (!(error instanceof ApiError && error.status === 409)) {
          // Restore: the decision did not go through, so the card must come back.
          flag(setDismissed, a.callId, false);
          if (before.run) client.setQueryData(runKey, before.run);
          if (before.approvals) client.setQueryData(chatKeys.approvals, before.approvals);
          toast.show(error instanceof Error ? error.message : "could not send decision");
        }
      } finally {
        flag(setInflight, a.callId, false);
        client.invalidateQueries({ queryKey: runKey });
        client.invalidateQueries({ queryKey: chatKeys.approvals });
      }
    },
    [client, toast, inflight],
  );
  const decide = useCallback(
    (a: { runId: string; callId: string }, approved: boolean) => resolve(a, () => decideApproval(a.runId, a.callId, approved)),
    [resolve],
  );
  const answer = useCallback(
    (a: { runId: string; callId: string }, answers: string[][]) => resolve(a, () => answerQuestion(a.runId, a.callId, answers)),
    [resolve],
  );
  return { decide, answer, dismissed, inflight };
}
