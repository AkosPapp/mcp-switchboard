import { useEndpoints } from "../api/queries";
import type { EndpointRow } from "../api/types";
import CopyButton from "../components/CopyButton";

function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function Connect({ installCommand }: { installCommand: string | null }) {
  return (
    <section className="rounded-md border border-border bg-surface">
      <h2 className="border-b border-border px-3 py-2 text-sm font-semibold">Connect a client</h2>
      <div className="px-3 py-3">
        {installCommand !== null ? (
          <>
            <div className="flex items-start gap-2">
              <code className="min-w-0 flex-1 overflow-x-auto whitespace-pre rounded border border-border bg-raised p-2 font-mono text-xs">
                {installCommand}
              </code>
              <CopyButton value={installCommand} className="min-h-[44px] shrink-0" />
            </div>
            <p className="mt-2 text-xs text-muted">
              This command carries the tunnel token, which is why it is shown here and nowhere
              else — treat it like the token itself.
            </p>
          </>
        ) : (
          <p className="text-xs text-muted">
            <code className="font-mono text-text">MCP_SWITCHBOARD_PUBLIC_URL</code> is unset. Set
            it to the tunnel listener's externally-reachable address (what a Tailscale Funnel
            answers on, say) and a ready-to-run install command appears here.
          </p>
        )}
      </div>
    </section>
  );
}

function urlOf(base: string, row: EndpointRow): string {
  return base + row.path;
}

function Row({ row, base }: { row: EndpointRow; base: string }) {
  const url = urlOf(base, row);
  return (
    <tr className="border-b border-border align-top">
      <td className="px-3 py-2">
        <span className="rounded bg-raised px-1.5 py-0.5 text-xs text-muted">
          {row.scope.replace("_", " ")}
        </span>
        {row.description ? (
          <p className="mt-1 max-w-[18rem] text-xs text-muted">{row.description}</p>
        ) : null}
      </td>
      <td className="px-3 py-2">
        <code className="break-all font-mono text-xs">{url}</code>
      </td>
      <td className="px-3 py-2">
        <code className="break-all font-mono text-xs text-muted">{row.example}</code>
      </td>
      <td className="px-3 py-2 text-right">
        <CopyButton value={url} label="Copy URL" />
      </td>
    </tr>
  );
}

function Card({ row, base }: { row: EndpointRow; base: string }) {
  const url = urlOf(base, row);
  return (
    <li className="border-b border-border px-3 py-3">
      <div className="flex items-center justify-between gap-2">
        <span className="rounded bg-raised px-1.5 py-0.5 text-xs text-muted">
          {row.scope.replace("_", " ")}
        </span>
        <CopyButton value={url} label="Copy URL" className="min-h-[44px]" />
      </div>
      <code className="mt-2 block break-all font-mono text-xs">{url}</code>
      <code className="mt-1 block break-all font-mono text-xs text-muted">{row.example}</code>
      {row.description ? <p className="mt-1 text-xs text-muted">{row.description}</p> : null}
    </li>
  );
}

export default function Endpoints() {
  const endpoints = useEndpoints();

  if (endpoints.isError) {
    return (
      <p className="px-3 py-6 text-sm text-danger">
        Could not load endpoints: {messageOf(endpoints.error)}
      </p>
    );
  }
  if (endpoints.isPending) {
    return <p className="px-3 py-6 text-sm text-muted">Loading endpoints…</p>;
  }

  const { localBaseUrl, installCommand, rows } = endpoints.data;

  return (
    <div className="mx-auto flex max-w-4xl flex-col gap-4 px-3 py-4 sm:px-4">
      <Connect installCommand={installCommand} />

      <section className="rounded-md border border-border bg-surface">
        <h2 className="border-b border-border px-3 py-2 text-sm font-semibold">MCP endpoints</h2>

        {rows.length === 0 ? (
          <p className="px-3 py-6 text-sm text-muted">No endpoints are reachable yet.</p>
        ) : (
          <>
            <table className="hidden w-full border-collapse text-sm md:table">
              <thead className="text-left text-xs uppercase tracking-wide text-muted">
                <tr className="border-b border-border">
                  <th className="px-3 py-2 font-medium">Scope</th>
                  <th className="px-3 py-2 font-medium">URL</th>
                  <th className="px-3 py-2 font-medium">Example tool name</th>
                  <th className="px-3 py-2" />
                </tr>
              </thead>
              <tbody>
                {rows.map((row) => (
                  <Row key={row.path} row={row} base={localBaseUrl} />
                ))}
              </tbody>
            </table>

            <ul className="md:hidden" aria-label="MCP endpoints">
              {rows.map((row) => (
                <Card key={row.path} row={row} base={localBaseUrl} />
              ))}
            </ul>
          </>
        )}
      </section>
    </div>
  );
}
