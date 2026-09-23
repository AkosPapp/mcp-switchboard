import { useEffect, useRef, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";

import { useToast } from "../../components/Toast";
import { forgetTab } from "../../lib/routeMemory";
import { Chip } from "../graph/ui";
import { buttonClass } from "../graph/ui";
import {
  messageOf,
  useCreateProfile,
  useDeleteProfile,
  usePatchProfile,
  useProfiles,
  type Profile,
  type ProfileInput,
} from "./api";
import ProfileForm from "./ProfileForm";

type Selection = { kind: "new" } | { kind: "edit"; id: string } | null;

function ProfileRow({
  profile,
  selected,
  onSelect,
}: {
  profile: Profile;
  selected: boolean;
  onSelect: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onSelect}
      aria-current={selected ? "true" : undefined}
      className={[
        "block min-h-[44px] w-full rounded px-3 py-2 text-left transition-colors",
        selected ? "bg-accent/15" : "hover:bg-raised",
      ].join(" ")}
    >
      <div className="flex items-center gap-2">
        <span className="truncate text-sm font-medium">{profile.name}</span>
        {profile.isDefault && (
          <span className="rounded bg-accent/15 px-1.5 py-0.5 text-xs text-accent">default</span>
        )}
      </div>
      {profile.description && (
        <p className="truncate text-xs text-muted">{profile.description}</p>
      )}
      <div className="mt-1 flex flex-wrap gap-1">
        <Chip title="default model">
          {profile.model ? `${profile.model.provider}/${profile.model.model}` : "hub default model"}
        </Chip>
        {profile.capabilities.canSpawn && <Chip title="switchboard.chat.spawn, chat.list, chat.stop">can create sub-chats</Chip>}
        {profile.capabilities.canMessage && <Chip title="switchboard.chat.send, chat.list">can message chats</Chip>}
        <Chip title="approval mode">approval: {profile.approval}</Chip>
      </div>
    </button>
  );
}

