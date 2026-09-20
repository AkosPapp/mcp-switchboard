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
        default:
          // agent/graph/chat belong to the orchestrator, which this console
          // does not show yet.
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
