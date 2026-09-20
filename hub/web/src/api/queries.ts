/**
 * TanStack Query bindings.
 *
 * The keys are exported because the event stream invalidates by key rather than
 * patching cached entries by hand (spec.md U3): a change notification says only
 * that something changed, so the honest response is to refetch it.
 */

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import {
  callTool,
  getCall,
  getCalls,
  getConnections,
  getEndpoints,
  getStats,
  restartServer,
  type CallFilters,
} from "./client";

export const keys = {
  connections: ["connections"] as const,
  endpoints: ["endpoints"] as const,
  stats: ["stats"] as const,
  calls: (filters: CallFilters) => ["calls", filters] as const,
  call: (id: string) => ["call", id] as const,
};

export function useConnections() {
  return useQuery({ queryKey: keys.connections, queryFn: getConnections });
}

export function useEndpoints() {
  return useQuery({ queryKey: keys.endpoints, queryFn: getEndpoints });
}

export function useStats() {
  return useQuery({ queryKey: keys.stats, queryFn: getStats });
}

export function useCalls(filters: CallFilters) {
  return useQuery({
    queryKey: keys.calls(filters),
    queryFn: () => getCalls(filters),
    // A list of calls that just happened is worth keeping on screen while the
    // next page loads, rather than flashing a spinner on every filter change.
    placeholderData: (previous) => previous,
  });
}

export function useCall(id: string | null) {
  return useQuery({
    queryKey: keys.call(id ?? ""),
    queryFn: () => getCall(id as string),
    enabled: id !== null,
  });
}

export function useCallTool() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: callTool,
    onSuccess: () => {
      // The call log gained a row; the caller shows the result itself.
      client.invalidateQueries({ queryKey: ["calls"] });
    },
  });
}

export function useRestartServer() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ connectionId, server }: { connectionId: string; server: string }) =>
      restartServer(connectionId, server),
    onSuccess: () => {
      client.invalidateQueries({ queryKey: keys.connections });
    },
  });
}
