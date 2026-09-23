/**
 * The browser's memory of where the user was, kept in localStorage only.
 *
 * Nothing here may ever throw or be required: storage is absent or throws in
 * private windows and when site data is blocked, holds whatever an older or
 * newer console wrote, and can be edited by hand. Every read validates, every
 * failure falls back to the default, and the app works the same without it.
 */

/** Bump when the shape of what is stored changes; old data is then ignored. */
export const MEMORY_VERSION = 1;
const PREFIX = `mcpsb.ui.v${MEMORY_VERSION}.`;

function storage(): Storage | null {
  try {
    return globalThis.localStorage ?? null;
  } catch {
    return null;
  }
}

/** Turns an unknown parsed value into a T, or undefined when it is not one. */
export type Validate<T> = (value: unknown) => T | undefined;

export function readMemory<T>(key: string, validate: Validate<T>, fallback: T): T {
  try {
    const raw = storage()?.getItem(PREFIX + key);
    if (raw === null || raw === undefined) return fallback;
    const envelope: unknown = JSON.parse(raw);
    if (
      envelope === null ||
      typeof envelope !== "object" ||
      (envelope as { v?: unknown }).v !== MEMORY_VERSION
    ) {
      return fallback;
    }
    const value = validate((envelope as { d?: unknown }).d);
    return value === undefined ? fallback : value;
  } catch {
    return fallback;
  }
}

export function writeMemory(key: string, value: unknown): void {
  try {
    storage()?.setItem(PREFIX + key, JSON.stringify({ v: MEMORY_VERSION, d: value }));
  } catch {
    // Quota, blocked storage: the memory is a convenience, not state.
  }
}

export function forgetMemory(key: string): void {
  try {
    storage()?.removeItem(PREFIX + key);
  } catch {
    // Nothing to do.
  }
}

export const isString: Validate<string> = (v) => (typeof v === "string" ? v : undefined);
export const isBoolean: Validate<boolean> = (v) => (typeof v === "boolean" ? v : undefined);
