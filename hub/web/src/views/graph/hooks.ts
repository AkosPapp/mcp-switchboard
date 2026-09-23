/**
 * Queries and mutations for the graph view.
 *
 * Nothing here caches by time: the global feed invalidates ["graph"] on every
 * chat/graph event (spec.md U17), and each mutation invalidates it too so the
 * console does not depend on the feed to see its own write.
 */

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { request } from "../../api/client";
import type {
  Agent,
  Edge,
  Grant,
  GrantInput,
  GraphView,
  ModelInfo,
  PatchAgentInput,
} from "../../api/types";

export const graphKeys = {
  graph: ["graph"] as const,
  // Not ["models"]: the chat view caches the raw { models } response under that
  // key, and two shapes under one key crash whichever view loads second.
  models: ["graph", "models"] as const,
};

export function useGraph() {
  return useQuery({
    queryKey: graphKeys.graph,
    queryFn: () => request<GraphView>("graph"),
    // Push invalidation is the freshness mechanism; refetch on mount only if
    // something invalidated it in the meantime.
    staleTime: Infinity,
  });
}

export function useModels() {
  return useQuery({
    queryKey: graphKeys.models,
    queryFn: async () => (await request<{ models: ModelInfo[] }>("models")).models,
    staleTime: 60_000,
  });
}

function useInvalidateGraph() {
  const client = useQueryClient();
  return () => {
    client.invalidateQueries({ queryKey: graphKeys.graph });
    client.invalidateQueries({ queryKey: ["agents"] });
  };
}

export function useSetGrants() {
  const invalidate = useInvalidateGraph();
  return useMutation({
    mutationFn: ({ agentId, grants }: { agentId: string; grants: GrantInput[] }) =>
      request<{ grants: Grant[]; revoked: Grant[] }>(
        `agents/${encodeURIComponent(agentId)}/grants`,
        { method: "PUT", body: JSON.stringify({ grants }) },
      ),
    onSuccess: invalidate,
  });
}

export function useSetEdge() {
  const invalidate = useInvalidateGraph();
  return useMutation({
    mutationFn: ({ from, to, allowed }: { from: string; to: string; allowed: boolean }) =>
      request<Edge>(`graph/edges/${encodeURIComponent(from)}/${encodeURIComponent(to)}`, {
        method: "PUT",
        body: JSON.stringify({ allowed }),
      }),
    onSuccess: invalidate,
  });
}

export function usePatchAgent() {
  const invalidate = useInvalidateGraph();
  return useMutation({
    mutationFn: ({ id, patch }: { id: string; patch: PatchAgentInput }) =>
      request<Agent>(`agents/${encodeURIComponent(id)}`, {
        method: "PATCH",
        body: JSON.stringify(patch),
      }),
    onSuccess: invalidate,
  });
}

export function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
