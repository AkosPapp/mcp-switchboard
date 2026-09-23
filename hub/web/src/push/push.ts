/**
 * Turning "Notify me on this device" on and off (spec.md 8.7).
 *
 * The whole flow: check support, ask permission, register the service
 * worker, subscribe with the hub's VAPID public key, and hand the
 * subscription to the hub. Every step degrades gracefully - unsupported,
 * denied, or a network failure all resolve to a explained failure rather
 * than a broken toggle.
 */

import { getServiceWorkerRegistration } from "./serviceWorker";

export type PushSupport = "unsupported" | "denied" | "available";

/** What the toggle should show before the user does anything. */
export function pushSupport(): PushSupport {
  if (
    typeof window === "undefined" ||
    !("serviceWorker" in navigator) ||
    !("PushManager" in window) ||
    !("Notification" in window)
  ) {
    return "unsupported";
  }
  if (Notification.permission === "denied") return "denied";
  return "available";
}

/** Base64url (the PushManager spec's encoding, not standard base64) to the
 * Uint8Array applicationServerKey wants. */
function urlBase64ToUint8Array(base64: string): Uint8Array<ArrayBuffer> {
  const padding = "=".repeat((4 - (base64.length % 4)) % 4);
  const normalised = (base64 + padding).replace(/-/g, "+").replace(/_/g, "/");
  const raw = atob(normalised);
  const bytes = new Uint8Array(new ArrayBuffer(raw.length));
  for (let i = 0; i < raw.length; i++) bytes[i] = raw.charCodeAt(i);
  return bytes;
}

export class PushSetupError extends Error {}

/**
 * Enables push: permission, service worker, PushManager subscription. Throws
 * PushSetupError with a message fit to show the user on any step that fails,
 * including a denied permission prompt.
 */
export async function enablePush(vapidPublicKey: string): Promise<PushSubscriptionJSON> {
  if (pushSupport() === "unsupported") {
    throw new PushSetupError("This browser does not support push notifications.");
  }

  const permission = await Notification.requestPermission();
  if (permission !== "granted") {
    throw new PushSetupError("Notification permission was not granted.");
  }

  const registration = await getServiceWorkerRegistration();
  if (!registration) {
    throw new PushSetupError("The service worker could not be installed.");
  }

  const existing = await registration.pushManager.getSubscription();
  const subscription =
    existing ??
    (await registration.pushManager.subscribe({
      userVisibleOnly: true,
      applicationServerKey: urlBase64ToUint8Array(vapidPublicKey),
    }));

  return subscription.toJSON();
}

/** Unsubscribes the browser's own PushManager registration, independent of
 * whether the hub-side delete succeeds - a device that no longer wants
 * pushes should stop receiving them even if the API call to forget it on the
 * hub fails, since the alternative is silently leaving the toggle "off" on a
 * device that is still subscribed. */
export async function disablePushLocally(): Promise<void> {
  if (!("serviceWorker" in navigator)) return;
  const registration = await navigator.serviceWorker.getRegistration();
  const subscription = await registration?.pushManager.getSubscription();
  await subscription?.unsubscribe();
}
