/**
 * The shared query hooks for agents (the execution record behind each chat: its
 * status and model) and profiles (shown as "prompts"): ONE hook, ONE cached
 * shape per resource, used by the Chat panel, the Prompts tab, attention and
 * the graph's invalidations. Two modules once cached different shapes under one key
 * ("Cannot read properties of undefined (reading 'find')"), so nothing outside
 * this file may build a query key for these resources.
 *
 * Every key sits under its resource's root (["agents"], ["profiles"]) so the
 * global event feed's prefix invalidation reaches all of them.
 */

import { useQuery } from "@tanstack/react-query";

import { request } from "./client";
import type { Agent, ApprovalMode, ModelInfo } from "./types";

export interface ProfileModel {
  provider: string;
  model: string;
  [key: string]: unknown;
}

export interface Profile {
  id: string;
  name: string;
  description: string;
  systemPrompt: string;
  model: ProfileModel | null;
  capabilities: { canSpawn: boolean; canMessage: boolean };
  approval: ApprovalMode;
  budget: Record<string, number>;
  isDefault: boolean;
  createdAt: string;
  updatedAt: string;
}

export type ProfileInput = Partial<Omit<Profile, "id" | "createdAt" | "updatedAt">>;

export const resourceKeys = {
  agents: ["agents", "list"] as const,
  agent: (id: string) => ["agents", "detail", id] as const,
  profiles: ["profiles", "list"] as const,
  profileModels: ["profiles", "models"] as const,
};


export const getAgents = async (): Promise<Agent[]> =>
  (await request<{ agents?: Agent[] }>("agents")).agents ?? [];
export const getProfiles = async (): Promise<Profile[]> =>
  (await request<{ profiles?: Profile[] }>("profiles")).profiles ?? [];

export function useAgents(enabled = true) {
  return useQuery({ queryKey: resourceKeys.agents, queryFn: getAgents, enabled });
}

export function useAgent(id: string | null) {
  return useQuery({
    queryKey: resourceKeys.agent(id ?? ""),
    queryFn: () => request<Agent>(`agents/${encodeURIComponent(id as string)}`),
    enabled: id !== null,
  });
}

export function useProfiles(enabled = true) {
  return useQuery({ queryKey: resourceKeys.profiles, queryFn: getProfiles, enabled });
}

export function useProfileModels() {
  return useQuery({
    queryKey: resourceKeys.profileModels,
    queryFn: async () => (await request<{ models: ModelInfo[] }>("models")).models,
    staleTime: 60_000,
  });
}
