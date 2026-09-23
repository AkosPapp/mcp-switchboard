import { useEffect, useState } from "react";

import { ApiError, getVapidPublicKey, subscribePush, unsubscribePush } from "../api/client";
import { disablePushLocally, enablePush, pushSupport, type PushSupport } from "../push/push";
import { useToast } from "../components/Toast";

const STORAGE_KEY = "mcp-switchboard:push-subscription-id";

function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

/** Persisted only so this device can unsubscribe itself later; never read by
 * anything but this component, and never assumed to survive - a cleared
 * storage just means "unsubscribe" becomes a local no-op instead of a hub
 * round trip, which is harmless. */
function readSubscriptionId(): string | null {
  try {
    return window.localStorage.getItem(STORAGE_KEY);
  } catch {
    return null;
  }
}

function writeSubscriptionId(id: string | null) {
  try {
    if (id === null) window.localStorage.removeItem(STORAGE_KEY);
    else window.localStorage.setItem(STORAGE_KEY, id);
  } catch {
    // Private browsing, storage disabled, whatever - the toggle still works
    // for this session, it just cannot remember across a reload.
  }
}

type Availability = PushSupport | "checking" | "hub-disabled";

function explanation(state: Availability): string | null {
  switch (state) {
    case "unsupported":
      return "This browser does not support push notifications.";
    case "denied":
      return "Notifications are blocked for this site. Allow them in your browser settings to enable this.";
    case "hub-disabled":
      return "This hub has not been configured for push notifications.";
    case "checking":
      return null;
    case "available":
      return null;
  }
}

export default function Settings() {
  const toast = useToast();
  const [availability, setAvailability] = useState<Availability>("checking");
  const [vapidPublicKey, setVapidPublicKey] = useState<string | null>(null);
  const [enabled, setEnabled] = useState(false);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let cancelled = false;

    async function probe() {
      const support = pushSupport();
      if (support !== "available") {
        if (!cancelled) setAvailability(support);
        return;
      }
      try {
        const { publicKey } = await getVapidPublicKey();
        if (cancelled) return;
        setVapidPublicKey(publicKey);
        setAvailability("available");

        const registration = await navigator.serviceWorker.getRegistration();
        const subscription = await registration?.pushManager.getSubscription();
        if (!cancelled) setEnabled(subscription != null);
      } catch (err) {
        if (cancelled) return;
        // A 404 here means the hub has no VAPID keys configured - the same
        // "not available" story as an unsupported browser, just server-side.
        setAvailability(err instanceof ApiError && err.status === 404 ? "hub-disabled" : "unsupported");
      }
    }

    void probe();
    return () => {
      cancelled = true;
    };
  }, []);

  async function onToggle(next: boolean) {
    setBusy(true);
    try {
      if (next) {
        if (!vapidPublicKey) throw new Error("push is not configured on this hub");
        const subscriptionJson = await enablePush(vapidPublicKey);
        const record = await subscribePush(subscriptionJson);
        writeSubscriptionId(record.id);
        setEnabled(true);
        toast.show("Push notifications enabled on this device.");
      } else {
        const id = readSubscriptionId();
        await disablePushLocally();
        if (id) {
          try {
            await unsubscribePush(id);
          } catch {
            // The browser-side unsubscribe already happened; a hub that
            // cannot be reached right now will still stop getting 200s from
            // a dead endpoint and clean it up via the 404/410 path.
          }
        }
        writeSubscriptionId(null);
        setEnabled(false);
        toast.show("Push notifications turned off.");
      }
    } catch (err) {
      toast.show(messageOf(err));
    } finally {
      setBusy(false);
    }
  }

  const note = explanation(availability);
  const disabled = availability !== "available" || busy;

  return (
    <div className="mx-auto flex max-w-2xl flex-col gap-4 px-3 py-4 sm:px-4">
      <section className="rounded-md border border-border bg-surface">
        <h2 className="border-b border-border px-3 py-2 text-sm font-semibold">Notifications</h2>
        <div className="flex items-start justify-between gap-3 px-3 py-3">
          <div className="min-w-0">
            <label htmlFor="push-toggle" className="text-sm font-medium">
              Notify me on this device
            </label>
            <p className="mt-1 text-xs text-muted">
              A phone notification when a chat's run finishes or needs your approval.
            </p>
            {note ? <p className="mt-1 text-xs text-warn">{note}</p> : null}
          </div>
          <button
            id="push-toggle"
            type="button"
            role="switch"
            aria-checked={enabled}
            disabled={disabled}
            onClick={() => void onToggle(!enabled)}
            className={[
              "min-h-[44px] shrink-0 rounded-full px-4 text-sm font-medium transition-colors",
              enabled ? "bg-accent text-on-accent" : "bg-raised text-muted",
              disabled ? "cursor-not-allowed opacity-50" : "hover:opacity-90",
            ].join(" ")}
          >
            {enabled ? "On" : "Off"}
          </button>
        </div>
      </section>
    </div>
  );
}
