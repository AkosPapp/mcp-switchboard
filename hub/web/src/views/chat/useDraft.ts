/**
 * The composer's text, saved on the server as a per-chat draft.
 *
 * Text only: attachments are deliberately not drafted (they can be megabytes
 * and are cheap to re-add). Sending a message clears the draft server-side, so
 * the client never deletes it after a send - it only has to make sure no save
 * of the just-sent text lands afterwards (`beforeSend`).
 */

import { useCallback, useEffect, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import { draftKey, getDraft, putDraft } from "./api";

export type DraftStatus = "idle" | "saving" | "saved" | "error";

export const DRAFT_DEBOUNCE_MS = 600;

export interface UseDraft {
  text: string;
  setText: (value: string) => void;
  status: DraftStatus;
  /** Save a pending edit now (blur). */
  flush: () => void;
  onFocus: () => void;
  onBlur: () => void;
  /** Cancels the pending save and waits for an in-flight one. Call before posting. */
  beforeSend: () => Promise<void>;
  /** After a successful send: empty the box without saving anything. */
  cleared: () => void;
}

export function useDraft(chatId: string): UseDraft {
  const client = useQueryClient();
  const [text, setTextState] = useState("");
  const [status, setStatus] = useState<DraftStatus>("idle");

  const chatRef = useRef(chatId);
  const textRef = useRef("");
  // What the server is known to hold for the current chat.
  const serverRef = useRef("");
  const serverStampRef = useRef<string | null>(null);
  const typedRef = useRef(false);
  const focusedRef = useRef(false);
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const inflightRef = useRef<Promise<void>>(Promise.resolve());
  const failedRef = useRef(false);

  const save = useCallback((id: string, value: string, keepalive: boolean) => {
    if (id === chatRef.current) setStatus("saving");
    const previous = inflightRef.current;
    // Saves of one chat go out in order, so an older text cannot land last.
    const run = previous
      .catch(() => undefined)
      .then(() => putDraft(id, value, keepalive))
      .then(
        (saved) => {
          if (id !== chatRef.current) return;
          serverRef.current = value;
          if (saved?.updatedAt) serverStampRef.current = saved.updatedAt;
          failedRef.current = false;
          // Only claim "saved" when nothing newer is waiting.
          if (timerRef.current === null && textRef.current === value) setStatus("saved");
        },
        () => {
          if (id !== chatRef.current) return;
          // Once per failure streak; the local text is untouched.
          if (!failedRef.current) setStatus("error");
          failedRef.current = true;
        },
      );
    inflightRef.current = run;
  }, []);

  /** Sends a pending edit now. */
  const flushNow = useCallback(
    (keepalive: boolean) => {
      if (timerRef.current === null) return;
      clearTimeout(timerRef.current);
      timerRef.current = null;
      if (textRef.current !== serverRef.current) save(chatRef.current, textRef.current, keepalive);
      else setStatus("saved");
    },
    [save],
  );

  // A different chat: flush the old one (with its own id), reset for the new.
  useEffect(() => {
    chatRef.current = chatId;
    textRef.current = "";
    serverRef.current = "";
    serverStampRef.current = null;
    typedRef.current = false;
    failedRef.current = false;
    inflightRef.current = Promise.resolve();
    setTextState("");
    setStatus("idle");

    const id = chatId;
    return () => {
      if (timerRef.current !== null) {
        clearTimeout(timerRef.current);
        timerRef.current = null;
        if (textRef.current !== serverRef.current) {
          // chatRef still names this chat here; the next effect run moves it.
          void putDraft(id, textRef.current, true).catch(() => undefined);
        }
      }
    };
  }, [chatId]);

  useEffect(() => {
    const onHide = () => flushNow(true);
    window.addEventListener("pagehide", onHide);
    return () => window.removeEventListener("pagehide", onHide);
  }, [flushNow]);

  const query = useQuery({
    queryKey: draftKey(chatId),
    queryFn: () => getDraft(chatId),
    retry: false,
    // Always refetch on open: leaving flushed a save the cache never saw.
    staleTime: 0,
    gcTime: 0,
  });

  useEffect(() => {
    const data = query.data;
    if (!data) return;
    // Never apply an older snapshot than what this tab already saved.
    if (data.updatedAt && serverStampRef.current && data.updatedAt < serverStampRef.current) return;
    if (data.draft === serverRef.current) {
      if (data.updatedAt) serverStampRef.current = data.updatedAt;
      return;
    }
    // Only an untouched textarea (no unsaved local edits) may be refreshed,
    // and never one the user is typing in.
    const clean = textRef.current === serverRef.current;
    if (!clean || (focusedRef.current && typedRef.current)) return;
    serverRef.current = data.draft;
    serverStampRef.current = data.updatedAt;
    textRef.current = data.draft;
    setTextState(data.draft);
  }, [query.data]);

  const setText = useCallback(
    (value: string) => {
      typedRef.current = true;
      textRef.current = value;
      setTextState(value);
      if (timerRef.current !== null) clearTimeout(timerRef.current);
      const id = chatRef.current;
      timerRef.current = setTimeout(() => {
        timerRef.current = null;
        if (id !== chatRef.current) return;
        if (textRef.current !== serverRef.current) save(id, textRef.current, false);
        else setStatus("saved");
      }, DRAFT_DEBOUNCE_MS);
    },
    [save],
  );

  const beforeSend = useCallback(async () => {
    if (timerRef.current !== null) {
      clearTimeout(timerRef.current);
      timerRef.current = null;
    }
    await inflightRef.current.catch(() => undefined);
  }, []);

  const cleared = useCallback(() => {
    if (timerRef.current !== null) {
      clearTimeout(timerRef.current);
      timerRef.current = null;
    }
    // The server dropped the draft when it accepted the message.
    textRef.current = "";
    serverRef.current = "";
    typedRef.current = false;
    setTextState("");
    setStatus("idle");
    client.setQueryData(draftKey(chatRef.current), { draft: "", updatedAt: null });
  }, [client]);

  return {
    text,
    setText,
    status,
    flush: () => flushNow(false),
    onFocus: () => {
      focusedRef.current = true;
    },
    onBlur: () => {
      focusedRef.current = false;
      flushNow(false);
    },
    beforeSend,
    cleared,
  };
}
