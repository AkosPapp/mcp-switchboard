/**
 * Registration for the console's service worker (hub/web/public/sw.js).
 *
 * Kept separate from the push-subscription logic in push.ts: this file's job
 * is "get a worker installed", not "turn on notifications" - the note left in
 * sw.js about merging with the PWA install-prompt work's own service worker
 * registration applies here too, so this stays a single small function
 * rather than growing app-specific setup.
 */

/** Registers /sw.js if the browser supports it. Resolves to null when it
 * does not (the toggle in Settings hides itself in that case) or when
 * registration fails for some other reason - a console that cannot get push
 * notifications working must never be a console that fails to load. */
export async function registerServiceWorker(): Promise<ServiceWorkerRegistration | null> {
  if (!("serviceWorker" in navigator)) return null;
  try {
    return await navigator.serviceWorker.register("/sw.js");
  } catch (err) {
    console.warn("service worker registration failed", err);
    return null;
  }
}

/** Whatever registration already exists, or a fresh one. */
export async function getServiceWorkerRegistration(): Promise<ServiceWorkerRegistration | null> {
  if (!("serviceWorker" in navigator)) return null;
  const existing = await navigator.serviceWorker.getRegistration();
  if (existing) return existing;
  return registerServiceWorker();
}
