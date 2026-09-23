/**
 * Drives the "Install app" affordance (spec.md U62).
 *
 * Two ways in:
 *  - Chromium/Android fires `beforeinstallprompt`, which this hook captures
 *    (and prevents the browser's own mini-infobar from also showing) so the
 *    console can offer its own small, dismissible UI instead and call
 *    `.prompt()` on the captured event when the user opts in.
 *  - iOS Safari never fires that event and has no programmatic install API at
 *    all, so there the hook just detects "iOS Safari, not already installed"
 *    and the caller shows manual Share -> Add to Home Screen steps instead.
 *
 * A dismissal is remembered per-browser (localStorage) so the affordance
 * doesn't nag on every visit; it comes back if the app is later actually
 * installed and un-installed, because a fresh `beforeinstallprompt`/UA check
 * runs again and the dismissal key is generation-agnostic. localStorage is
 * best-effort - private browsing or a blocked store just means the affordance
 * can reappear, which is harmless.
 */

import { useEffect, useState } from "react";

const DISMISS_KEY = "switchboard:install-prompt-dismissed";

// The event type isn't in lib.dom yet.
interface BeforeInstallPromptEvent extends Event {
  prompt: () => Promise<void>;
  userChoice: Promise<{ outcome: "accepted" | "dismissed" }>;
}

export type InstallPromptKind = "native" | "ios";

export interface InstallPromptState {
  kind: InstallPromptKind;
  /** Trigger the browser's own install UI. Only meaningful for kind "native". */
  promptInstall: () => Promise<void>;
  dismiss: () => void;
}

function isStandalone(): boolean {
  if (typeof window === "undefined") return false;
  // iOS Safari's own flag.
  if ((navigator as unknown as { standalone?: boolean }).standalone) return true;
  // The standard way, for everyone else (also true on iOS 17+ home-screen apps).
  return window.matchMedia?.("(display-mode: standalone)").matches ?? false;
}

function isIosSafari(): boolean {
  if (typeof navigator === "undefined") return false;
  const ua = navigator.userAgent;
  const isIos = /iphone|ipad|ipod/i.test(ua) || (ua.includes("Macintosh") && "ontouchend" in document);
  if (!isIos) return false;
  // Exclude other iOS browsers, which embed Safari's engine but say so too:
  // Chrome ("CriOS"), Firefox ("FxiOS"), Edge ("EdgiOS").
  return !/crios|fxios|edgios/i.test(ua);
}

function wasDismissed(): boolean {
  try {
    return window.localStorage.getItem(DISMISS_KEY) === "1";
  } catch {
    return false;
  }
}

function rememberDismissed(): void {
  try {
    window.localStorage.setItem(DISMISS_KEY, "1");
  } catch {
    // Best-effort; nothing to fall back to.
  }
}

export function useInstallPrompt(): InstallPromptState | null {
  const [deferred, setDeferred] = useState<BeforeInstallPromptEvent | null>(null);
  const [dismissed, setDismissed] = useState<boolean>(wasDismissed);
  const [showIos, setShowIos] = useState(false);

  useEffect(() => {
    if (isStandalone()) return;

    const onBeforeInstallPrompt = (event: Event) => {
      // Stop the browser's default mini-infobar; the console shows its own.
      event.preventDefault();
      setDeferred(event as BeforeInstallPromptEvent);
    };
    window.addEventListener("beforeinstallprompt", onBeforeInstallPrompt);

    if (isIosSafari()) {
      setShowIos(true);
    }

    const onInstalled = () => {
      setDeferred(null);
      setShowIos(false);
    };
    window.addEventListener("appinstalled", onInstalled);

    return () => {
      window.removeEventListener("beforeinstallprompt", onBeforeInstallPrompt);
      window.removeEventListener("appinstalled", onInstalled);
    };
  }, []);

  if (dismissed) return null;

  const dismiss = () => {
    rememberDismissed();
    setDismissed(true);
  };

  if (deferred) {
    return {
      kind: "native",
      dismiss,
      promptInstall: async () => {
        await deferred.prompt();
        const choice = await deferred.userChoice;
        setDeferred(null);
        if (choice.outcome === "accepted") {
          rememberDismissed();
          setDismissed(true);
        }
      },
    };
  }

  if (showIos) {
    return {
      kind: "ios",
      dismiss,
      promptInstall: async () => {},
    };
  }

  return null;
}
