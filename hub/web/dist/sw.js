// The console's service worker (spec.md U61).
//
// Deliberately minimal: its only job today is to exist, so the console is
// installable and (once wired up separately, spec.md U63/§8.7) can receive
// push. It does NOT cache or serve API responses - chats, the graph and calls
// are realtime, and a service worker answering a fetch with yesterday's data
// would be actively wrong, not just stale. The only thing worth caching is the
// static JS/CSS bundle, and even that is skipped here: Vite fingerprints every
// asset by content hash and the hub already serves those with a one-year
// immutable Cache-Control (hub/internal/console/console.go), so the browser's
// own HTTP cache already does this job without the double bookkeeping (and
// staleness risk) of a second cache the service worker would have to version
// and evict itself.
//
// A plain, hand-written file rather than a generated one, so a `push` and
// `notificationclick` handler can be added below later without restructuring
// anything else (the registration in src/pwa/registerServiceWorker.ts already
// points at this exact file).

self.addEventListener("install", () => {
  // Take over immediately rather than waiting for every open tab to close;
  // there is no cached version of anything to be careful about invalidating.
  self.skipWaiting();
});

self.addEventListener("activate", (event) => {
  event.waitUntil(self.clients.claim());
});

// No `fetch` handler: leaving it unregistered means every request just goes to
// the network as if there were no service worker at all, which is exactly the
// behaviour realtime data needs.

// push: the payload is exactly {title, body, chatId, kind} (spec.md 8.7).
self.addEventListener("push", (event) => {
  let payload = {};
  try {
    payload = event.data ? event.data.json() : {};
  } catch {
    // Not JSON - show something rather than throwing away a real push.
    payload = { title: "mcp-switchboard", body: event.data ? event.data.text() : "" };
  }

  const title = payload.title || "mcp-switchboard";
  const options = {
    body: payload.body || "",
    // Tagging by chatId means a second notification for the same chat
    // replaces the first on the device's notification tray instead of
    // piling up, which matters for "run finished" pushes on a chat that
    // gets several runs in a row.
    tag: payload.chatId ? `chat-${payload.chatId}` : undefined,
    data: payload,
  };

  event.waitUntil(self.registration.showNotification(title, options));
});

// notificationclick: focus an existing console tab on that chat if one is
// open, otherwise open a new one.
self.addEventListener("notificationclick", (event) => {
  event.notification.close();

  const chatId = event.notification.data && event.notification.data.chatId;
  const targetPath = chatId ? `/chat/${chatId}` : "/";
  const targetUrl = new URL(targetPath, self.registration.scope).href;

  event.waitUntil(
    self.clients
      .matchAll({ type: "window", includeUncontrolled: true })
      .then((clients) => {
        for (const client of clients) {
          if ("focus" in client) {
            const focused = client.focus();
            if ("navigate" in client) {
              return Promise.resolve(focused).then(() => client.navigate(targetUrl));
            }
            return focused;
          }
        }
        if (self.clients.openWindow) {
          return self.clients.openWindow(targetUrl);
        }
        return undefined;
      }),
  );
});
