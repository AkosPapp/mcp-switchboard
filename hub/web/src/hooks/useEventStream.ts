/**
 * The global change feed, wired to the query cache.
 *
 * The feed is lossy by design (spec.md 7.3): a subscriber that falls behind
 * loses events, which is correct because every event is an idempotent
 * notification - "connections changed", not "here is what changed". So the only
 * safe reaction is to invalidate and refetch, never to patch a cache entry from
 * the event's contents. The same goes for a reconnect: whatever happened while
 * the socket was down is unknown, so everything is refetched.
 */

import { useEffect, useRef, useSyncExternalStore } from "react";
import { useQueryClient, type QueryClient } from "@tanstack/react-query";

import type { ChangeEvent } from "../api/types";
import { hubSocket, type HubSocket, type SocketState } from "../lib/hubSocket";

export type StreamState = SocketState;

/** What an event makes stale. */
export function applyEvent(cache: QueryClient, event: ChangeEvent) {
  // What a chat can call depends on all of these.
  if (["connections", "agent", "graph", "profile"].includes(event.type)) {
    cache.invalidateQueries({ queryKey: ["chatTools"] });
    cache.invalidateQueries({ queryKey: ["chatSystemPrompt"] });
  }
  switch (event.type) {
    case "connections":
      // The endpoint list is derived from what is connected, so it moves
      // with the registry.
      cache.invalidateQueries({ queryKey: ["connections"] });
      cache.invalidateQueries({ queryKey: ["endpoints"] });
      break;
    case "call":
      cache.invalidateQueries({ queryKey: ["calls"] });
      cache.invalidateQueries({ queryKey: ["stats"] });
      break;
    case "agent":
    case "graph":
      // Approvals and the "new reply" signal follow the run's status (U33).
      if (event.type === "agent") {
        cache.invalidateQueries({ queryKey: ["approvals"] });
        cache.invalidateQueries({ queryKey: ["chats"] });
      }
      // The graph view refetches /api/graph; there is no TTL cache (U17).
      cache.invalidateQueries({ queryKey: ["graph"] });
      cache.invalidateQueries({ queryKey: ["agents"] });
      break;
    case "draft":
      // Saved in some tab (maybe this one). The composer decides whether it is
      // safe to show; nothing else about the chat changed.
      if (event.chatId) cache.invalidateQueries({ queryKey: ["chatDraft", event.chatId] });
      break;
    case "chat":
      // A draft may have been cleared by a send in another tab; the composer
      // decides whether it is safe to show.
      if (event.chatId) {
        cache.invalidateQueries({ queryKey: ["chatDraft", event.chatId] });
        // Another chat may have injected a message into this one.
        cache.invalidateQueries({ queryKey: ["chatMessages", event.chatId] });
        cache.invalidateQueries({ queryKey: ["chat", event.chatId] });
      }
      cache.invalidateQueries({ queryKey: ["approvals"] });
      cache.invalidateQueries({ queryKey: ["chats"] });
      break;
    case "profile":
      cache.invalidateQueries({ queryKey: ["profiles"] });
      break;
    case "skill":
      cache.invalidateQueries({ queryKey: ["skills"] });
      cache.invalidateQueries({ queryKey: ["chatSystemPrompt"] });
      cache.invalidateQueries({ queryKey: ["chatTools"] });
      break;
    default:
      break;
  }
}

export function useEventStream(socket: HubSocket = hubSocket): StreamState {
  const client = useQueryClient();
  // Keeps the effect from re-subscribing on every render while still reading
  // the current client.
  const clientRef = useRef(client);
  clientRef.current = client;
  const state = useSyncExternalStore(socket.subscribeState, socket.getState);

  useEffect(() => {
    const offEvent = socket.onEvent((event) => applyEvent(clientRef.current, event));
    const offReopen = socket.onReopen(() => void clientRef.current.invalidateQueries());
    return () => {
      offEvent();
      offReopen();
    };
  }, [socket]);

  return state;
}
