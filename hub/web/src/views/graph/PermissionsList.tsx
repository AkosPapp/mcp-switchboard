import { useState } from "react";

import type { GraphChat, GraphChatView } from "./chatNodes";
import { useToast } from "../../components/Toast";
import { describeRef, permissionRows, toInput, type PermissionRow } from "../../lib/grants";
import { messageOf, useSetGrants } from "./hooks";
import { buttonClass, Chip, SourceBadge } from "./ui";

/**
 * U16: every ServerRef the chat may use, joined with the live registry.
 * A toggle writes a human grant (A13); one that would exceed the parent's set
 * asks first (A12).
 */
export default function PermissionsList({
  agent,
  graph,
}: {
  agent: GraphChat;
  graph: GraphChatView;
}) {
  const toast = useToast();
  const setGrants = useSetGrants();
  const [confirming, setConfirming] = useState<PermissionRow | null>(null);

  const parent = agent.parentId ? graph.agents.find((a) => a.id === agent.parentId) : undefined;
  const parentGrants = parent ? graph.grants.filter((g) => g.agentId === parent.id) : null;
  const own = graph.grants.filter((g) => g.agentId === agent.id);
  const rows = permissionRows(own, graph.servers, parentGrants);

  const apply = (row: PermissionRow, allowed: boolean) => {
    setConfirming(null);
    setGrants.mutate(
      { agentId: agent.id, grants: [toInput(row, allowed)] },
      { onError: (error) => toast.show(messageOf(error)) },
    );
  };

  const toggle = (row: PermissionRow) => {
    const next = !row.allowed;
    if (next && row.widens) {
      setConfirming(row);
      return;
    }
    apply(row, next);
  };

  return (
    <section aria-labelledby="perm-heading" className="px-4 py-3">
      <div className="flex items-center gap-2">
        <h3 id="perm-heading" className="text-sm font-semibold">
          Permissions
        </h3>
        <span className="ml-auto flex items-center gap-1 text-xs text-muted">
          approval
          <Chip title="Approval mode is set when the chat is created and never re-derived (W1)">
            {agent.approval}
          </Chip>
        </span>
      </div>

      {rows.length === 0 ? (
        <p className="mt-2 text-xs text-muted">No servers are connected and nothing is granted.</p>
      ) : (
        <ul className="mt-2 divide-y divide-border rounded border border-border">
          {rows.map((row) => (
            <li
              key={row.key}
              data-testid={`perm-${row.key}`}
              className={`flex items-start gap-2 px-2 py-2 ${row.connected ? "" : "opacity-60"}`}
            >
              {/*
                The pill itself stays small (h-6 w-11); the button around it grows
                to the 44px tap target the rest of the console uses on phones
                (grep "min-h-[44px]", e.g. Settings.tsx), so it is tappable
                without rendering as a tiny, near-invisible circle.
              */}
              <button
                type="button"
                role="switch"
                aria-checked={row.allowed}
                aria-label={`Allow ${describeRef(row)}`}
                disabled={setGrants.isPending}
                onClick={() => toggle(row)}
                className="flex min-h-[44px] min-w-[44px] shrink-0 items-center justify-center disabled:opacity-50"
              >
                <span
                  aria-hidden="true"
                  className={`relative h-6 w-11 rounded-full border border-border transition-colors ${
                    row.allowed ? "bg-accent" : "bg-raised"
                  }`}
                >
                  <span
                    className={`absolute top-0.5 h-[18px] w-[18px] rounded-full bg-surface shadow transition-all ${
                      row.allowed ? "left-5" : "left-0.5"
                    }`}
                  />
                </span>
              </button>
              <div className="min-w-0 flex-1">
                <div className="break-all font-mono text-xs">{describeRef(row)}</div>
                <div className="mt-1 flex flex-wrap items-center gap-1">
                  {row.source !== null && <SourceBadge source={row.source} />}
                  {row.viaWildcard && <Chip title="Decided by a wildcard grant">via wildcard</Chip>}
                  {!row.connected && <Chip title="Held, not deleted (I3)">not connected</Chip>}
                  {row.orphaned && (
                    <span
                      className="rounded bg-warn/15 px-1.5 py-0.5 text-xs text-warn"
                      title="A human grant that its parent no longer holds (A14)"
                    >
                      orphaned by policy
                    </span>
                  )}
                  {row.widens && !row.allowed && (
                    <Chip title="The parent does not hold this">parent lacks</Chip>
                  )}
                </div>
              </div>
            </li>
          ))}
        </ul>
      )}

      {confirming !== null && (
        <div
          role="alertdialog"
          aria-label="Widen permissions"
          className="mt-2 rounded border border-warn bg-warn/10 p-3 text-sm"
        >
          <p>
            <span className="font-mono text-xs">{describeRef(confirming)}</span> is not held by
            the parent{parent ? <> ({parent.name})</> : null}. Allowing it stores a{" "}
            <strong>human</strong> grant that goes beyond the parent's set (A12/A13).
          </p>
          <div className="mt-2 flex gap-2">
            <button type="button" className={buttonClass("primary")} onClick={() => apply(confirming, true)}>
              Allow anyway
            </button>
            <button type="button" className={buttonClass()} onClick={() => setConfirming(null)}>
              Cancel
            </button>
          </div>
        </div>
      )}
    </section>
  );
}
