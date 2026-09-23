/** Skills (agents/skills.go): named instructions run with /name, and optionally loadable by the model. */

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { request } from "../../api/client";

export interface Skill {
  id: string;
  name: string;
  description: string;
  body: string;
  /** The model sees its name and description and may load it itself. */
  auto: boolean;
  createdAt: string;
  updatedAt: string;
}

export type SkillInput = Partial<Pick<Skill, "name" | "description" | "body" | "auto">>;

export const skillKeys = { list: ["skills", "list"] as const };

export function useSkills(enabled = true) {
  return useQuery({
    queryKey: skillKeys.list,
    queryFn: async () => (await request<{ skills?: Skill[] }>("skills")).skills ?? [],
    enabled,
  });
}

function useInvalidate() {
  const client = useQueryClient();
  // A skill's name and description are part of every chat's system prompt.
  return () => {
    client.invalidateQueries({ queryKey: ["skills"] });
    client.invalidateQueries({ queryKey: ["chatSystemPrompt"] });
    client.invalidateQueries({ queryKey: ["chatTools"] });
  };
}

export function useCreateSkill() {
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: (body: SkillInput) => request<Skill>("skills", { method: "POST", body: JSON.stringify(body) }),
    onSuccess: invalidate,
  });
}

export function usePatchSkill() {
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: ({ id, patch }: { id: string; patch: SkillInput }) =>
      request<Skill>(`skills/${encodeURIComponent(id)}`, { method: "PATCH", body: JSON.stringify(patch) }),
    onSuccess: invalidate,
  });
}

export function useDeleteSkill() {
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: (id: string) => request<void>(`skills/${encodeURIComponent(id)}`, { method: "DELETE" }),
    onSuccess: invalidate,
  });
}

/** What follows the slash: lowercase letters, digits, - and _ (mirrors the hub's check). */
export const SKILL_NAME = /^[a-z0-9][a-z0-9_-]{0,63}$/;
