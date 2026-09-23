/** Prompts, called profiles on the wire (docs/PROFILES_API.md): templates that chats follow live. */

import { useMutation, useQueryClient } from "@tanstack/react-query";

import { request } from "../../api/client";
import { resourceKeys, type Profile, type ProfileInput } from "../../api/resources";

// One shape, one hook: the queries live in api/resources and are re-exported here.
export { useProfileModels, useProfiles } from "../../api/resources";
export type { Profile, ProfileInput, ProfileModel } from "../../api/resources";

function useInvalidate() {
  const client = useQueryClient();
  return () => {
    client.invalidateQueries({ queryKey: resourceKeys.profiles });
    // Chats resolve their prompt from the profile, so their views follow it.
    client.invalidateQueries({ queryKey: ["agents"] });
  };
}

export function useCreateProfile() {
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: (body: ProfileInput) =>
      request<Profile>("profiles", { method: "POST", body: JSON.stringify(body) }),
    onSuccess: invalidate,
  });
}

export function usePatchProfile() {
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: ({ id, patch }: { id: string; patch: ProfileInput }) =>
      request<Profile>(`profiles/${encodeURIComponent(id)}`, {
        method: "PATCH",
        body: JSON.stringify(patch),
      }),
    onSuccess: invalidate,
  });
}

export function useDeleteProfile() {
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: (id: string) =>
      request<void>(`profiles/${encodeURIComponent(id)}`, { method: "DELETE" }),
    onSuccess: invalidate,
  });
}

export function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
