/**
 * The hub's HTTP surface, as TypeScript.
 *
 * These mirror the Go structs that serve them (internal/registry/json.go,
 * internal/store/store.go, internal/api). Absent optional values arrive as
 * `null` rather than as an empty string - the hub is deliberate about that, so
 * the console can tell "no project" from "a project named empty" - which is why
 * so many fields here are `| null` rather than optional.
 */

export interface ToolInfo {
  name: string;
  /** The name a consumer calls, or null when the tool was dropped under I4. */
  exposedName: string | null;
  title: string | null;
  description: string | null;
  inputSchema: JsonSchema;
}

export type ServerState = "starting" | "running" | "exited" | "failed";

export interface ServerInfo {
  name: string;
  project: string | null;
  command: string;
  state: ServerState;
  error: string | null;
  exitCode: number | null;
  toolCount: number;
  tools: ToolInfo[];
}

export interface ClientEnvironment {
  kinds: string[];
  project?: string;
  workspace?: string;
  details?: Record<string, string>;
}

export interface ClientInfo {
  name: string;
  version: string;
  instance: string;
  label: string;
  environment?: ClientEnvironment | null;
}

export interface ConnectionInfo {
  id: string;
  label: string;
  client: ClientInfo;
  connectedAt: string;
  servers: ServerInfo[];
}

export interface Snapshot {
  connections: ConnectionInfo[];
}

export type CallStatus = "ok" | "error" | "denied";
export type CallSource = "console" | "mcp" | "api" | "agent";

export interface CallRecord {
  id: string;
  connectionId: string;
  label: string;
  server: string;
  tool: string;
  exposedName: string;
  arguments: Record<string, unknown>;
  result: Record<string, unknown> | null;
  error: string | null;
  status: CallStatus;
  source: CallSource;
  startedAt: string;
  durationMs: number;
  agentId?: string | null;
  chatId?: string | null;
  runId?: string | null;
}

export interface CallList {
  calls: CallRecord[];
}

export interface EndpointRow {
  path: string;
  scope: string;
  description: string;
  example: string;
}

export interface Endpoints {
  localBaseUrl: string;
  publicUrl: string | null;
  installCommand: string | null;
  rows: EndpointRow[];
}

export interface Stats {
  databaseBytes: number;
  rows: Record<string, number>;
  calls: { total: number; ok: number; error: number };
}

/** Only the parts of JSON Schema the argument form knows how to render. */
export interface JsonSchema {
  type?: string | string[];
  properties?: Record<string, JsonSchema>;
  required?: string[];
  description?: string;
  title?: string;
  enum?: unknown[];
  default?: unknown;
  items?: JsonSchema;
  [key: string]: unknown;
}

/** An event from the global feed. Notifications only - never content (N3). */
export interface ChangeEvent {
  type: "connections" | "call" | "agent" | "graph" | "chat" | "profile";
  callId?: string;
  agentId?: string;
  chatId?: string;
}

/* ---- Orchestrator surface (docs/API.md) ---- */

export type AgentStatus = "idle" | "running" | "waiting" | "blocked" | "done" | "error";
export type AgentOrigin = "manual" | "spawn" | "chat";
export type ApprovalMode = "never" | "destructive" | "always";
export type GrantSource = "inherited" | "explicit" | "human";

export interface AgentModel {
  provider?: string;
  model?: string;
  [key: string]: unknown;
}

export interface Agent {
  id: string;
  parentId: string | null;
  name: string;
  description: string;
  project: string | null;
  model: AgentModel;
  systemPrompt: string;
  depth: number;
  status: AgentStatus;
  budget: Record<string, unknown>;
  capabilities: { canSpawn: boolean; canMessage: boolean };
  approval: ApprovalMode;
  autoWake: boolean;
  tokenTotal: number;
  costTotalMicros: number;
  createdAt: string;
  updatedAt: string;
  lastActivityAt: string;
  deletedAt: string | null;
  /** The execution record behind a chat (1:1 with it); the console never shows it as such. */
  origin?: AgentOrigin;
  /** The one MCP client the agent may use; null = none. */
  clientLabel?: string | null;
  /** Live profile reference; null = the agent's own systemPrompt. */
  profileId?: string | null;
}

export interface Grant {
  agentId: string;
  label: string;
  project: string;
  server: string;
  allowed: boolean;
  source: GrantSource;
  createdAt: string;
  updatedAt: string;
}

export interface GrantInput {
  label: string;
  project?: string;
  server: string;
  allowed: boolean;
}

export interface Edge {
  fromAgentId: string;
  toAgentId: string;
  allowed: boolean;
  createdAt: string;
  updatedAt: string;
}

export interface GraphAgent extends Agent {
  unreadMail: number;
  currentTool?: string;
}

export interface GraphGrant extends Grant {
  connected: boolean;
  orphaned?: boolean;
}

export interface GraphServer {
  label: string;
  project: string;
  server: string;
  connected: boolean;
  toolCount: number;
}

export interface GraphView {
  agents: GraphAgent[];
  edges: Edge[];
  grants: GraphGrant[];
  servers: GraphServer[];
}

export interface PatchAgentInput {
  name?: string;
  description?: string;
  project?: string | null;
  model?: AgentModel;
  systemPrompt?: string;
  budget?: Record<string, unknown>;
  capabilities?: { canSpawn: boolean; canMessage: boolean };
  approval?: ApprovalMode;
  autoWake?: boolean;
  clientLabel?: string | null;
  profileId?: string | null;
}

export interface ModelInfo {
  provider: string;
  model: string;
  prices?: Record<string, unknown>;
  contextWindow?: number;
  supportsTools?: boolean;
  /** Found from the provider rather than declared in the models file. */
  discovered?: boolean;
}

/** GET /api/push/vapid-public-key (spec.md 8.7). */
export interface VapidPublicKey {
  publicKey: string;
}

/** A registered Web Push subscription, as the hub stores and returns it. */
export interface PushSubscriptionRecord {
  id: string;
  endpoint: string;
  createdAt: string;
  lastSeenAt: string;
  label?: string | null;
}
