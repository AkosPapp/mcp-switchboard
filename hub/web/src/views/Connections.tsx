import { useMemo } from "react";
import { useSearchParams } from "react-router-dom";

import { useConnections } from "../api/queries";
import type { ConnectionInfo, ServerInfo, ToolInfo } from "../api/types";
import ClientBadge, { environmentSearchText } from "../components/ClientBadge";
import { ServerStateDot } from "../components/Status";
import { useOrchestratorEnabled } from "../hooks/useOrchestratorEnabled";
import { useStoredState } from "../hooks/useStoredState";
import { isString } from "../lib/uiMemory";
import HubTools from "./HubTools";
import ToolPanel from "./ToolPanel";

/**
 * Which tool the right-hand pane is showing.
 *
 * Held as the three names rather than as an object reference, because the
 * registry snapshot is refetched whenever anything changes and object identity
 * does not survive that. The names do, and re-resolving them each render is how
 * the pane notices that the tool it is showing has gone away.
 */
export interface Selection {
  connectionId: string;
  server: string;
  tool: string;
}

function matches(needle: string, ...haystack: (string | null)[]): boolean {
  if (needle === "") return true;
  const lowered = needle.toLowerCase();
  return haystack.some((value) => value !== null && value.toLowerCase().includes(lowered));
}

interface Filtered {
  connection: ConnectionInfo;
  servers: { server: ServerInfo; tools: ToolInfo[] }[];
}

/**
 * Apply the filter box.
 *
 * A machine or server whose own name matches keeps all of its children, so
 * typing a machine name shows you that machine rather than nothing; otherwise
 * only the matching tools survive, and a server with no surviving tools is
 * dropped.
 */
function filterTree(connections: ConnectionInfo[], needle: string): Filtered[] {
  if (needle === "") {
    return connections.map((connection) => ({
      connection,
      servers: connection.servers.map((server) => ({ server, tools: server.tools })),
    }));
  }

  const out: Filtered[] = [];
  for (const connection of connections) {
    const machineMatches = matches(
      needle,
      connection.label,
      connection.client.name,
      ...environmentSearchText(connection.client.environment),
    );
    const servers: Filtered["servers"] = [];

    for (const server of connection.servers) {
      const serverMatches = machineMatches || matches(needle, server.name, server.project);
      const tools = serverMatches
        ? server.tools
        : server.tools.filter((tool) =>
            matches(needle, tool.name, tool.exposedName, tool.description),
          );
      if (serverMatches || tools.length > 0) servers.push({ server, tools });
    }

    if (servers.length > 0) out.push({ connection, servers });
  }
  return out;
}

function ToolRow({
  tool,
  selected,
  onSelect,
}: {
  tool: ToolInfo;
  selected: boolean;
  onSelect: () => void;
}) {
  return (
    <button
      type="button"
      role="treeitem"
      aria-selected={selected}
      onClick={onSelect}
      className={[
        "flex w-full items-center gap-2 rounded px-2 py-1.5 text-left text-sm transition-colors",
        selected ? "bg-accent/15 text-text" : "text-muted hover:bg-raised hover:text-text",
      ].join(" ")}
    >
      <span className="truncate font-mono text-xs">{tool.name}</span>
      {tool.exposedName === null && (
        // I4 dropped it from the catalog: it exists upstream but nobody can
        // call it, which is worth saying here rather than only on the panel.
        <span className="ml-auto shrink-0 text-[10px] uppercase text-warn" title="not exposed">
          hidden
        </span>
      )}
    </button>
  );
}

function ServerGroup({
  connection,
  server,
  tools,
  selection,
  onSelect,
}: {
  connection: ConnectionInfo;
  server: ServerInfo;
  tools: ToolInfo[];
  selection: Selection | null;
  onSelect: (selection: Selection) => void;
}) {
  return (
    <div className="mt-1">
      <div className="flex items-center gap-2 px-2 py-1">
        <ServerStateDot state={server.state} />
        <span className="truncate text-sm font-medium">{server.name}</span>
        {server.project !== null && (
          <span className="rounded bg-raised px-1.5 py-0.5 text-[10px] text-muted">
            {server.project}
          </span>
        )}
        <span className="ml-auto text-xs text-muted">{server.toolCount}</span>
      </div>

      {server.error !== null && (
        <p className="px-2 pb-1 text-xs text-danger">{server.error}</p>
      )}

      <div className="pl-3">
        {tools.map((tool) => (
          <ToolRow
            key={tool.name}
            tool={tool}
            selected={
              selection?.connectionId === connection.id &&
              selection.server === server.name &&
              selection.tool === tool.name
            }
            onSelect={() =>
              onSelect({ connectionId: connection.id, server: server.name, tool: tool.name })
            }
          />
        ))}
        {tools.length === 0 && (
          <p className="px-2 py-1 text-xs text-muted">no tools</p>
        )}
      </div>
    </div>
  );
}

