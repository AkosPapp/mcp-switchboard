import type { ClientEnvironment } from "../api/types";

const KIND_NAMES: Record<string, string> = {
  devcontainer: "devcontainer",
  container: "container",
  direnv: "direnv",
  "nix-shell": "nix-shell",
  venv: "venv",
};

// Subtle, distinct tints; they are hints, not status colours.
const KIND_STYLES: Record<string, string> = {
  devcontainer: "border-sky-500/40 bg-sky-500/10 text-sky-600 dark:text-sky-300",
  container: "border-cyan-500/40 bg-cyan-500/10 text-cyan-600 dark:text-cyan-300",
  direnv: "border-emerald-500/40 bg-emerald-500/10 text-emerald-600 dark:text-emerald-300",
  "nix-shell": "border-violet-500/40 bg-violet-500/10 text-violet-600 dark:text-violet-300",
  venv: "border-amber-500/40 bg-amber-500/10 text-amber-600 dark:text-amber-300",
};
const FALLBACK_STYLE = "border-border bg-raised text-muted";

/** Human name of one kind, e.g. "nix-shell (pure)". */
export function kindName(kind: string, environment?: ClientEnvironment | null): string {
  const base = KIND_NAMES[kind] ?? kind;
  if (kind === "nix-shell") {
    const mode = environment?.details?.nixShell;
    if (mode === "pure" || mode === "impure") return `${base} (${mode})`;
  }
  return base;
}

/** Everything searchable about an environment, for the Connections filter. */
export function environmentSearchText(environment?: ClientEnvironment | null): (string | null)[] {
  if (!environment) return [];
  return [
    environment.project ?? null,
    environment.workspace ?? null,
    ...environment.kinds,
    ...environment.kinds.map((k) => kindName(k, environment)),
  ];
}

function summary(label: string, environment?: ClientEnvironment | null): string {
  const parts = [label];
  if (environment?.project) parts.push(`project ${environment.project}`);
  if (environment && environment.kinds.length > 0) {
    parts.push(`environment ${environment.kinds.map((k) => kindName(k, environment)).join(", ")}`);
  }
  return parts.join(", ");
}

function tooltip(label: string, environment?: ClientEnvironment | null): string {
  const lines = [label];
  if (environment?.project) lines.push(`project: ${environment.project}`);
  if (environment?.workspace) lines.push(`workspace: ${environment.workspace}`);
  if (environment && environment.kinds.length > 0) {
    lines.push(`environment: ${environment.kinds.map((k) => kindName(k, environment)).join(", ")}`);
  }
  for (const [key, value] of Object.entries(environment?.details ?? {})) {
    lines.push(`${key}: ${value}`);
  }
  return lines.join("\n");
}

/**
 * A client as the user picks it: `label · project` plus a chip per detected
 * environment kind. `environment` absent means an older client that did not
 * report one; nothing is added in that case.
 *
 * Sizing: by default (`wrap` false) the badge is `inline-flex`, shrink-to-fit
 * and truncates label/project - right for a tight, fixed-size spot (a Graph
 * node card, a one-line chip). Pass `wrap` where the badge has a whole row or
 * block to itself (e.g. a client-group header): it becomes a full-width flex
 * box and the label is never truncated - only the (less identifying) project
 * name may still ellipsize, and the whole thing wraps to a second line rather
 * than silently dropping most of a long name into a "legi…" fragment.
 */
export default function ClientBadge({
  label,
  environment,
  connected,
  compact = false,
  wrap = false,
  bare = false,
}: {
  label: string;
  environment?: ClientEnvironment | null;
  connected?: boolean;
  compact?: boolean;
  /** Full-width, wrapping layout with an untruncated label; see above. */
  wrap?: boolean;
  /** Label and project only: no environment chips (they stay in the tooltip). */
  bare?: boolean;
}) {
  const project = environment?.project;
  const kinds = bare ? [] : (environment?.kinds ?? []);
  return (
    <span
      className={[
        wrap ? "flex w-full" : "inline-flex max-w-full",
        "min-w-0 flex-wrap items-center gap-x-1.5 gap-y-0.5 align-middle",
      ].join(" ")}
      title={tooltip(label, environment)}
      aria-label={summary(label, environment)}
      data-testid="client-badge"
    >
      {connected !== undefined && (
        <span
          aria-hidden="true"
          className={["h-2 w-2 shrink-0 rounded-full", connected ? "bg-ok" : "bg-muted/50"].join(" ")}
        />
      )}
      <span className={["flex min-w-0 max-w-full items-baseline gap-1", wrap ? "flex-wrap" : ""].join(" ")}>
        <span className={wrap ? "font-semibold" : "truncate font-semibold"}>{label}</span>
        {project && (
          <>
            <span aria-hidden="true" className="shrink-0 text-muted">
              ·
            </span>
            <span className={wrap ? "min-w-0 truncate text-muted" : "truncate text-muted"}>{project}</span>
          </>
        )}
      </span>
      {kinds.map((kind) => (
        <span
          key={kind}
          data-kind={kind}
          className={[
            "shrink-0 whitespace-nowrap rounded border font-mono leading-none",
            compact ? "px-1 py-0.5 text-[9px]" : "px-1.5 py-0.5 text-[10px]",
            KIND_STYLES[kind] ?? FALLBACK_STYLE,
          ].join(" ")}
        >
          {kindName(kind, environment)}
        </span>
      ))}
    </span>
  );
}
