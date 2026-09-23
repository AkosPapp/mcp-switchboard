/**
 * Registers the console's service worker (public/sw.js, spec.md U61).
 *
 * Installability is a bonus, never a requirement: an unsupported browser, a
 * denied registration or any other failure here must degrade silently to the
 * ordinary web app rather than surface an error the user can't act on.
 */
export function registerServiceWorker(): void {
  if (typeof window === "undefined" || !("serviceWorker" in navigator)) {
    return;
  }

  window.addEventListener("load", () => {
    navigator.serviceWorker.register("/sw.js").catch(() => {
      // Feature-detected above; a failure here (denied, unsupported, network)
      // is not worth telling the user about - the app works without it.
    });
  });
}
