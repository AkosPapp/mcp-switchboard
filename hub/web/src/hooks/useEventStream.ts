/**
 * The global change feed, wired to the query cache.
 *
 * The feed is lossy by design (spec.md 7.3): a subscriber that falls behind
 * loses events, which is correct because every event is an idempotent
 * notification - "connections changed", not "here is what changed". So the only
 * safe reaction is to invalidate and refetch, never to patch a cache entry from
 * the event's contents.
 */

import { useEffect, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";

import { API_BASE } from "../api/client";
import type { ChangeEvent } from "../api/types";

export type StreamState = "connecting" | "live" | "offline";

export function useEventStream(): StreamState {
  const client = useQueryClient();
  const [state, setState] = useState<StreamState>("connecting");
  // Keeps the effect from re-subscribing on every render while still reading
  // the current client.
  const clientRef = useRef(client);
  clientRef.current = client;

  useEffect(() => {
    if (typeof EventSource === "undefined") {
      setState("offline");
      return;
    }

    const source = new EventSource(`${API_BASE}/events`);

    source.onopen = () => setState("live");

    source.onmessage = (message) => {
      setState("live");
      let event: ChangeEvent;
      try {
        event = JSON.parse(message.data);
      } catch {
        return; // A malformed frame is not worth tearing the stream down for.
      }

      const cache = clientRef.current;
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
        case "chat":
          // A draft may have changed in another tab; the composer decides
          // whether it is safe to show.
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
        default:
          break;
      }
    };

    // EventSource reconnects on its own, so an error is a gap to report rather
    // than something to handle.
    source.onerror = () => setState("offline");

    return () => source.close();
  }, []);

  return state;
}
