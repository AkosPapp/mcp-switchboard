/**
 * Which chats need the user right now (spec.md U33): the data, the
 * notifications and the tab-title count. The state machine lives in
 * lib/attention; this wires it to the query cache, the router and the browser.
 */

import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from "react";

import {
  AttentionTracker,
  loadSeen,
  notifyPermission,
  requestNotifyPermission,
  saveSeen,
  showNotification,
  titleWithCount,
  type Mark,
  type NotifyPermission,
} from "../lib/attention";
import { useAgents } from "../api/resources";
import { useApprovals, useChats } from "../views/chat/api";

export interface Attention {
  /** chat id -> why it needs the user */
  marks: Record<string, Mark>;
  count: number;
  permission: NotifyPermission;
  /** must be called from a click: browsers refuse the prompt otherwise */
  enableNotifications: () => Promise<void>;
}

const NONE: Attention = { marks: {}, count: 0, permission: "unsupported", enableNotifications: async () => {} };
export const AttentionContext = createContext<Attention>(NONE);
export const useAttention = () => useContext(AttentionContext);

const pageVisible = () => document.visibilityState === "visible" && document.hasFocus();

export function useAttentionState(
  enabled: boolean,
  openChatId: string | null,
  onOpenChat: (chatId: string) => void,
): Attention {
  const approvals = useApprovals(enabled);
  const chats = useChats({}, enabled);
  const agents = useAgents(enabled);
  const tracker = useRef<AttentionTracker | null>(null);
  tracker.current ??= new AttentionTracker(loadSeen());
  const [marks, setMarks] = useState<Record<string, Mark>>({});
  const [visible, setVisible] = useState(pageVisible);
  const [permission, setPermission] = useState<NotifyPermission>(notifyPermission);
  const open = useRef(onOpenChat);
  open.current = onOpenChat;

  useEffect(() => {
    const sync = () => setVisible(pageVisible());
    document.addEventListener("visibilitychange", sync);
    window.addEventListener("focus", sync);
    window.addEventListener("blur", sync);
    return () => {
      document.removeEventListener("visibilitychange", sync);
      window.removeEventListener("focus", sync);
      window.removeEventListener("blur", sync);
    };
  }, []);

  const chatList = chats.data?.chats;
  useEffect(() => {
    if (!enabled) return;
    const out = tracker.current!.update({
      approvals: approvals.data,
      chats: chatList,
      agents: agents.data,
      openChatId,
      visible,
    });
    setMarks((previous) => (sameMarks(previous, out.marks) ? previous : out.marks));
    if (chatList) saveSeen(tracker.current!.snapshot(), chatList.map((c) => c.id));
    for (const m of out.notify) showNotification(m, (id) => open.current(id));
  }, [enabled, approvals.data, chatList, agents.data, openChatId, visible]);

  const count = Object.keys(marks).length;
  useEffect(() => {
    const base = document.title;
    document.title = titleWithCount(base, count);
    return () => {
      document.title = titleWithCount(base, 0);
    };
  }, [count]);

  const enableNotifications = useCallback(async () => setPermission(await requestNotifyPermission()), []);
  return useMemo(() => ({ marks, count, permission, enableNotifications }), [marks, count, permission, enableNotifications]);
}

function sameMarks(a: Record<string, Mark>, b: Record<string, Mark>) {
  const ka = Object.keys(a);
  return ka.length === Object.keys(b).length && ka.every((k) => b[k] && a[k].key === b[k].key && a[k].reason === b[k].reason);
}
