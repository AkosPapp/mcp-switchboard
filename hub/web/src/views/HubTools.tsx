import { useState } from "react";
import { useQuery } from "@tanstack/react-query";

import { request } from "../api/client";
import Json from "../components/Json";
import { Chip } from "./graph/ui";

export interface HubTool {
  name: string;
  description: string;
  inputSchema: object;
  annotations: {
    readOnlyHint?: boolean;
    destructiveHint?: boolean;
    idempotentHint?: boolean;
    openWorldHint?: boolean;
  };
  requires: "canSpawn" | "canMessage" | "canSpawn or canMessage" | "always";
}

export function useHubTools(enabled: boolean) {
  return useQuery({
    queryKey: ["hub-tools"],
    queryFn: async () => (await request<{ tools: HubTool[] }>("hub-tools")).tools,
    enabled,
    staleTime: 60_000,
  });
}

const ANNOTATIONS: [keyof HubTool["annotations"], string][] = [
  ["readOnlyHint", "read-only"],
  ["destructiveHint", "destructive"],
  ["openWorldHint", "open-world"],
  ["idempotentHint", "idempotent"],
];

function HubToolRow({ tool }: { tool: HubTool }) {
  const [open, setOpen] = useState(false);
  return (
    <li className="border-b border-border px-3 py-2 last:border-b-0">
      <div className="break-all font-mono text-xs font-medium">{tool.name}</div>
      <p className="mt-0.5 text-xs text-muted">{tool.description}</p>
      <div className="mt-1 flex flex-wrap gap-1">
        {ANNOTATIONS.filter(([key]) => tool.annotations?.[key]).map(([, label]) => (
          <Chip key={label}>{label}</Chip>
        ))}
        <Chip title="capability a profile needs for this tool">requires: {tool.requires}</Chip>
      </div>
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setOpen(!open)}
        className="mt-1 min-h-[44px] text-xs text-accent md:min-h-0"
      >
        {open ? "Hide input schema" : "Show input schema"}
      </button>
      {open && <Json value={tool.inputSchema} className="max-h-72" />}
    </li>
  );
}

/** The switchboard.* tools the hub itself provides. Absent when the
 * orchestrator is off (the endpoint 404s). */
export default function HubTools({ enabled }: { enabled: boolean | undefined }) {
  const [open, setOpen] = useState(false);
  const query = useHubTools(enabled === true && open);
  if (!enabled) return null;
  const tools = query.data ?? [];
  return (
    <section className="border-t border-border" aria-label="Hub tools">
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setOpen(!open)}
        className="flex min-h-[44px] w-full items-center gap-2 px-3 text-left text-sm font-semibold"
      >
        <span aria-hidden="true">{open ? "▾" : "▸"}</span>
        Hub tools
        {query.data && <span className="text-xs font-normal text-muted">{tools.length}</span>}
      </button>
      {open && (
        <div className="max-h-[50vh] overflow-auto">
          {query.isLoading && <p className="px-3 pb-2 text-sm text-muted">Loading…</p>}
          {query.error && (
            <p className="px-3 pb-2 text-sm text-danger">
              {query.error instanceof Error ? query.error.message : "could not load"}
            </p>
          )}
          <ul>
            {tools.map((tool) => (
              <HubToolRow key={tool.name} tool={tool} />
            ))}
          </ul>
        </div>
      )}
    </section>
  );
}
