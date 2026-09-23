import { useEffect, useMemo, useState } from "react";

import { useCallTool, useRestartServer } from "../api/queries";
import type { CallRecord, ConnectionInfo, ServerInfo, ToolInfo } from "../api/types";
import ClientBadge from "../components/ClientBadge";
import CopyButton from "../components/CopyButton";
import Json, { pretty } from "../components/Json";
import { CallStatusBadge, formatDuration } from "../components/Status";
import { useToast } from "../components/Toast";
import { collect, fieldsOf, seed, unknownKeys, type Field } from "../lib/schema";
import type { Selection } from "./Connections";

type Mode = "form" | "json";

interface Resolved {
  connection: ConnectionInfo | null;
  server: ServerInfo | null;
  tool: ToolInfo | null;
}

/** Re-resolve the selection against the current snapshot every render: a
 * machine that disconnected or a server that came back with a different tool
 * list has to be noticed, not cached. */
function resolve(connections: ConnectionInfo[], selection: Selection): Resolved {
  const connection = connections.find((c) => c.id === selection.connectionId) ?? null;
  const server = connection?.servers.find((s) => s.name === selection.server) ?? null;
  const tool = server?.tools.find((t) => t.name === selection.tool) ?? null;
  return { connection, server, tool };
}

function FormField({
  field,
  value,
  onChange,
}: {
  field: Field;
  value: string;
  onChange: (value: string) => void;
}) {
  const id = `arg-${field.name}`;
  const label = (
    <label htmlFor={id} className="flex items-baseline gap-2 text-sm">
      <span className="font-mono">{field.name}</span>
      {field.required && <span className="text-xs text-danger">required</span>}
      <span className="text-xs text-muted">{field.kind}</span>
    </label>
  );

  const control =
    field.kind === "boolean" ? (
      <select
        id={id}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        className="w-full rounded border border-border bg-surface px-2 py-1.5 text-sm"
      >
        <option value="">unset</option>
        <option value="true">true</option>
        <option value="false">false</option>
      </select>
    ) : field.kind === "enum" ? (
      <select
        id={id}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        className="w-full rounded border border-border bg-surface px-2 py-1.5 text-sm"
      >
        <option value="">unset</option>
        {field.options?.map((option) => (
          <option key={option} value={option}>
            {option}
          </option>
        ))}
      </select>
    ) : (
      <input
        id={id}
        type={field.kind === "string" ? "text" : "number"}
        value={value}
        spellCheck={false}
        onChange={(event) => onChange(event.target.value)}
        className="w-full rounded border border-border bg-surface px-2 py-1.5 text-sm font-mono"
      />
    );

  return (
    <div className="mb-3">
      {label}
      {field.description !== undefined && (
        <p className="mb-1 text-xs text-muted">{field.description}</p>
      )}
      <div className="mt-1">{control}</div>
    </div>
  );
}

