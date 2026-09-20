import { useMemo, useState, type FormEvent } from "react";
import { Link, useSearchParams } from "react-router-dom";

import { useCall, useCalls, useStats } from "../api/queries";
import type { CallFilters } from "../api/client";
import type { CallRecord, Stats } from "../api/types";
import CopyButton from "../components/CopyButton";
import Json, { pretty } from "../components/Json";
import { CallStatusBadge, formatDateTime, formatDuration, formatTime } from "../components/Status";

const LIMITS = [50, 100, 250, 1000];
const DEFAULT_LIMIT = 100;

interface FilterState {
  label: string;
  server: string;
  tool: string;
  status: string;
  limit: number;
}

/**
 * Filters live in the query string, not in component state.
 *
 * The console this replaces kept them in the form and lost them on reload, so a
 * filtered log could not be linked or bookmarked - which is the first thing you
 * want when you are showing someone a call that went wrong.
 */
function readFilters(params: URLSearchParams): FilterState {
  const limit = Number(params.get("limit"));
  return {
    label: params.get("label") ?? "",
    server: params.get("server") ?? "",
    tool: params.get("tool") ?? "",
    status: params.get("status") ?? "",
    limit: LIMITS.includes(limit) ? limit : DEFAULT_LIMIT,
  };
}

function writeFilters(filters: FilterState): URLSearchParams {
  const params = new URLSearchParams();
  if (filters.label) params.set("label", filters.label);
  if (filters.server) params.set("server", filters.server);
  if (filters.tool) params.set("tool", filters.tool);
  if (filters.status) params.set("status", filters.status);
  if (filters.limit !== DEFAULT_LIMIT) params.set("limit", String(filters.limit));
  return params;
}

function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function statsLine(stats: Stats): string {
  const { total, ok, error } = stats.calls;
  return `${total} calls · ${ok} ok · ${error} error`;
}

const FIELD =
  "min-h-[44px] w-full rounded border border-border bg-surface px-2 py-1.5 text-sm " +
  "placeholder:text-muted focus:border-accent";

function Filters({
  value,
  onApply,
}: {
  value: FilterState;
  onApply: (next: FilterState) => void;
}) {
  // The form is uncontrolled-ish: typing does not refetch, submitting does.
  const [draft, setDraft] = useState(value);
  const stats = useStats();

  const patch = (part: Partial<FilterState>) => setDraft((current) => ({ ...current, ...part }));

  const submit = (event: FormEvent) => {
    event.preventDefault();
    onApply(draft);
  };

  const reset = () => {
    const cleared = { label: "", server: "", tool: "", status: "", limit: DEFAULT_LIMIT };
    setDraft(cleared);
    onApply(cleared);
  };

  return (
    <form
      onSubmit={submit}
      autoComplete="off"
      aria-label="Call filters"
      className="border-b border-border bg-surface px-3 py-3 sm:px-4"
    >
      <div className="grid grid-cols-2 gap-2 sm:grid-cols-3 lg:grid-cols-6">
        <label className="flex flex-col gap-1 text-xs text-muted">
          Machine
          <input
            className={FIELD}
            type="text"
            placeholder="any"
            spellCheck={false}
            value={draft.label}
            onChange={(event) => patch({ label: event.target.value })}
          />
        </label>
        <label className="flex flex-col gap-1 text-xs text-muted">
          Server
          <input
            className={FIELD}
            type="text"
            placeholder="any"
            spellCheck={false}
            value={draft.server}
            onChange={(event) => patch({ server: event.target.value })}
          />
        </label>
        <label className="flex flex-col gap-1 text-xs text-muted">
          Tool
          <input
            className={FIELD}
            type="text"
            placeholder="any"
            spellCheck={false}
            value={draft.tool}
            onChange={(event) => patch({ tool: event.target.value })}
          />
        </label>
        <label className="flex flex-col gap-1 text-xs text-muted">
          Status
          <select
            className={FIELD}
            value={draft.status}
            onChange={(event) => patch({ status: event.target.value })}
          >
            <option value="">any</option>
            <option value="ok">ok</option>
            <option value="error">error</option>
            <option value="denied">denied</option>
          </select>
        </label>
        <label className="flex flex-col gap-1 text-xs text-muted">
          Limit
          <select
            className={FIELD}
            value={String(draft.limit)}
            onChange={(event) => patch({ limit: Number(event.target.value) })}
          >
            {LIMITS.map((limit) => (
              <option key={limit} value={limit}>
                {limit}
              </option>
            ))}
          </select>
        </label>
        <div className="col-span-2 flex items-end gap-2 sm:col-span-1">
          <button
            type="submit"
            className="min-h-[44px] flex-1 rounded bg-accent px-3 py-1.5 text-sm font-medium text-surface transition-opacity hover:opacity-90"
          >
            Apply
          </button>
          <button
            type="button"
            onClick={reset}
            className="min-h-[44px] flex-1 rounded border border-border px-3 py-1.5 text-sm text-muted transition-colors hover:bg-raised hover:text-text"
          >
            Reset
          </button>
        </div>
      </div>

      <p className="mt-2 text-xs text-muted">
        {stats.data ? statsLine(stats.data) : stats.isError ? "" : "…"}
      </p>
    </form>
  );
}

