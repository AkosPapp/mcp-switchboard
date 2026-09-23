import { useState, type FormEvent } from "react";

import type { ApprovalMode } from "../../api/types";
import { ModelPicker, NoToolsWarning } from "../../lib/models";
import { fieldClass, buttonClass } from "../graph/ui";
import { useProfileModels, type Profile, type ProfileInput } from "./api";
import { APPROVALS, initialValues, toInput, type FormErrors, type FormValues } from "./form";

const HELP = "text-xs text-muted";

export default function ProfileForm({
  profile,
  pending,
  serverError,
  onSubmit,
  onCancel,
}: {
  profile: Profile | null;
  pending: boolean;
  serverError: string | null;
  onSubmit: (input: ProfileInput) => void;
  onCancel: () => void;
}) {
  const models = useProfileModels();
  const [values, setValues] = useState<FormValues>(() => initialValues(profile));
  const [errors, setErrors] = useState<FormErrors>({});
  const set = <K extends keyof FormValues>(key: K, value: FormValues[K]) =>
    setValues((v) => ({ ...v, [key]: value }));

  const options = models.data ?? [];

  const submit = (event: FormEvent) => {
    event.preventDefault();
    const result = toInput(values, profile);
    if (result.errors) {
      setErrors(result.errors);
      return;
    }
    setErrors({});
    onSubmit(result.input);
  };

  const approval = APPROVALS.find((a) => a.value === values.approval);

  return (
    <form onSubmit={submit} noValidate aria-label="Prompt" className="space-y-4 p-4">
      <p className={HELP}>
        A prompt is a reusable template for chats. It does not choose which client&apos;s tools
        a chat can use — that is chosen per chat.
      </p>

      <label className="block text-sm">
        Name
        <input
          className={fieldClass()}
          value={values.name}
          onChange={(e) => set("name", e.target.value)}
          aria-invalid={errors.name ? true : undefined}
        />
        {errors.name && <span className="text-xs text-danger">{errors.name}</span>}
      </label>

      <label className="block text-sm">
        Description
        <input
          className={fieldClass()}
          value={values.description}
          onChange={(e) => set("description", e.target.value)}
        />
      </label>

      <label className="block text-sm">
        System prompt
        <textarea
          className={`${fieldClass()} font-mono text-xs`}
          rows={8}
          value={values.systemPrompt}
          onChange={(e) => set("systemPrompt", e.target.value)}
          aria-invalid={errors.systemPrompt ? true : undefined}
        />
        {errors.systemPrompt && <span className="text-xs text-danger">{errors.systemPrompt}</span>}
      </label>

      <div className="text-sm">
        <span id="default-model-label">Default model</span>
        <ModelPicker
          models={options}
          value={values.modelKey}
          onChange={(key) => set("modelKey", key)}
          emptyLabel="hub default"
          ariaLabel="Default model"
          className={`${fieldClass()} flex min-h-[44px] items-center justify-between gap-2 text-left md:min-h-0`}
        />
        <NoToolsWarning models={options} value={values.modelKey} />
      </div>

      <fieldset className="space-y-1 text-sm">
        <legend>Hub tools</legend>
        <p className={HELP}>
          Tools the hub itself provides to a chat, on top of the chosen client&apos;s tools.
        </p>
        <label className="flex min-h-[44px] items-start gap-2 py-2">
          <input
            type="checkbox"
            className="mt-1"
            checked={values.canSpawn}
            onChange={(e) => set("canSpawn", e.target.checked)}
          />
          <span>
            Can create sub-chats
            <span className={`block ${HELP}`}>
              Enables switchboard.chat.spawn, chat.list, chat.stop and mcp.list_tools. Sub-chats
              inherit this prompt&apos;s client, approval mode and model. Connecting chats and
              granting MCP servers is done from the Graph view, not by the chat itself.
            </span>
          </span>
        </label>
        <label className="flex min-h-[44px] items-start gap-2 py-2">
          <input
            type="checkbox"
            className="mt-1"
            checked={values.canMessage}
            onChange={(e) => set("canMessage", e.target.checked)}
          />
          <span>
            Can message other chats
            <span className={`block ${HELP}`}>
              Enables switchboard.chat.send, switchboard.chat.list and switchboard.mcp.list_tools.
            </span>
          </span>
        </label>
      </fieldset>

      <label className="block text-sm">
        Approval mode
        <select
          className={`${fieldClass()} min-h-[44px] md:min-h-0`}
          value={values.approval}
          onChange={(e) => set("approval", e.target.value as ApprovalMode)}
        >
          {APPROVALS.map((a) => (
            <option key={a.value} value={a.value}>
              {a.value}
            </option>
          ))}
        </select>
        <span className={HELP}>{approval?.help}</span>
      </label>

      <label className="block text-sm">
        Budget (JSON, optional)
        <textarea
          className={`${fieldClass()} font-mono text-xs`}
          rows={3}
          value={values.budget}
          onChange={(e) => set("budget", e.target.value)}
          aria-invalid={errors.budget ? true : undefined}
        />
        <span className={HELP}>{"{}"} uses the hub defaults.</span>
        {errors.budget && <span className="block text-xs text-danger">{errors.budget}</span>}
      </label>

      {serverError && (
        <p role="alert" className="text-sm text-danger">
          {serverError}
        </p>
      )}

      <div className="flex gap-2">
        <button
          type="submit"
          className={`${buttonClass("primary")} min-h-[44px] md:min-h-[36px]`}
          disabled={pending}
        >
          {profile ? "Save" : "Create prompt"}
        </button>
        <button
          type="button"
          className={`${buttonClass()} min-h-[44px] md:min-h-[36px]`}
          onClick={onCancel}
        >
          Cancel
        </button>
      </div>
    </form>
  );
}
