import { useState, type FormEvent } from "react";

import type { ApprovalMode, PatchAgentInput } from "../../api/types";
import { ModelPicker, NoToolsWarning } from "../../lib/models";
import { useToast } from "../../components/Toast";
import { messageOf, useModels, usePatchAgent } from "./hooks";
import type { GraphChat } from "./chatNodes";
import { buttonClass, fieldClass } from "./ui";

/** Editable run settings of a chat (U16); its title, prompt and client are in the chat's own settings. Only what changed is sent. */
export default function ChatSettings({ agent }: { agent: GraphChat }) {
  const toast = useToast();
  const patch = usePatchAgent();
  const models = useModels();

  const [modelKey, setModelKey] = useState(
    `${agent.model.provider ?? ""}/${agent.model.model ?? ""}`,
  );
  const [budget, setBudget] = useState(JSON.stringify(agent.budget ?? {}, null, 2));
  const [canSpawn, setCanSpawn] = useState(agent.capabilities.canSpawn);
  const [canMessage, setCanMessage] = useState(agent.capabilities.canMessage);
  const [autoWake, setAutoWake] = useState(agent.autoWake);
  const [approval, setApproval] = useState<ApprovalMode>(agent.approval);
  const [budgetError, setBudgetError] = useState<string | null>(null);

  const currentKey = `${agent.model.provider ?? ""}/${agent.model.model ?? ""}`;
  const options = models.data ?? [];

  const submit = (event: FormEvent) => {
    event.preventDefault();
    const body: PatchAgentInput = {};
    if (modelKey !== currentKey) {
      const [provider, ...rest] = modelKey.split("/");
      body.model = { ...agent.model, provider, model: rest.join("/") };
    }
    if (canSpawn !== agent.capabilities.canSpawn || canMessage !== agent.capabilities.canMessage) {
      body.capabilities = { canSpawn, canMessage };
    }
    if (autoWake !== agent.autoWake) body.autoWake = autoWake;
    if (approval !== agent.approval) body.approval = approval;
    try {
      const parsed = JSON.parse(budget || "{}");
      if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
        throw new Error("budget must be a JSON object");
      }
      setBudgetError(null);
      if (JSON.stringify(parsed) !== JSON.stringify(agent.budget ?? {})) body.budget = parsed;
    } catch (error) {
      setBudgetError(messageOf(error));
      return;
    }
    if (Object.keys(body).length === 0) {
      toast.show("Nothing changed");
      return;
    }
    patch.mutate(
      { id: agent.id, patch: body },
      {
        onSuccess: () => toast.show("Saved"),
        onError: (error) => toast.show(messageOf(error)),
      },
    );
  };

  return (
    <form onSubmit={submit} aria-labelledby="settings-heading" className="space-y-3 px-4 py-3">
      <h3 id="settings-heading" className="text-sm font-semibold">
        Settings
      </h3>
      <div className="text-xs text-muted">
        Model
        <ModelPicker
          models={options}
          value={modelKey}
          onChange={setModelKey}
          ariaLabel="Model"
          className={`${fieldClass()} flex min-h-[44px] items-center justify-between gap-2 text-left text-sm text-text md:min-h-0`}
        />
        <NoToolsWarning models={options} value={modelKey} />
      </div>
      <label className="block text-xs text-muted">
        Budget (JSON)
        <textarea
          className={`${fieldClass()} font-mono text-xs`}
          rows={3}
          value={budget}
          onChange={(e) => setBudget(e.target.value)}
        />
        {budgetError && <span className="text-danger">{budgetError}</span>}
      </label>
      <fieldset className="space-y-1 text-sm">
        <legend className="text-xs text-muted">Capabilities</legend>
        <label className="flex min-h-[44px] items-center gap-2 sm:min-h-0">
          <input type="checkbox" checked={canSpawn} onChange={(e) => setCanSpawn(e.target.checked)} />
          can create sub-chats
        </label>
        <label className="flex min-h-[44px] items-center gap-2 sm:min-h-0">
          <input type="checkbox" checked={canMessage} onChange={(e) => setCanMessage(e.target.checked)} />
          can message other chats
        </label>
        <label className="flex min-h-[44px] items-center gap-2 sm:min-h-0">
          <input type="checkbox" checked={autoWake} onChange={(e) => setAutoWake(e.target.checked)} />
          auto-wake on incoming messages
        </label>
      </fieldset>
      <label className="block text-xs text-muted">
        Approval mode
        <select
          className={fieldClass()}
          value={approval}
          onChange={(e) => setApproval(e.target.value as ApprovalMode)}
        >
          <option value="never">never</option>
          <option value="destructive">destructive</option>
          <option value="always">always</option>
        </select>
      </label>
      <button type="submit" className={buttonClass("primary")} disabled={patch.isPending}>
        Save settings
      </button>
    </form>
  );
}
