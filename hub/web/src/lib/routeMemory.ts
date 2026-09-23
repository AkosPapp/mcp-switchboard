import { forgetMemory, readMemory, writeMemory } from "./uiMemory";

/** The top-level tabs; each one remembers the last place visited under it. */
export const TAB_ROOTS = [
  "/connections",
  "/calls",
  "/endpoints",
  "/chat",
  "/graph",
  "/prompts",
] as const;

export interface Loc {
  pathname: string;
  search: string;
}

/** tab root -> full remembered location ("/chat/abc", "/connections?tool=x"). */
export interface RouteMemory {
  tabs: Record<string, string>;
  /** The most recent location under any tab; where "/" sends you. */
  last: string | null;
}

export const EMPTY_ROUTE_MEMORY: RouteMemory = { tabs: {}, last: null };

export function tabOf(pathname: string): string | null {
  return TAB_ROOTS.find((root) => pathname === root || pathname.startsWith(`${root}/`)) ?? null;
}

/** True when `location` is a plain path-plus-query under `tab`. */
function belongsTo(tab: string, location: unknown): location is string {
  return (
    typeof location === "string" &&
    !location.startsWith("//") &&
    tabOf(location.split(/[?#]/)[0]) === tab
  );
}

/** Where the nav link for `tab` should go from `current`. */
export function nextLocationForTab(tab: string, current: Loc, memory: RouteMemory): string {
  // Clicking the tab you are on goes to its bare root, so you can deselect.
  if (tabOf(current.pathname) === tab) return tab;
  const remembered = memory.tabs[tab];
  return belongsTo(tab, remembered) ? remembered : tab;
}

/** Where "/" should go: the last place visited, else Connections. */
export function restoreTarget(memory: RouteMemory): string {
  const last = memory.last;
  if (typeof last !== "string") return "/connections";
  const tab = tabOf(last.split(/[?#]/)[0]);
  return tab !== null && belongsTo(tab, last) ? last : "/connections";
}

/** The string worth remembering for a location, or null when it is not under a tab. */
export function locationToRemember(location: Loc): string | null {
  if (tabOf(location.pathname) === null) return null;
  // Prefilled arguments can be large and belong to one hand-off, not to a place.
  const params = new URLSearchParams(location.search);
  params.delete("args");
  const search = params.toString();
  return search === "" ? location.pathname : `${location.pathname}?${search}`;
}

export function remember(memory: RouteMemory, location: Loc): RouteMemory {
  const entry = locationToRemember(location);
  if (entry === null) return memory;
  const tab = tabOf(location.pathname) as string;
  return { tabs: { ...memory.tabs, [tab]: entry }, last: entry };
}

const KEY = "route";

/**
 * The Agents tab became Prompts: a location remembered under the old root
 * ("/agents", "/agents/<id>") maps to the same place under the new one.
 */
export function migrateLocation<T>(location: T): T {
  return typeof location === "string" ? (location.replace(/^\/agents(?=$|[/?#])/, "/prompts") as T & string) : location;
}

function validate(value: unknown): RouteMemory | undefined {
  if (value === null || typeof value !== "object") return undefined;
  const raw = value as { tabs?: unknown; last?: unknown };
  const tabs: Record<string, string> = {};
  if (raw.tabs !== null && typeof raw.tabs === "object") {
    for (const [oldTab, oldLoc] of Object.entries(raw.tabs)) {
      const tab = migrateLocation(oldTab);
      const loc = migrateLocation(oldLoc);
      if (belongsTo(tab, loc)) tabs[tab] = loc;
    }
  }
  return { tabs, last: typeof raw.last === "string" ? migrateLocation(raw.last) : null };
}

export function loadRouteMemory(): RouteMemory {
  return readMemory(KEY, validate, EMPTY_ROUTE_MEMORY);
}

export function saveLocation(location: Loc): void {
  if (locationToRemember(location) === null) return;
  writeMemory(KEY, remember(loadRouteMemory(), location));
}

/**
 * Drop what is remembered for `tab` (its target no longer exists). Also clears
 * `last` when it points into that tab, so a reload does not walk back to it.
 */
export function forgetTab(tab: string): void {
  const memory = loadRouteMemory();
  const tabs = { ...memory.tabs };
  delete tabs[tab];
  const last = memory.last !== null && tabOf(memory.last.split(/[?#]/)[0]) === tab ? null : memory.last;
  if (Object.keys(tabs).length === 0 && last === null) forgetMemory(KEY);
  else writeMemory(KEY, { tabs, last });
}
