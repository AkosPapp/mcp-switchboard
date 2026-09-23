/**
 * Effective grants, mirroring spec.md A10-A15.
 *
 * The hub is authoritative; this exists so the console can say what a toggle
 * will do before it is sent (A12/A13 widening warning, U18 spawn refusal) and
 * can list every ServerRef an agent may use next to the live registry (U16).
 */

import type { GraphGrant, GraphServer, GrantInput, GrantSource } from "../api/types";

export interface ServerRef {
  label: string;
  project: string;
  server: string;
}

type GrantLike = ServerRef & { allowed: boolean; source?: GrantSource };

export const WILDCARD = "*";

export function refKey(ref: ServerRef): string {
  return `${ref.label}\u0000${ref.project ?? ""}\u0000${ref.server}`;
}

/** A11: each of the three fields may be `*`, matching any value. */
export function matches(pattern: ServerRef, ref: ServerRef): boolean {
  return (
    (pattern.label === WILDCARD || pattern.label === ref.label) &&
    (pattern.project === WILDCARD || (pattern.project ?? "") === (ref.project ?? "")) &&
    (pattern.server === WILDCARD || pattern.server === ref.server)
  );
}

function specificity(ref: ServerRef): number {
  return [ref.label, ref.project, ref.server].filter((f) => f !== WILDCARD).length;
}

/**
 * Whether a set of grants lets an agent use `ref`. The most specific matching
 * row wins, so an explicit deny carves an exception out of a wildcard allow;
 * at equal specificity a deny wins. No matching row means not allowed.
 */
export function effective<G extends GrantLike>(
  grants: readonly G[],
  ref: ServerRef,
): { allowed: boolean; grant: G | null } {
  let best: G | null = null;
  for (const grant of grants) {
    if (!matches(grant, ref)) continue;
    if (best === null) {
      best = grant;
      continue;
    }
    const a = specificity(grant);
    const b = specificity(best);
    if (a > b || (a === b && !grant.allowed && best.allowed)) best = grant;
  }
  return { allowed: best?.allowed ?? false, grant: best };
}

/**
 * Would allowing `ref` on a child give it more than its parent holds (A12/A13)?
 * A root has no parent, so nothing widens.
 */
export function wouldWiden(
  parentGrants: readonly GrantLike[] | null,
  ref: ServerRef,
): boolean {
  if (parentGrants === null) return false;
  return !effective(parentGrants, ref).allowed;
}

export interface PermissionRow extends ServerRef {
  key: string;
  allowed: boolean;
  /** Source of the row that decides `allowed`; null when nothing grants it. */
  source: GrantSource | null;
  /** The ServerRef matches something currently connected (I3, A15). */
  connected: boolean;
  /** A human grant its parent no longer holds (A14). */
  orphaned: boolean;
  /** Decided by a wildcard row rather than one of its own. */
  viaWildcard: boolean;
  toolCount: number | null;
  /** Enabling this row would exceed the parent's set. */
  widens: boolean;
}

/**
 * Every ServerRef an agent may use or could be given: the live servers, plus any
 * grant held for something that is not connected (kept, not deleted - I3).
 */
export function permissionRows(
  grants: readonly GraphGrant[],
  servers: readonly GraphServer[],
  parentGrants: readonly GrantLike[] | null,
): PermissionRow[] {
  const rows = new Map<string, PermissionRow>();

  const build = (ref: ServerRef, connected: boolean, toolCount: number | null): PermissionRow => {
    const { allowed, grant } = effective(grants, ref);
    const own = grants.find((g) => refKey(g) === refKey(ref));
    const decisive = own ?? grant;
    return {
      ...ref,
      key: refKey(ref),
      allowed,
      source: decisive?.source ?? null,
      connected,
      orphaned: Boolean(own?.orphaned ?? (decisive as GraphGrant | null)?.orphaned),
      viaWildcard: own === undefined && decisive !== null,
      toolCount,
      widens: wouldWiden(parentGrants, ref),
    };
  };

  for (const server of servers) {
    rows.set(refKey(server), build(server, server.connected, server.toolCount));
  }
  for (const grant of grants) {
    const key = refKey(grant);
    if (rows.has(key)) continue;
    // A wildcard is connected when anything live matches it.
    const connected =
      grant.connected || servers.some((s) => s.connected && matches(grant, s));
    rows.set(key, build(grant, connected, null));
  }

  return [...rows.values()].sort(
    (a, b) =>
      a.label.localeCompare(b.label) ||
      (a.project ?? "").localeCompare(b.project ?? "") ||
      a.server.localeCompare(b.server),
  );
}

export function toInput(ref: ServerRef, allowed: boolean): GrantInput {
  return { label: ref.label, project: ref.project, server: ref.server, allowed };
}

/** U18: what a new child starts with - everything the parent may use. */
export function defaultChildGrants(parentGrants: readonly GrantLike[]): GrantInput[] {
  return parentGrants.filter((g) => g.allowed).map((g) => toInput(g, true));
}

/** U18/A12: requested grants the parent does not hold. */
export function refusedGrants(
  parentGrants: readonly GrantLike[],
  requested: readonly GrantInput[],
): GrantInput[] {
  return requested.filter(
    (r) =>
      r.allowed &&
      !effective(parentGrants, { label: r.label, project: r.project ?? "", server: r.server })
        .allowed,
  );
}

export function describeRef(ref: ServerRef): string {
  return ref.project
    ? `${ref.label}/${ref.project}/${ref.server}`
    : `${ref.label}/${ref.server}`;
}
