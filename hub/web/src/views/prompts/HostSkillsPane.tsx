import { useToast } from "../../components/Toast";
import { Chip, buttonClass } from "../graph/ui";
import { messageOf } from "./api";
import { useHostSkills, useImportHostSkill, type HostSkill } from "./skills";

/** The SKILL.md skills clients scanned on their hosts, stored read-only under
 * skills/hosts/<label>/ in the hub's data directory. Importing copies one into
 * the managed library — the hub copy then owns the name. */
export default function HostSkillsPane({ onBack }: { onBack: () => void }) {
  const toast = useToast();
  const query = useHostSkills();
  const imp = useImportHostSkill();
  const rows = query.data ?? [];

  return (
    <div className="mx-auto max-w-2xl" data-testid="host-skills">
      <div className="flex flex-wrap items-center gap-2 border-b border-border p-2">
        <button type="button" className={`${buttonClass()} min-h-[44px] md:hidden`} onClick={onBack}>
          ← Back
        </button>
        <h2 className="min-w-0 flex-1 px-1 text-sm font-semibold">Host skills</h2>
        <button type="button" className={`${buttonClass()} min-h-[44px] md:min-h-[36px]`} onClick={() => query.refetch()}>
          Refresh
        </button>
      </div>
      <div className="space-y-2 p-4">
        <p className="text-xs text-muted">
          Scanned from every connected client&apos;s <span className="font-mono">~/.claude/skills</span>,{" "}
          <span className="font-mono">.opencode/skills</span> and friends, kept current automatically. Files land under{" "}
          <span className="font-mono">skills/hosts/…</span> in the hub&apos;s data directory, outside the model&apos;s
          reach until imported.
        </p>
        {query.isPending && <p className="text-sm text-muted">Loading…</p>}
        {query.error && <p className="text-sm text-danger">{messageOf(query.error)}</p>}
        {!query.isPending && rows.length === 0 && (
          <p className="text-sm text-muted">No host has reported a SKILL.md skill yet.</p>
        )}
        {rows.map((s: HostSkill) => (
          <div key={`${s.host}/${s.name}`} className="flex items-start gap-2 border border-border p-2 text-sm">
            <div className="min-w-0 flex-1">
              <div className="flex flex-wrap items-center gap-2">
                <span className="truncate font-mono text-sm font-medium">{s.name}</span>
                <Chip title="where the client found it">{s.host}</Chip>
                {s.shadowed && (
                  <span className="rounded border border-border px-1.5 py-0.5 text-[11px] leading-none text-muted" title="a managed skill of this name wins">
                    shadowed
                  </span>
                )}
              </div>
              {s.description && <p className="truncate text-xs text-muted">{s.description}</p>}
              <p className="truncate font-mono text-[11px] text-muted">{s.path}</p>
            </div>
            {!s.shadowed && (
              <button
                type="button"
                className={`${buttonClass("primary")} min-h-[44px] md:min-h-[36px]`}
                disabled={imp.isPending}
                onClick={() =>
                  imp.mutate(
                    { host: s.host, name: s.name },
                    {
                      onSuccess: (k) => {
                        toast.show(`Imported /${k.name} into the hub library`);
                        query.refetch();
                      },
                      onError: (e) => toast.show(messageOf(e)),
                    }
                  )
                }
              >
                Import
              </button>
            )}
          </div>
        ))}
      </div>
    </div>
  );
}