function Row({
  record,
  selected,
  onSelect,
}: {
  record: CallRecord;
  selected: boolean;
  onSelect: () => void;
}) {
  return (
    <tr
      onClick={onSelect}
      onKeyDown={(event) => {
        if (event.key === "Enter" || event.key === " ") {
          event.preventDefault();
          onSelect();
        }
      }}
      tabIndex={0}
      aria-selected={selected}
      className={`cursor-pointer border-b border-border transition-colors hover:bg-raised ${
        selected ? "bg-raised" : ""
      }`}
    >
      <td className="whitespace-nowrap px-3 py-2 text-muted">{formatTime(record.startedAt)}</td>
      <td className="whitespace-nowrap px-3 py-2">{record.label}</td>
      <td className="whitespace-nowrap px-3 py-2">{record.server}</td>
      <td className="px-3 py-2" title={record.exposedName}>
        {record.tool}
      </td>
      <td className="whitespace-nowrap px-3 py-2">
        <CallStatusBadge status={record.status} />
      </td>
      <td className="whitespace-nowrap px-3 py-2 text-right tabular-nums text-muted">
        {formatDuration(record.durationMs)}
      </td>
      <td className="whitespace-nowrap px-3 py-2 text-muted">{record.source}</td>
    </tr>
  );
}

function Card({
  record,
  selected,
  onSelect,
}: {
  record: CallRecord;
  selected: boolean;
  onSelect: () => void;
}) {
  return (
    <li>
      <button
        type="button"
        onClick={onSelect}
        aria-selected={selected}
        className={`flex w-full min-h-[44px] flex-col gap-1 border-b border-border px-3 py-3 text-left ${
          selected ? "bg-raised" : ""
        }`}
      >
        <span className="flex items-baseline justify-between gap-2">
          <span className="truncate font-medium">{record.tool}</span>
          <CallStatusBadge status={record.status} />
        </span>
        <span className="flex flex-wrap gap-x-2 text-xs text-muted">
          <span>{formatTime(record.startedAt)}</span>
          <span aria-hidden="true">·</span>
          <span className="truncate">
            {record.label} › {record.server}
          </span>
          <span aria-hidden="true">·</span>
          <span className="tabular-nums">{formatDuration(record.durationMs)}</span>
          <span aria-hidden="true">·</span>
          <span>{record.source}</span>
        </span>
      </button>
    </li>
  );
}

function Detail({ record, onClose }: { record: CallRecord; onClose: () => void }) {
  // The list carries whole records, so the fetch is only about freshness; the
  // row is shown immediately and replaced when the hub answers.
  const fetched = useCall(record.id);
  const call = fetched.data ?? record;

  return (
    // One element, two shapes: a docked pane from md up, a full-screen
    // slide-over below it (spec.md U2). Rendering it twice would duplicate the
    // detail query and the copy buttons.
    <aside
      aria-label="Call detail"
      className="fixed inset-0 z-40 flex flex-col overflow-y-auto bg-surface md:static md:z-auto md:w-[26rem] md:shrink-0 md:border-l md:border-border lg:w-[32rem]"
    >
      <div className="flex items-start gap-2 border-b border-border px-3 py-3">
        <div className="min-w-0 flex-1">
          <h2 className="truncate text-base font-semibold">{call.tool}</h2>
          <p className="truncate text-xs text-muted">
            {call.label || call.connectionId} › {call.server}
          </p>
          <p className="mt-1 break-all font-mono text-xs text-muted">{call.exposedName}</p>
        </div>
        <button
          type="button"
          onClick={onClose}
          aria-label="Close call detail"
          className="min-h-[44px] min-w-[44px] rounded border border-border px-2 text-sm text-muted transition-colors hover:bg-raised hover:text-text"
        >
          ✕
        </button>
      </div>

      <div className="flex flex-col gap-3 px-3 py-3 text-sm">
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted">
          <CallStatusBadge status={call.status} />
          <span>{formatDateTime(call.startedAt)}</span>
          <span aria-hidden="true">·</span>
          <span>{formatDuration(call.durationMs)}</span>
          <span aria-hidden="true">·</span>
          <span>{call.source}</span>
        </div>

        <div>
          <div className="flex items-center justify-between gap-2">
            <h3 className="text-xs uppercase tracking-wide text-muted">Arguments</h3>
            <div className="flex items-center gap-1">
              {/* The console this replaces could push a recorded call back into
                  the tool pane, which is how you re-run one with a tweak. The
                  arguments ride along in the query string so the pane can open
                  prefilled without any shared state between the two views. */}
              <Link
                to={{
                  pathname: "/connections",
                  search: new URLSearchParams({
                    connection: call.connectionId,
                    server: call.server,
                    tool: call.tool,
                    args: JSON.stringify(call.arguments ?? {}),
                  }).toString(),
                }}
                className="rounded border border-border px-2 py-1 text-xs text-muted transition-colors hover:bg-raised hover:text-text"
              >
                Open in Connections
              </Link>
              <CopyButton value={pretty(call.arguments)} />
            </div>
          </div>
          <Json value={call.arguments} className="mt-1 max-h-72" />
        </div>

        {call.error !== null && (
          <div>
            <div className="flex items-center justify-between gap-2">
              <h3 className="text-xs uppercase tracking-wide text-muted">Error</h3>
              <CopyButton value={call.error} />
            </div>
            <p className="mt-1 whitespace-pre-wrap rounded border border-border bg-raised p-2 font-mono text-xs text-danger">
              {call.error}
            </p>
          </div>
        )}

        {call.result !== null && (
          <div>
            <div className="flex items-center justify-between gap-2">
              <h3 className="text-xs uppercase tracking-wide text-muted">Result</h3>
              <CopyButton value={pretty(call.result)} />
            </div>
            <Json value={call.result} className="mt-1 max-h-96" />
          </div>
        )}

        <dl className="grid grid-cols-[auto,1fr] gap-x-3 gap-y-1 border-t border-border pt-3 text-xs text-muted">
          <dt>call id</dt>
          <dd className="break-all font-mono">{call.id}</dd>
          <dt>connection</dt>
          <dd className="break-all font-mono">{call.connectionId}</dd>
          {/* Orchestrator ids are optional on the wire and absent on a plain
              MCP call, so they appear only when the hub sent them. */}
          {call.agentId ? (
            <>
              <dt>agent</dt>
              <dd className="break-all font-mono">{call.agentId}</dd>
            </>
          ) : null}
          {call.chatId ? (
            <>
              <dt>chat</dt>
              <dd className="break-all font-mono">{call.chatId}</dd>
            </>
          ) : null}
          {call.runId ? (
            <>
              <dt>run</dt>
              <dd className="break-all font-mono">{call.runId}</dd>
            </>
          ) : null}
        </dl>
      </div>
    </aside>
  );
}

