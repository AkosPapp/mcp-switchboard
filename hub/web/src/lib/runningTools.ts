/** Which tool calls are executing right now (spec.md U32). Names are the exposed names, as-is. */

export interface CallView {
  callId: string;
  name?: string;
  done: boolean;
}

/**
 * Calls seen without a result, minus those still waiting on a human (they show
 * an approval card, not "running"). Order is first seen; duplicates collapse.
 */
export function runningToolNames(
  calls: CallView[],
  waitingCallIds: ReadonlySet<string>,
  resultIds: ReadonlySet<string>,
): string[] {
  const seen = new Set<string>();
  const names: string[] = [];
  for (const c of calls) {
    if (!c.name || c.done || resultIds.has(c.callId) || waitingCallIds.has(c.callId) || seen.has(c.callId)) continue;
    seen.add(c.callId);
    names.push(c.name);
  }
  return names;
}

export const runningLabel = (names: string[]) => `Running ${names.join(", ")}…`;
