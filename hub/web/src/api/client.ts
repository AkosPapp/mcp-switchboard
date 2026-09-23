/**
 * Talking to the hub.
 *
 * Every response the console shows comes through here, so the two rules that
 * matter live in one place: a tool error is not a transport error (spec.md N1),
 * and a failed request carries the hub's own message rather than a status code.
 */

import type {
  CallList,
  CallRecord,
  Endpoints,
  PushSubscriptionRecord,
  Snapshot,
  Stats,
  VapidPublicKey,
} from "./types";

/** Thrown for a request the hub refused. `status` is the HTTP status. */
export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

/**
 * The hub serves the console from its own origin, so requests are relative.
 * Kept as a constant rather than inlined because the dev server proxies /api to
 * a real hub and tests point it at a fake one.
 */
export const API_BASE = "api";

export async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(`${API_BASE}/${path}`, {
    headers: { "Content-Type": "application/json" },
    ...init,
  });

  if (!response.ok) {
    // The hub answers errors as {"detail": "..."}; fall back to the status line
    // for anything that is not our own handler (a proxy, say).
    let detail = response.statusText;
    try {
      const body = await response.json();
      if (body && typeof body.detail === "string") detail = body.detail;
      else if (body && typeof body.error === "string") detail = body.error;
    } catch {
      // Not JSON. The status line is all there is.
    }
    throw new ApiError(detail, response.status);
  }

  if (response.status === 204) return undefined as T;
  return (await response.json()) as T;
}

export function getConnections(): Promise<Snapshot> {
  return request<Snapshot>("connections");
}

export function getEndpoints(): Promise<Endpoints> {
  return request<Endpoints>("endpoints");
}

export function getStats(): Promise<Stats> {
  return request<Stats>("stats");
}

export interface CallFilters {
  label?: string;
  server?: string;
  tool?: string;
  status?: string;
  source?: string;
  limit?: number;
  offset?: number;
}

export function getCalls(filters: CallFilters = {}): Promise<CallList> {
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(filters)) {
    if (value !== undefined && value !== "" && value !== null) {
      query.set(key, String(value));
    }
  }
  const suffix = query.toString();
  return request<CallList>(`calls${suffix ? `?${suffix}` : ""}`);
}

export function getCall(id: string): Promise<CallRecord> {
  return request<CallRecord>(`calls/${encodeURIComponent(id)}`);
}

export interface CallToolArgs {
  connectionId: string;
  server: string;
  tool: string;
  args: Record<string, unknown>;
}

/**
 * Call a tool by hand.
 *
 * Resolves with the recorded call whether the tool succeeded or failed: N1
 * again, a tool that answers with an error is a call that happened. Only an
 * unknown connection, server or tool rejects.
 */
export function callTool({
  connectionId,
  server,
  tool,
  args,
}: CallToolArgs): Promise<CallRecord> {
  const path =
    `connections/${encodeURIComponent(connectionId)}` +
    `/servers/${encodeURIComponent(server)}` +
    `/tools/${encodeURIComponent(tool)}/call`;
  return request<CallRecord>(path, {
    method: "POST",
    body: JSON.stringify({ arguments: args }),
  });
}

export function restartServer(connectionId: string, server: string): Promise<void> {
  const path =
    `connections/${encodeURIComponent(connectionId)}` +
    `/servers/${encodeURIComponent(server)}/restart`;
  return request<void>(path, { method: "POST" });
}

// --------------------------------------------------------------------------
// Web Push (spec.md 8.7)
// --------------------------------------------------------------------------

/** Throws ApiError(404) when the hub has no VAPID keys configured - the
 * Settings toggle treats that the same as "unsupported browser". */
export function getVapidPublicKey(): Promise<VapidPublicKey> {
  return request<VapidPublicKey>("push/vapid-public-key");
}

export function subscribePush(
  subscription: PushSubscriptionJSON,
  label?: string,
): Promise<PushSubscriptionRecord> {
  return request<PushSubscriptionRecord>("push/subscribe", {
    method: "POST",
    body: JSON.stringify({
      endpoint: subscription.endpoint,
      keys: subscription.keys,
      label: label ?? null,
    }),
  });
}

export function unsubscribePush(id: string): Promise<void> {
  return request<void>(`push/subscriptions/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}
