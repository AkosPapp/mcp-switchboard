/**
 * The per-chat SSE stream (spec.md N4-N8, U3).
 *
 * Deltas live in component state (the reducer in lib/streamAssembly), never in
 * the query cache. What the stream does to the cache is invalidate: a finished
 * message or run means the persisted state changed, so refetch it.
 */

import { useCallback, useEffect, useReducer, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";

import { API_BASE } from "../api/client";
import {
  initialStream,
  parseFrame,
  streamReducer,
  type StreamState,
} from "../lib/streamAssembly";

export type ChatStreamConnection = "connecting" | "live" | "offline";

const FRAME_EVENTS = [
  "run_started",
  "delta",
  "tool_call",
  "tool_result",
  "approval_required",
  "message_done",
  "run_done",
  "error",
];

const RECONNECT_MS = 1500;

export function useChatStream(chatId: string | null): {
  state: StreamState;
  connection: ChatStreamConnection;
  /** Tell the hook the fetched path now holds these message ids. */
  synced: (ids: string[]) => void;
} {
  const client = useQueryClient();
  const clientRef = useRef(client);
  clientRef.current = client;
  const [state, dispatch] = useReducer(streamReducer, initialStream);
  const [connection, setConnection] = useState<ChatStreamConnection>("connecting");
  const lastId = useRef<string | null>(null);
  lastId.current = state.lastEventId;

  useEffect(() => {
    dispatch({ type: "reset" });
    if (!chatId || typeof EventSource === "undefined") {
      setConnection("offline");
      return;
    }

    let source: EventSource | null = null;
    let timer: number | undefined;
    let disposed = false;

    const refetch = () => {
      const cache = clientRef.current;
      cache.invalidateQueries({ queryKey: ["chat", chatId] });
      cache.invalidateQueries({ queryKey: ["chatMessages", chatId] });
    };

    const open = (resumeFrom: string | null) => {
      if (disposed) return;
      setConnection("connecting");
      // A browser reconnect sends Last-Event-ID itself; this explicit query
      // form is only for the manual reconnect after the browser gave up.
      const suffix = resumeFrom ? `?lastEventId=${encodeURIComponent(resumeFrom)}` : "";
      const es = new EventSource(`${API_BASE}/chats/${encodeURIComponent(chatId)}/stream${suffix}`);
      source = es;
      es.onopen = () => setConnection("live");

      for (const name of FRAME_EVENTS) {
        es.addEventListener(name, (ev) => {
          const frame = parseFrame(name, (ev as MessageEvent).data);
          if (!frame) return;
          dispatch({ type: "frame", frame });
          const cache = clientRef.current;
          if (name === "message_done") refetch();
          if (name === "approval_required" || name === "run_started") {
            cache.invalidateQueries({ queryKey: ["run", frame.runId] });
          }
          if (name === "run_done") {
            refetch();
            cache.invalidateQueries({ queryKey: ["run", frame.runId] });
            cache.invalidateQueries({ queryKey: ["chats"] });
            cache.invalidateQueries({ queryKey: ["agent"] });
          }
        });
      }

      es.addEventListener("overflow", (ev) => {
        // Close first: an auto-reconnect would resend the stale Last-Event-ID
        // and loop. Then refetch and resubscribe without a resume position.
        es.close();
        let reason = "overflow";
        try {
          reason = JSON.parse((ev as MessageEvent).data).reason ?? reason;
        } catch {
          // keep the generic reason
        }
        dispatch({ type: "overflow", reason });
        refetch();
        clientRef.current.invalidateQueries({ queryKey: ["run"] });
        timer = window.setTimeout(() => {
          dispatch({ type: "overflow_handled" });
          open(null);
        }, 200);
      });

      es.onerror = () => {
        if (disposed) return;
        setConnection("offline");
        // CLOSED means the browser will not retry (an HTTP error, say).
        if (es.readyState === EventSource.CLOSED) {
          es.close();
          timer = window.setTimeout(() => open(lastId.current), RECONNECT_MS);
        }
      };
    };

    open(null);
    return () => {
      disposed = true;
      window.clearTimeout(timer);
      source?.close();
    };
  }, [chatId]);

  const synced = useCallback((ids: string[]) => dispatch({ type: "synced", ids }), []);
  return { state, connection, synced };
}
