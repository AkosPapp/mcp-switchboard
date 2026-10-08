import { useEffect, useState, type FormEvent } from "react";

import { useToast } from "../../components/Toast";
import { buttonClass, fieldClass } from "../graph/ui";
import { messageOf } from "./api";
import { usePersona, usePutPersona } from "./skills";

/** The persona prompt ($DATA_DIR/prompts/base.md): the identity that leads
 * every chat's system prompt, ahead of the followed profile's text. */
export default function PersonaPane({ onBack }: { onBack: () => void }) {
  const toast = useToast();
  const query = usePersona();
  const put = usePutPersona();
  const [text, setText] = useState("");
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (query.data !== undefined && text === "") setText(query.data);
  }, [query.data, text]);

  const submit = (event: FormEvent) => {
    event.preventDefault();
    setError(null);
    put.mutate(text, {
      onSuccess: () => toast.show("Persona saved — every chat's next turn uses it"),
      onError: (e) => setError(messageOf(e)),
    });
  };

  return (
    <div className="mx-auto max-w-2xl">
      <div className="flex flex-wrap items-center gap-2 border-b border-border p-2">
        <button type="button" className={`${buttonClass()} min-h-[44px] md:hidden`} onClick={onBack}>
          ← Back
        </button>
        <h2 className="min-w-0 flex-1 px-1 text-sm font-semibold">Persona prompt</h2>
      </div>
      <form onSubmit={submit} noValidate aria-label="Persona" className="space-y-4 p-4">
        <p className="text-xs text-muted">
          Stored at <span className="font-mono">prompts/base.md</span> in the hub&apos;s data directory. It is the first
          thing every model turn sees — who the agent is and how it works. A chat&apos;s followed prompt adds to it, it
          does not replace it.
        </p>
        {query.isPending ? (
          <p className="text-xs text-muted">Loading…</p>
        ) : (
          <textarea
            className={`${fieldClass()} font-mono text-xs`}
            rows={22}
            value={text}
            onChange={(e) => setText(e.target.value)}
            spellCheck={false}
          />
        )}
        {error && (
          <p role="alert" className="text-sm text-danger">
            {error}
          </p>
        )}
        <button type="submit" className={`${buttonClass("primary")} min-h-[44px] md:min-h-[36px]`} disabled={put.isPending || query.isPending}>
          Save persona
        </button>
      </form>
    </div>
  );
}