export default function PromptsView() {
  const toast = useToast();
  const profiles = useProfiles();
  const create = useCreateProfile();
  const patch = usePatchProfile();
  const del = useDeleteProfile();
  // The selected prompt is in the URL (/prompts/:profileId); only the
  // "new prompt" form, which has no id yet, is local state.
  const { profileId } = useParams();
  const navigate = useNavigate();
  const [creating, setCreating] = useState(false);
  const selection: Selection = creating
    ? { kind: "new" }
    : profileId
      ? { kind: "edit", id: profileId }
      : null;
  const [error, setError] = useState<string | null>(null);
  const [confirming, setConfirming] = useState(false);

  const list = profiles.data ?? [];
  const current =
    selection?.kind === "edit" ? (list.find((p) => p.id === selection.id) ?? null) : null;

  const select = (next: Selection) => {
    setCreating(next?.kind === "new");
    navigate(next?.kind === "edit" ? `/prompts/${next.id}` : "/prompts");
    setError(null);
    setConfirming(false);
  };

  // A remembered prompt that no longer exists: checked once, against the first
  // list that loads, so a profile created a moment ago is never bounced.
  const checked = useRef(false);
  useEffect(() => {
    if (checked.current || !profiles.data) return;
    checked.current = true;
    if (profileId && !profiles.data.some((p) => p.id === profileId)) {
      forgetTab("/prompts");
      navigate("/prompts", { replace: true });
    }
  }, [profiles.data, profileId, navigate]);

  const save = (input: ProfileInput) => {
    setError(null);
    if (selection?.kind === "new") {
      create.mutate(input, {
        onSuccess: (created) => {
          toast.show("Created");
          select({ kind: "edit", id: created.id });
        },
        onError: (e) => setError(messageOf(e)),
      });
    } else if (current) {
      patch.mutate(
        { id: current.id, patch: input },
        { onSuccess: () => toast.show("Saved"), onError: (e) => setError(messageOf(e)) },
      );
    }
  };

  const makeDefault = (p: Profile) =>
    patch.mutate(
      { id: p.id, patch: { isDefault: true } },
      {
        onSuccess: () => toast.show(`${p.name} is now the default`),
        onError: (e) => setError(messageOf(e)),
      },
    );

  const remove = (p: Profile) =>
    del.mutate(p.id, {
      onSuccess: () => {
        toast.show("Deleted");
        select(null);
      },
      onError: (e) => {
        setConfirming(false);
        setError(messageOf(e)); // e.g. 409: the default profile cannot be deleted
      },
    });

  return (
    <div className="flex h-full min-h-0 flex-col md:flex-row" data-testid="prompts-view">
      <aside
        className={[
          "min-h-0 flex-col border-border md:flex md:w-96 md:border-r",
          selection !== null ? "hidden" : "flex flex-1",
        ].join(" ")}
      >
        <div className="flex items-center justify-between gap-2 border-b border-border p-2">
          <h2 className="px-1 text-sm font-semibold">Prompts</h2>
          <button
            type="button"
            className={`${buttonClass("primary")} min-h-[44px] md:min-h-[36px]`}
            onClick={() => select({ kind: "new" })}
          >
            New prompt
          </button>
        </div>
        <div className="min-h-0 flex-1 space-y-1 overflow-auto p-1" aria-label="Prompts">
          {profiles.isLoading && <p className="p-3 text-sm text-muted">Loading…</p>}
          {profiles.error && (
            <p className="p-3 text-sm text-danger">{messageOf(profiles.error)}</p>
          )}
          {list.map((p) => (
            <ProfileRow
              key={p.id}
              profile={p}
              selected={selection?.kind === "edit" && selection.id === p.id}
              onSelect={() => select({ kind: "edit", id: p.id })}
            />
          ))}
        </div>
      </aside>

      <section
        className={["min-h-0 flex-1 overflow-auto", selection === null ? "hidden md:block" : "block"].join(" ")}
      >
        {selection === null && (
          <div className="mx-auto max-w-prose p-6 text-sm text-muted">
            <h2 className="text-base font-semibold text-text">Prompts</h2>
            <p className="mt-2">
              A prompt is a reusable template: a system prompt, a default model, the hub tools a
              chat may use and an approval mode. Chats follow their prompt live, so editing it
              changes them too. Which client&apos;s tools a chat can use is chosen per chat, not here.
            </p>
          </div>
        )}

        {selection !== null && (
          <div className="mx-auto max-w-2xl">
            <div className="flex flex-wrap items-center gap-2 border-b border-border p-2">
              <button
                type="button"
                className={`${buttonClass()} min-h-[44px] md:hidden`}
                onClick={() => select(null)}
              >
                ← Back
              </button>
              <h2 className="min-w-0 flex-1 truncate px-1 text-sm font-semibold">
                {current ? current.name : "New prompt"}
              </h2>
              {current && !current.isDefault && (
                <button
                  type="button"
                  className={`${buttonClass()} min-h-[44px] md:min-h-[36px]`}
                  onClick={() => makeDefault(current)}
                  disabled={patch.isPending}
                >
                  Make default
                </button>
              )}
              {current &&
                (confirming ? (
                  <>
                    <button
                      type="button"
                      className={`${buttonClass("danger")} min-h-[44px] md:min-h-[36px]`}
                      onClick={() => remove(current)}
                      disabled={del.isPending}
                    >
                      Confirm delete
                    </button>
                    <button
                      type="button"
                      className={`${buttonClass()} min-h-[44px] md:min-h-[36px]`}
                      onClick={() => setConfirming(false)}
                    >
                      Keep
                    </button>
                  </>
                ) : (
                  <button
                    type="button"
                    className={`${buttonClass("danger")} min-h-[44px] md:min-h-[36px]`}
                    onClick={() => setConfirming(true)}
                  >
                    Delete
                  </button>
                ))}
            </div>
            {selection.kind === "edit" && !current ? (
              <p className="p-4 text-sm text-muted">This prompt no longer exists.</p>
            ) : (
              <ProfileForm
                key={current ? `${current.id}:${current.updatedAt}` : "new"}
                profile={current}
                pending={create.isPending || patch.isPending}
                serverError={error}
                onSubmit={save}
                onCancel={() => select(null)}
              />
            )}
          </div>
        )}
      </section>
    </div>
  );
}
