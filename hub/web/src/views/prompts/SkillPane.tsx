import { useState, type FormEvent } from "react";

import { useToast } from "../../components/Toast";
import { buttonClass, fieldClass } from "../graph/ui";
import { messageOf } from "./api";
import { SKILL_NAME, useCreateSkill, useDeleteSkill, usePatchSkill, type Skill } from "./skills";

const HELP = "text-xs text-muted";

/** The editor for one skill (or a new one), with its header. */
export default function SkillPane({
  skill,
  onBack,
  onSaved,
  onDeleted,
}: {
  skill: Skill | null;
  onBack: () => void;
  onSaved: (skill: Skill) => void;
  onDeleted: () => void;
}) {
  const toast = useToast();
  const create = useCreateSkill();
  const patch = usePatchSkill();
  const del = useDeleteSkill();
  const [name, setName] = useState(skill?.name ?? "");
  const [description, setDescription] = useState(skill?.description ?? "");
  const [body, setBody] = useState(skill?.body ?? "");
  const [auto, setAuto] = useState(skill?.auto ?? true);
  const [error, setError] = useState<string | null>(null);
  const [confirming, setConfirming] = useState(false);
  const pending = create.isPending || patch.isPending;

  const submit = (event: FormEvent) => {
    event.preventDefault();
    const n = name.trim();
    if (!SKILL_NAME.test(n)) return setError("Name: lowercase letters, digits, - or _ (e.g. code-review).");
    if (!body.trim()) return setError("Write the instructions.");
    setError(null);
    const input = { name: n, description: description.trim(), body, auto };
    const opts = {
      onSuccess: (saved: Skill) => {
        toast.show(skill ? "Saved" : "Created");
        onSaved(saved);
      },
      onError: (e: unknown) => setError(messageOf(e)),
    };
    if (skill) patch.mutate({ id: skill.id, patch: input }, opts);
    else create.mutate(input, opts);
  };

  return (
    <div className="mx-auto max-w-2xl">
      <div className="flex flex-wrap items-center gap-2 border-b border-border p-2">
        <button type="button" className={`${buttonClass()} min-h-[44px] md:hidden`} onClick={onBack}>
          ← Back
        </button>
        <h2 className="min-w-0 flex-1 truncate px-1 text-sm font-semibold">{skill ? `/${skill.name}` : "New skill"}</h2>
        {skill &&
          (confirming ? (
            <>
              <button
                type="button"
                className={`${buttonClass("danger")} min-h-[44px] md:min-h-[36px]`}
                disabled={del.isPending}
                onClick={() =>
                  del.mutate(skill.id, {
                    onSuccess: () => {
                      toast.show("Deleted");
                      onDeleted();
                    },
                    onError: (e) => {
                      setConfirming(false);
                      setError(messageOf(e));
                    },
                  })
                }
              >
                Confirm delete
              </button>
              <button type="button" className={`${buttonClass()} min-h-[44px] md:min-h-[36px]`} onClick={() => setConfirming(false)}>
                Keep
              </button>
            </>
          ) : (
            <button type="button" className={`${buttonClass("danger")} min-h-[44px] md:min-h-[36px]`} onClick={() => setConfirming(true)}>
              Delete
            </button>
          ))}
      </div>

      <form onSubmit={submit} noValidate aria-label="Skill" className="space-y-4 p-4">
        <p className={HELP}>
          A skill is a block of instructions. Type <span className="font-mono">/name</span> in a chat to run it, followed by
          whatever you want it applied to.
        </p>

        <label className="block text-sm">
          Name
          <div className="flex items-center gap-1">
            <span className="font-mono text-muted">/</span>
            <input
              className={`${fieldClass()} font-mono`}
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="code-review"
              autoCapitalize="none"
              spellCheck={false}
            />
          </div>
        </label>

        <label className="block text-sm">
          Description
          <input
            className={fieldClass()}
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            placeholder="Review a change for bugs and unclear code"
          />
          <span className={HELP}>What it is for. Shown in the / menu, and to the model when it decides whether to load the skill.</span>
        </label>

        <label className="block text-sm">
          Instructions
          <textarea
            className={`${fieldClass()} font-mono text-xs`}
            rows={12}
            value={body}
            onChange={(e) => setBody(e.target.value)}
          />
        </label>

        <label className="flex min-h-[44px] items-start gap-2 py-2 text-sm">
          <input type="checkbox" className="mt-1" checked={auto} onChange={(e) => setAuto(e.target.checked)} />
          <span>
            Let the model use it
            <span className={`block ${HELP}`}>
              Its name and description are added to every chat&apos;s system prompt, and the model can load the instructions
              itself when a task matches. Off: the skill only runs when you type its /command.
            </span>
          </span>
        </label>

        {error && (
          <p role="alert" className="text-sm text-danger">
            {error}
          </p>
        )}

        <div className="flex gap-2">
          <button type="submit" className={`${buttonClass("primary")} min-h-[44px] md:min-h-[36px]`} disabled={pending}>
            {skill ? "Save" : "Create skill"}
          </button>
          <button type="button" className={`${buttonClass()} min-h-[44px] md:min-h-[36px]`} onClick={onBack}>
            Cancel
          </button>
        </div>
      </form>
    </div>
  );
}