export default function Calls() {
  const [params, setParams] = useSearchParams();
  const [selectedId, setSelectedId] = useState<string | null>(null);

  const filters = useMemo(() => readFilters(params), [params]);
  const query: CallFilters = useMemo(
    () => ({
      label: filters.label || undefined,
      server: filters.server || undefined,
      tool: filters.tool || undefined,
      status: filters.status || undefined,
      limit: filters.limit,
    }),
    [filters],
  );

  const calls = useCalls(query);
  const records = calls.data?.calls ?? [];
  // A filter change can drop the selected call; falling back to nothing is
  // better than pinning a detail pane to a row that is no longer listed.
  const selected = records.find((record) => record.id === selectedId) ?? null;

  const apply = (next: FilterState) => {
    setParams(writeFilters(next));
    setSelectedId(null);
  };

  return (
    <div className="flex h-full min-h-0 flex-col">
      {/* Remounting on a filter change resets the draft, so Reset and a
          back-button navigation both show what the URL actually asks for. */}
      <Filters key={params.toString()} value={filters} onApply={apply} />

      <div className="flex min-h-0 flex-1">
        <div className="min-w-0 flex-1 overflow-y-auto">
          {calls.isError ? (
            <p className="px-3 py-6 text-sm text-danger">
              Could not load calls: {messageOf(calls.error)}
            </p>
          ) : calls.isPending ? (
            <p className="px-3 py-6 text-sm text-muted">Loading calls…</p>
          ) : records.length === 0 ? (
            <p className="px-3 py-6 text-sm text-muted">No calls recorded yet.</p>
          ) : (
            <>
              <table className="hidden w-full border-collapse text-sm md:table">
                <thead className="sticky top-0 bg-bg text-left text-xs uppercase tracking-wide text-muted">
                  <tr className="border-b border-border">
                    <th className="px-3 py-2 font-medium">Time</th>
                    <th className="px-3 py-2 font-medium">Machine</th>
                    <th className="px-3 py-2 font-medium">Server</th>
                    <th className="px-3 py-2 font-medium">Tool</th>
                    <th className="px-3 py-2 font-medium">Status</th>
                    <th className="px-3 py-2 text-right font-medium">Duration</th>
                    <th className="px-3 py-2 font-medium">Source</th>
                  </tr>
                </thead>
                <tbody>
                  {records.map((record) => (
                    <Row
                      key={record.id}
                      record={record}
                      selected={record.id === selectedId}
                      onSelect={() => setSelectedId(record.id)}
                    />
                  ))}
                </tbody>
              </table>

              <ul className="md:hidden" aria-label="Calls">
                {records.map((record) => (
                  <Card
                    key={record.id}
                    record={record}
                    selected={record.id === selectedId}
                    onSelect={() => setSelectedId(record.id)}
                  />
                ))}
              </ul>
            </>
          )}
        </div>

        {selected !== null && (
          <Detail record={selected} onClose={() => setSelectedId(null)} />
        )}
      </div>
    </div>
  );
}
