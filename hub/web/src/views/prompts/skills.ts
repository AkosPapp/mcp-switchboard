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

/** What follows the slash: the opencode grammar (mirrors the hub's check). */export const SKILL_NAME = /^[a-z0-9]+(-[a-z0-9]+)*$/;

/** The SKILL.md file verbatim, as the hub stores it on disk. */
export interface SkillRaw {
  markdown: string;
}

export function useSkillRaw(id: string | null) {
  return useQuery({
    queryKey: [...skillKeys.list, "raw", id ?? ""],
    queryFn: async () => (await request<SkillRaw>(`skills/${encodeURIComponent(id!)}/raw`)).markdown,
    enabled: id !== null,
  });
}

export function usePutSkillRaw() {
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: ({ id, markdown }: { id: string; markdown: string }) =>
      request<Skill>(`skills/${encodeURIComponent(id)}/raw`, { method: "PUT", body: JSON.stringify({ markdown }) }),
    onSuccess: invalidate,
  });
}

/** Skills scanned on client hosts, stored read-only under skills/hosts/. */
export interface HostSkill {
  host: string;
  name: string;
  description: string;
  source: string;
  path: string;
  /** A managed skill of the same name wins; this copy is only a mirror. */
  shadowed: boolean;
}

export const hostSkillKeys = { list: ["skills", "hosts"] as const };

export function useHostSkills(enabled = true) {
  return useQuery({
    queryKey: hostSkillKeys.list,
    queryFn: async () => (await request<{ skills?: HostSkill[] }>("skills/hosts")).skills ?? [],
    enabled,
  });
}

export function useImportHostSkill() {
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: ({ host, name }: { host: string; name: string }) =>
      request<Skill>("skills/import", { method: "POST", body: JSON.stringify({ host, name }) }),
    onSuccess: invalidate,
  });
}

/** The persona prompt ($DATA_DIR/prompts/base.md): the identity leading every chat. */
export const personaKeys = { get: ["persona"] as const };

export function usePersona() {
  return useQuery({
    queryKey: personaKeys.get,
    queryFn: async () => (await request<{ persona: string }>("persona")).persona,
  });
}

export function usePutPersona() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (persona: string) => request<{ ok: boolean }>("persona", { method: "PUT", body: JSON.stringify({ persona }) }),
    onSuccess: () => {
      client.invalidateQueries({ queryKey: personaKeys.get });
      client.invalidateQueries({ queryKey: ["chatSystemPrompt"] });
    },
  });
}