export default function Connections() {
  const { data, isLoading, error, refetch, isFetching } = useConnections();
  const orchestrator = useOrchestratorEnabled();
  // The selection is in the URL (and so in the tab's remembered location); the
  // filter text is UI-only, so the browser remembers it.
  const [filter, setFilter] = useStoredState("connections.filter", "", isString);
  const [params, setParams] = useSearchParams();

  // The selection lives in the URL so a tool is linkable and survives a reload.
  const selection = useMemo<Selection | null>(() => {
    const connectionId = params.get("connection");
    const server = params.get("server");
    const tool = params.get("tool");
    if (connectionId === null || server === null || tool === null) return null;
    return { connectionId, server, tool };
  }, [params]);

  // Arguments arrive from the Calls view when a recorded call is reopened here
  // (see its detail pane), so a call can be re-run with a tweak.
  const prefill = useMemo<Record<string, unknown> | null>(() => {
    const raw = params.get("args");
    if (raw === null) return null;
    try {
      const parsed = JSON.parse(raw);
      return parsed !== null && typeof parsed === "object" && !Array.isArray(parsed)
        ? (parsed as Record<string, unknown>)
        : null;
    } catch {
      return null;
    }
  }, [params]);

  const select = (next: Selection | null) => {
    const updated = new URLSearchParams(params);
    // A hand-picked tool starts from a clean form, never from the arguments of
    // whatever call was opened here before it.
    updated.delete("args");
    if (next === null) {
      updated.delete("connection");
      updated.delete("server");
      updated.delete("tool");
    } else {
      updated.set("connection", next.connectionId);
      updated.set("server", next.server);
      updated.set("tool", next.tool);
    }
    setParams(updated, { replace: true });
  };

  const connections = data?.connections ?? [];
  const tree = useMemo(() => filterTree(connections, filter.trim()), [connections, filter]);

  const totals = useMemo(() => {
    let servers = 0;
    let tools = 0;
    for (const connection of connections) {
      servers += connection.servers.length;
      for (const server of connection.servers) tools += server.toolCount;
    }
    return { machines: connections.length, servers, tools };
  }, [connections]);

  return (
    <div className="flex h-full min-h-0 flex-col md:flex-row">
      {/* The tree. On a phone it is the whole view until a tool is picked,
          which is what keeps this usable at 375px without a drawer. */}
      <aside
        className={[
          "flex min-h-0 flex-col border-border md:w-80 md:border-r lg:w-96",
          selection !== null ? "hidden md:flex" : "flex flex-1",
        ].join(" ")}
      >
        <div className="flex items-center gap-2 border-b border-border p-2">
          <input
            type="search"
            value={filter}
            onChange={(event) => setFilter(event.target.value)}
            placeholder="filter tools, servers, machines"
            spellCheck={false}
            autoComplete="off"
            className="min-w-0 flex-1 rounded border border-border bg-surface px-2 py-1.5 text-sm placeholder:text-muted"
          />
          <button
            type="button"
            onClick={() => refetch()}
            title="Refresh"
            aria-label="Refresh"
            className="rounded border border-border px-2 py-1.5 text-sm text-muted hover:bg-raised hover:text-text"
          >
            <span className={isFetching ? "inline-block animate-spin" : undefined}>↻</span>
          </button>
        </div>

        <div className="min-h-0 flex-1 overflow-auto p-1" role="tree" aria-label="Connections">
          {isLoading && <p className="p-3 text-sm text-muted">Loading…</p>}

          {error !== null && (
            <p className="p-3 text-sm text-danger">
              {error instanceof Error ? error.message : "could not reach the hub"}
            </p>
          )}

          {!isLoading && error === null && connections.length === 0 && (
            <div className="p-3 text-sm text-muted">
              <p className="font-medium text-text">Nothing has dialled in yet.</p>
              <p className="mt-1">
                Start a client with your hub URL and tunnel token; the Endpoints tab has the
                command.
              </p>
            </div>
          )}

          {tree.map(({ connection, servers }) => (
            <div key={connection.id} className="mb-2">
              <div className="flex flex-col gap-0.5 px-2 py-1">
                <div className="text-sm">
                  <ClientBadge
                    label={connection.label}
                    environment={connection.client.environment}
                  />
                </div>
                <span className="truncate font-mono text-[10px] text-muted">
                  {connection.client.name} {connection.client.version}
                  {connection.client.environment == null && (
                    <span className="font-sans"> · environment not reported</span>
                  )}
                </span>
              </div>
              {servers.map(({ server, tools }) => (
                <ServerGroup
                  key={server.name}
                  connection={connection}
                  server={server}
                  tools={tools}
                  selection={selection}
                  onSelect={select}
                />
              ))}
            </div>
          ))}
        </div>

        <HubTools enabled={orchestrator} />

        <div className="border-t border-border px-3 py-1.5 text-xs text-muted">
          {totals.machines} machine{totals.machines === 1 ? "" : "s"} · {totals.servers} server
          {totals.servers === 1 ? "" : "s"} · {totals.tools} tool{totals.tools === 1 ? "" : "s"}
        </div>
      </aside>

      <section
        className={[
          "min-h-0 flex-1 overflow-auto",
          selection === null ? "hidden md:block" : "block",
        ].join(" ")}
      >
        {selection === null ? (
          <div className="mx-auto max-w-prose p-6 text-sm text-muted">
            <h2 className="text-base font-semibold text-text">Pick a tool</h2>
            <p className="mt-2">
              Every machine that has dialled in is on the left, with the MCP servers it carries
              and the tools each one exposes. Select a tool to read its schema and call it by
              hand.
            </p>
          </div>
        ) : (
          <ToolPanel
            selection={selection}
            connections={connections}
            prefill={prefill}
            onClose={() => select(null)}
          />
        )}
      </section>
    </div>
  );
}
