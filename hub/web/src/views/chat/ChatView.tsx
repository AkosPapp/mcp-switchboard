import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";

import { ApiError } from "../../api/client";
import { AttentionContext, useAttentionState } from "../../hooks/useAttention";
import { forgetTab } from "../../lib/routeMemory";
import { useChat, useInvalidateChat } from "./api";

import ChatDialog from "./ChatDialog";
import ChatList from "./ChatList";
import ChatThread from "./ChatThread";

/**
 * Sizes the view to the space under the app header in dvh, so the composer is
 * pinned to the bottom of the visible viewport rather than of the document
 * (spec.md U2). Measured rather than assumed because the header height varies.
 */
function useFillViewport() {
  const ref = useRef<HTMLDivElement>(null);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const fit = () => {
      const top = el.getBoundingClientRect().top + window.scrollY;
      el.style.height = `calc(100dvh - ${Math.round(top)}px)`;
    };
    fit();
    window.addEventListener("resize", fit);
    return () => window.removeEventListener("resize", fit);
  }, []);
  return ref;
}

export default function ChatView() {
  const { chatId } = useParams();
  const navigate = useNavigate();
  const invalidate = useInvalidateChat();
  // A chat that was remembered (or linked) and has since been deleted: forget
  // it and show the bare list instead of an error over an empty thread.
  const chat = useChat(chatId ?? null);
  const gone = chat.error instanceof ApiError && chat.error.status === 404;
  useEffect(() => {
    if (!gone) return;
    forgetTab("/chat");
    navigate("/chat", { replace: true });
  }, [gone, navigate]);
  const ref = useFillViewport();
  // The new-chat form: shown full-size in the main panel (not a popup), so it
  // reads as "the actual conversation panel" rather than a floating dialog.
  // null = not composing; otherwise the client the new chat starts with.
  const [creating, setCreating] = useState<{ client: string | null } | null>(null);
  // Navigating to a different (or no) chat leaves the compose flow.
  useEffect(() => setCreating(null), [chatId]);

  // Below md the list is a slide-over; with nothing to show in the main panel
  // (no chat chosen and not composing one) it is the page instead.
  const [drawer, setDrawer] = useState(false);
  const listOpen = drawer || (!chatId && !creating);
  // ChatView only ever mounts once the orchestrator is confirmed on (App.tsx's
  // orchestratorOnly), so attention tracking can run unconditionally here.
  const attention = useAttentionState(true, chatId ?? null, (id) => navigate(`/chat/${id}`));

  return (
    <AttentionContext.Provider value={attention}>
      <div ref={ref} className="relative flex min-h-0 overflow-hidden" data-testid="chat-view">
        {listOpen ? (
          <div
            className="fixed inset-0 z-20 bg-text/40 md:hidden"
            onClick={() => setDrawer(false)}
            aria-hidden="true"
            hidden={!chatId}
          />
        ) : null}
        <aside
          aria-label="chats"
          className={`${
            listOpen ? "translate-x-0" : "-translate-x-full"
          } absolute inset-y-0 left-0 z-30 w-[88%] max-w-sm border-r border-border transition-transform md:static md:z-auto md:w-80 md:max-w-none md:shrink-0 md:translate-x-0`}
        >
          <ChatList
            activeId={chatId ?? null}
            onNavigate={() => setDrawer(false)}
            onNewChat={(client) => {
              setCreating({ client });
              setDrawer(false);
            }}
          />
        </aside>
        <section className="min-w-0 flex-1">
          {creating ? (
            <ChatDialog
              key={creating.client ?? "none"}
              variant="panel"
              defaultClient={creating.client}
              onClose={() => setCreating(null)}
              onSaved={(chat) => {
                setCreating(null);
                invalidate();
                setDrawer(false);
                navigate(`/chat/${chat.id}`);
              }}
            />
          ) : chatId ? (
            <ChatThread key={chatId} chatId={chatId} onOpenList={() => setDrawer(true)} />
          ) : (
            <div className="hidden h-full items-center justify-center p-6 text-sm text-muted md:flex">
              Select a chat, or start a new one.
            </div>
          )}
        </section>
      </div>
    </AttentionContext.Provider>
  );
}
