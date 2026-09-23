import { useState } from "react";

import { useInstallPrompt } from "../hooks/useInstallPrompt";

/**
 * A small, dismissible "Install app" affordance (spec.md U62) - never a modal,
 * so it never blocks the console it's advertising. Renders nothing until
 * there's actually something to offer (a captured `beforeinstallprompt`, or
 * iOS Safari not yet installed), and nothing again once dismissed or installed.
 */
export function InstallPrompt() {
  const install = useInstallPrompt();
  const [showIosSteps, setShowIosSteps] = useState(false);

  if (!install) return null;

  return (
    <div
      role="status"
      className="fixed inset-x-0 bottom-4 z-40 mx-auto flex w-fit max-w-[92vw] flex-col gap-2 rounded-md border border-border bg-raised px-3 py-2.5 text-sm shadow-lg"
      style={{ marginBottom: "env(safe-area-inset-bottom)" }}
    >
      <div className="flex items-center gap-3">
        <span className="h-4 w-4 shrink-0 rounded-sm bg-accent" aria-hidden="true" />
        <span className="flex-1">Install the console for full-screen access from your home screen.</span>
        <button
          type="button"
          className="shrink-0 rounded px-2 py-1 text-muted hover:bg-surface hover:text-text"
          onClick={install.dismiss}
          aria-label="Dismiss install prompt"
        >
          ✕
        </button>
      </div>

      {install.kind === "native" && (
        <div className="flex justify-end">
          <button
            type="button"
            className="rounded bg-accent px-2.5 py-1.5 font-medium text-white hover:opacity-90"
            onClick={() => void install.promptInstall()}
          >
            Install app
          </button>
        </div>
      )}

      {install.kind === "ios" && (
        <div className="flex flex-col gap-2">
          {!showIosSteps ? (
            <div className="flex justify-end">
              <button
                type="button"
                className="rounded bg-accent px-2.5 py-1.5 font-medium text-white hover:opacity-90"
                onClick={() => setShowIosSteps(true)}
              >
                How?
              </button>
            </div>
          ) : (
            <p className="text-muted">
              Tap <span className="font-medium text-text">Share</span>, then{" "}
              <span className="font-medium text-text">Add to Home Screen</span>.
            </p>
          )}
        </div>
      )}
    </div>
  );
}
