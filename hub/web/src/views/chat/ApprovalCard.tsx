import { useEffect, useState } from "react";

import Json from "../../components/Json";
import { clock, remainingSeconds } from "./format";

interface Props {
  runId: string;
  callId: string;
  tool: string;
  arguments: unknown;
  expiresAt: string;
  /** Resolves when the decision is sent (or failed and was reported); the parent removes the card at once (U31). */
  onDecide: (approved: boolean) => void | Promise<void>;
  busy?: boolean;
}

/** A run blocked on a human decision, with the time left before auto-deny (U10). */
export default function ApprovalCard({ tool, arguments: args, expiresAt, onDecide, busy = false }: Props) {
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(id);
  }, []);

  const left = remainingSeconds(expiresAt, now);

  return (
    <div
      role="group"
      aria-label={`approval for ${tool}`}
      className="my-2 rounded border border-warn bg-surface p-3 text-sm"
      data-testid="approval-card"
    >
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <span className="font-medium">
            Approve <code className="font-mono text-xs">{tool}</code>?
          </span>
        </div>
        <span className="text-xs text-muted" aria-live="off">
          {left > 0 ? `auto-deny in ${clock(left)}` : "expired"}
        </span>
      </div>
      <Json value={args} className="mt-2 max-h-64" />
      <div className="mt-2 flex gap-2">
        <button
          type="button"
          disabled={busy}
          onClick={() => void onDecide(true)}
          className="min-h-[44px] flex-1 rounded bg-accent px-3 py-1.5 text-sm font-medium text-bg sm:min-h-0 sm:flex-none"
        >
          Approve
        </button>
        <button
          type="button"
          disabled={busy}
          onClick={() => void onDecide(false)}
          className="min-h-[44px] flex-1 rounded border border-border px-3 py-1.5 text-sm sm:min-h-0 sm:flex-none"
        >
          Deny
        </button>
      </div>
    </div>
  );
}
