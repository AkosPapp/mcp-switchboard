/**
 * The per-chat stream (spec.md N4-N8, U3), carried by the tab's WebSocket
 * (lib/hubSocket).
 *
 * Deltas live in component state (the reducer in lib/streamAssembly), never in
 * the query cache. What the stream does to the cache is invalidate: a finished
 * message or run means the persisted state changed, so refetch it.
 */

import { useCallback, useEffect, useReducer, useRef, useSyncExternalStore } from "react";
import { useQueryClient } from "@tanstack/react-query";

import { hubSocket, type ChatMessage, type HubSocket } from "../lib/hubSocket";
import { initialStream, streamReducer, toFrame, type StreamState } from "../lib/streamAssembly";

export type ChatStreamConnection = "connecting" | "live" | "offline";

export function useChatStream(
  chatId: string | null,
  socket: HubSocket = hubSocket,
): {
  state: StreamState;
  connection: ChatStreamConnection;
  /** Tell the hook the fetched path now holds these message ids. */
  synced: (ids: string[]) => void;
} {
  const client = useQueryClient();
  const clientRef = useRef(client);
  clientRef.current = client;
  const [state, dispatch] = useReducer(streamReducer, initialStream);
  const lastId = useRef<string | null>(null);
  lastId.current = state.lastEventId;
  const socketState = useSyncExternalStore(socket.subscribeState, socket.getState);

  useEffect(() => {
    dispatch({ type: "reset" });
    if (!chatId) return;
    let timer: ReturnType<typeof setTimeout> | undefined;

    const refetch = () => {
      const cache = clientRef.current;
      cache.invalidateQueries({ queryKey: ["chat", chatId] });
      cache.invalidateQueries({ queryKey: ["chatMessages", chatId] });
    };

    const sub = socket.subscribeChat(chatId, () => lastId.current, (m: ChatMessage) => {
      if (m.kind === "overflow") {
        // The hub ended this subscription: what was assembled is unreliable.
        // Refetch, then follow the live edge again with no resume position.
        dispatch({ type: "overflow", reason: m.reason });
        refetch();
        clientRef.current.invalidateQueries({ queryKey: ["run"] });
        timer = setTimeout(() => {
          dispatch({ type: "overflow_handled" });
          sub.restart();
        }, 200);
        return;
      }
      const frame = toFrame(m.name, m.data);
      if (!frame) return;
      dispatch({ type: "frame", frame });
      const cache = clientRef.current;
      if (m.name === "message_done") refetch();
      if (m.name === "approval_required" || m.name === "run_started") {
        cache.invalidateQueries({ queryKey: ["run", frame.runId] });
      }
      if (m.name === "run_done") {
        refetch();
        cache.invalidateQueries({ queryKey: ["run", frame.runId] });
        cache.invalidateQueries({ queryKey: ["chats"] });
        cache.invalidateQueries({ queryKey: ["agent"] });
      }
    });
    return () => {
      clearTimeout(timer);
      sub.close();
    };
  }, [chatId, socket]);

  const synced = useCallback((ids: string[]) => dispatch({ type: "synced", ids }), []);
  return { state, connection: chatId ? socketState : "offline", synced };
}
