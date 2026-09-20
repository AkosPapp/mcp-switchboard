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

export interface ClientInfo {
  name: string;
  version: string;
  instance: string;
  label: string;
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
  type: "connections" | "call" | "agent" | "graph" | "chat";
  callId?: string;
  agentId?: string;
  chatId?: string;
}