export default function ToolPanel({
  selection,
  connections,
  prefill = null,
  onClose,
}: {
  selection: Selection;
  connections: ConnectionInfo[];
  /** Arguments to start from, when a recorded call was reopened here. */
  prefill?: Record<string, unknown> | null;
  onClose: () => void;
}) {
  const toast = useToast();
  const callTool = useCallTool();
  const restart = useRestartServer();

  const { connection, server, tool } = resolve(connections, selection);
  const fields = useMemo(() => fieldsOf(tool?.inputSchema), [tool]);
  const canForm = fields.length > 0;

  const [mode, setMode] = useState<Mode>(canForm ? "form" : "json");
  const [values, setValues] = useState<Record<string, string>>({});
  const [json, setJson] = useState("{}");
  const [jsonError, setJsonError] = useState("");
  const [result, setResult] = useState<CallRecord | null>(null);

  // A different tool is a different form: reset rather than carry one tool's
  // arguments into another's schema.
  const key = `${selection.connectionId}/${selection.server}/${selection.tool}`;
  useEffect(() => {
    const schemaFields = fieldsOf(tool?.inputSchema);
    const args = prefill ?? {};
    setValues(seed(schemaFields, args));
    setJson(prefill === null ? "{}" : pretty(args));
    setJsonError("");
    setResult(null);

    // A prefill that the form cannot represent in full opens in JSON mode, so
    // nothing the caller passed is silently dropped on the way in.
    const representable =
      schemaFields.length > 0 && unknownKeys(schemaFields, args).length === 0;
    setMode(representable ? "form" : "json");
    // prefill is derived from the URL, so it changes exactly when the selection
    // does; JSON.stringify keeps a fresh object from re-running this.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, tool, JSON.stringify(prefill)]);

  const toForm = () => {
    // Keep whatever was typed in JSON, dropping only the keys a form cannot
    // represent - and saying which, rather than losing them silently.
    let parsed: Record<string, unknown>;
    try {
      parsed = JSON.parse(json || "{}");
    } catch (error) {
      setJsonError(`invalid JSON: ${(error as Error).message}`);
      return;
    }
    if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
      setJsonError("arguments must be a JSON object");
      return;
    }
    const dropped = unknownKeys(fields, parsed);
    setValues(seed(fields, parsed));
    if (dropped.length > 0) toast.show(`dropped keys not in the schema: ${dropped.join(", ")}`);
    setJsonError("");
    setMode("form");
  };

  const toJson = () => {
    const collected = collect(fields, values);
    setJson(pretty(collected.args));
    setJsonError("");
    setMode("json");
  };

  const run = () => {
    let args: Record<string, unknown>;

    if (mode === "json") {
      try {
        args = JSON.parse(json || "{}");
      } catch (error) {
        setJsonError(`invalid JSON: ${(error as Error).message}`);
        return;
      }
      if (args === null || typeof args !== "object" || Array.isArray(args)) {
        setJsonError("arguments must be a JSON object");
        return;
      }
    } else {
      const collected = collect(fields, values);
      if (collected.errors.length > 0) {
        toast.show(collected.errors[0]);
        return;
      }
      args = collected.args;
    }

    setResult(null);
    callTool.mutate(
      {
        connectionId: selection.connectionId,
        server: selection.server,
        tool: selection.tool,
        args,
      },
      {
        // A tool that fails is still a call that happened (N1), so the record
        // is rendered either way; only a refused request lands in onError.
        onSuccess: (record) => setResult(record),
        onError: (error) => toast.show((error as Error).message),
      },
    );
  };

  return (
    <div className="mx-auto max-w-3xl p-3 sm:p-4">
      <div className="mb-3 flex items-start gap-2">
        <button
          type="button"
          onClick={onClose}
          className="rounded border border-border px-2 py-1 text-sm text-muted hover:bg-raised hover:text-text md:hidden"
        >
          ← Back
        </button>
        <div className="min-w-0">
          <h2 className="truncate text-lg font-semibold">{selection.tool}</h2>
          <p className="truncate text-xs text-muted">
            {connection?.label ?? selection.connectionId}
            {server?.project != null ? ` › ${server.project}` : ""} › {selection.server}
          </p>
          {connection !== null && (
            <div className="mt-1 text-xs" data-testid="connection-env">
              <ClientBadge
                label={connection.label}
                environment={connection.client.environment}
              />
              {connection.client.environment?.workspace ? (
                <p
                  className="truncate font-mono text-[11px] text-muted"
                  title={connection.client.environment.workspace}
                >
                  {connection.client.environment.workspace}
                </p>
              ) : connection.client.environment == null ? (
                <p className="text-[11px] text-muted">environment not reported</p>
              ) : null}
            </div>
          )}
        </div>
        {server !== null && connection !== null && (
          <button
            type="button"
            onClick={() =>
              restart.mutate(
                { connectionId: connection.id, server: server.name },
                {
                  onSuccess: () => toast.show(`restarting ${server.name}`),
                  onError: (error) => toast.show((error as Error).message),
                },
              )
            }
            className="ml-auto shrink-0 rounded border border-border px-2 py-1 text-xs text-muted hover:bg-raised hover:text-text"
          >
            Restart server
          </button>
        )}
      </div>

      {tool?.exposedName != null && (
        <div className="mb-3 flex items-center gap-2">
          <code className="min-w-0 flex-1 truncate rounded bg-raised px-2 py-1 font-mono text-xs">
            {tool.exposedName}
          </code>
          <CopyButton value={tool.exposedName} />
        </div>
      )}

      {tool?.description != null && (
        <p className="mb-3 text-sm text-muted">{tool.description}</p>
      )}

      {tool === null && (
        <p className="mb-3 rounded border border-warn/40 bg-warn/10 p-2 text-sm">
          This tool is no longer exposed by the hub — the machine may have disconnected, or the
          server restarted with a different tool list.
        </p>
      )}

      {server !== null && server.state !== "running" && (
        <p className="mb-3 rounded border border-danger/40 bg-danger/10 p-2 text-sm">
          server state: {server.state}
          {server.error != null ? ` — ${server.error}` : ""}
        </p>
      )}

      <div className="mb-2 flex items-center gap-2">
        <h3 className="text-sm font-semibold">Arguments</h3>
        <div className="ml-auto flex gap-1">
          <button
            type="button"
            disabled={!canForm}
            onClick={toForm}
            title={canForm ? undefined : "This tool's schema has no simple properties to build a form from."}
            className={[
              "rounded border px-2 py-1 text-xs",
              mode === "form"
                ? "border-accent bg-accent/15 text-text"
                : "border-border text-muted hover:bg-raised",
              canForm ? "" : "cursor-not-allowed opacity-50",
            ].join(" ")}
          >
            Form
          </button>
          <button
            type="button"
            onClick={toJson}
            className={[
              "rounded border px-2 py-1 text-xs",
              mode === "json"
                ? "border-accent bg-accent/15 text-text"
                : "border-border text-muted hover:bg-raised",
            ].join(" ")}
          >
            Raw JSON
          </button>
        </div>
      </div>

      {mode === "form" ? (
        <div>
          {fields.map((field) => (
            <FormField
              key={field.name}
              field={field}
              value={values[field.name] ?? ""}
              onChange={(value) => setValues((current) => ({ ...current, [field.name]: value }))}
            />
          ))}
          {fields.length === 0 && (
            <p className="text-sm text-muted">This tool takes no simple arguments.</p>
          )}
        </div>
      ) : (
        <div>
          <textarea
            value={json}
            spellCheck={false}
            onChange={(event) => {
              setJson(event.target.value);
              setJsonError("");
            }}
            rows={8}
            aria-label="Arguments as JSON"
            className="w-full rounded border border-border bg-surface p-2 font-mono text-xs"
          />
          {jsonError !== "" && <p className="mt-1 text-xs text-danger">{jsonError}</p>}
        </div>
      )}

      <div className="mt-3 flex items-center gap-2">
        <button
          type="button"
          onClick={run}
          disabled={callTool.isPending}
          className="rounded bg-accent px-3 py-2 text-sm font-medium text-on-accent disabled:opacity-60"
        >
          {callTool.isPending ? "Calling…" : "Call tool"}
        </button>
        {result !== null && (
          <span className="text-xs text-muted">
            <CallStatusBadge status={result.status} /> · {formatDuration(result.durationMs)}
          </span>
        )}
      </div>

      {result !== null && (
        <div className="mt-4">
          <div className="mb-1 flex items-center gap-2">
            <h3 className="text-sm font-semibold">Result</h3>
            <CopyButton
              className="ml-auto"
              value={pretty(result.error != null ? { error: result.error } : result.result)}
            />
          </div>
          {result.error != null && (
            <p className="mb-2 rounded border border-danger/40 bg-danger/10 p-2 text-sm">
              {result.error}
            </p>
          )}
          <Json value={result.result} className="max-h-96" />
        </div>
      )}
    </div>
  );
}
