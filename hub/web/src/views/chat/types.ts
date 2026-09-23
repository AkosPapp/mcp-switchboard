/** Wire types of the orchestrator surface (docs/API.md), chat-side subset. */

export type Ts = string;

export interface ContentBlock {
  type: "text" | "image" | "thinking" | "tool_use" | "tool_result" | string;
  text?: string;
  media_type?: string;
  data?: string;
  signature?: string;
  [key: string]: unknown;
}

export interface Siblings {
  ids: string[];
  index: number;
}

export interface ToolCallRef {
  id: string;
  name: string;
  arguments: unknown;
}

export interface ToolResultRef {
  tool_call_id: string;
  call_id?: string;
  result?: unknown;
  error?: string | null;
}

export type Role = "system" | "user" | "assistant" | "tool";

/**
 * Set on a user-role message another chat injected (docs/CHAT_MODEL_API.md):
 * it is not something the human typed, so it is neither editable nor regenerable.
 */
export interface MessageSender {
  chatId: string;
  /** the sender's title at the time of sending */
  chatTitle: string;
  /** message = a send, reply = a returned answer, spawn = the parent's first task */
  kind: "message" | "reply" | "spawn";
}

export interface Message {
  id: string;
  chatId: string;
  parentId: string | null;
  role: Role;
  content: ContentBlock[];
  toolCalls: ToolCallRef[] | null;
  toolResults: ToolResultRef[] | null;
  tokenInput: number;
  tokenOutput: number;
  costMicros: number;
  latencyMs: number;
  model: { provider?: string; model?: string; [k: string]: unknown } | null;
  finishReason: string | null;
  runId: string | null;
  lastActiveChildId: string | null;
  createdAt: Ts;
  /** Added by the messages and export endpoints; absent on stream payloads. */
  siblings?: Siblings;
  /** Present on a message injected by another chat. */
  sender?: MessageSender | null;
}

export interface Chat {
  id: string;
  agentId: string;
  peerAgentId: string | null;
  title: string;
  kind: "human" | "agent" | "spawn";
  activeLeafId: string | null;
  tags: string[];
  tokenTotal: number;
  costTotalMicros: number;
  createdAt: Ts;
  updatedAt: Ts;
  archivedAt: Ts | null;
  /** The prompt (profile) the chat follows by live reference; null = its own prompt or none. */
  profileId?: string | null;
  /** The one MCP client this chat may use; null = none. */
  clientLabel?: string | null;
  /** The chat that spawned this one; the console nests it under that chat. */
  parentChatId?: string | null;
}

/** POST /api/chats (docs/CHAT_MODEL_API.md). */
export interface CreateChatInput {
  title?: string;
  /** A profile id follows it live; an explicit null means none (or the custom text). */
  profileId?: string | null;
  systemPrompt?: string;
  /** Always sent: a client label, or an explicit null for none. */
  clientLabel: string | null;
  model?: object;
  parentChatId?: string;
}

export interface ToolAnnotations {
  readOnlyHint?: boolean;
  destructiveHint?: boolean;
  idempotentHint?: boolean;
  openWorldHint?: boolean;
}

export interface ChatTool {
  name: string;
  description: string;
  origin: "mcp" | "hub";
  server?: { label: string; project: string; server: string };
  annotations?: ToolAnnotations;
  inputSchema?: object;
}

export interface ChatToolsResponse {
  clientLabel: string | null;
  clientConnected: boolean;
  tools: ChatTool[];
}

export interface SearchHit {
  chatId: string;
  chatTitle: string;
  agentId: string;
  messageId: string;
  role: string;
  snippet: string;
}

export interface ChatList {
  chats: Chat[];
  limit: number;
  offset: number;
  hits?: Record<string, SearchHit[]>;
}

export interface MessageList {
  chatId: string;
  activeLeafId: string | null;
  tree: boolean;
  messages: Message[];
}

/** The run record behind a chat, shared with the Graph and attention (api/resources). */
export type { Agent } from "../../api/types";
export type { Profile } from "../../api/resources";

/** GET /api/chats/{id}/system-prompt: exactly what the run loop sends next turn. */
export interface SystemPromptInfo {
  systemPrompt: string;
  source: "profile" | "agent" | "none";
  profileId: string | null;
  profileName: string | null;
  model: { provider: string; model: string } | null;
  modelIsDefault: boolean;
  toolCount: number;
}

export type RunStatus =
  | "queued"
  | "running"
  | "waiting"
  | "done"
  | "cancelled"
  | "interrupted"
  | "error";

/** A question the model asked the user with switchboard.user.ask. */
export interface Question {
  question: string;
  header?: string;
  options?: { label: string; description?: string }[];
  multiSelect?: boolean;
}

export interface PendingApproval {
  callId: string;
  tool: string;
  arguments: unknown;
  expiresAt: Ts;
  /** Set when the run waits for answers rather than an approval. */
  questions?: Question[];
}

export interface Run {
  id: string;
  agentId: string;
  chatId: string;
  status: RunStatus;
  budgetSnapshot: Record<string, unknown>;
  usage: Record<string, unknown>;
  error: string | null;
  finishReason: string | null;
  pendingApprovals?: PendingApproval[];
}

export type { ModelInfo } from "../../api/types";

export const ACTIVE_RUN: RunStatus[] = ["queued", "running", "waiting"];

/** One entry of GET /api/approvals: a run waiting on a human, in any chat. */
export interface ApprovalItem {
  runId: string;
  chatId: string;
  agentId: string;
  agentName: string;
  chatTitle: string;
  callId: string;
  tool: string;
  arguments: unknown;
  expiresAt: Ts;
  questions?: Question[];
}
