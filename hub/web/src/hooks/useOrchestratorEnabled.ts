import { useQuery } from "@tanstack/react-query";

import { ApiError, request } from "../api/client";

/**
 * Whether the hub runs the orchestrator. Every orchestrator route answers 404
 * when it is disabled, so GET /api/models is the probe (docs/API.md). `undefined`
 * while the answer is unknown, so the nav does not flash.
 */
export function useOrchestratorEnabled(): boolean | undefined {
  const query = useQuery({
    queryKey: ["orchestrator-enabled"],
    queryFn: async () => {
      try {
        await request("models");
        return true;
      } catch (error) {
        if (error instanceof ApiError && error.status === 404) return false;
        throw error;
      }
    },
    staleTime: Infinity,
    retry: false,
  });
  return query.data;
}
