/**
 * The console's one live connection to the hub: a WebSocket carrying both the
 * change feed and the open chat's stream (GET /api/ws, spec.md 7.3-7.4).
 *
 * Why not two EventSources per tab: a browser allows about six HTTP/1.1
 * connections per host, streams never end, and a few tabs used them all up;
 * the next tab's ordinary requests then waited forever. WebSockets are outside
 * that pool.
 *
 * The two feeds keep their contracts. Change events are lossy notifications,
 * so after any reconnect listeners are told to refetch everything (`onReopen`).
 * A chat subscription resumes from the last frame it saw, or is told to start
 * over (`overflow`).
 */

import type { ChangeEvent } from "../api/types";

export type SocketState = "connecting" | "live" | "offline";

export type ChatMessage =
  | { kind: "frame"; name: string; id: string; data: unknown }
  | { kind: "overflow"; reason: string };

interface ChatSub {
  chatId: string;
  /** where to resume from; null for a live-only start */
  last: () => string | null;
  handler: (m: ChatMessage) => void;
}

/** The hub sends a heartbeat every 15 s; silence for this long means the link is dead. */
const SILENCE_MS = 45_000;
const BACKOFF_MS = [500, 1000, 2000, 5000, 10_000];

export function socketUrl(): string {
  const url = new URL("api/ws", document.baseURI);
  url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
  return url.toString();
}

export class HubSocket {
  private ws: WebSocket | null = null;
  private state: SocketState = "connecting";
  private everOpened = false;
  private attempt = 0;
  private retry: ReturnType<typeof setTimeout> | undefined;
  private watchdog: ReturnType<typeof setTimeout> | undefined;
  private users = 0;

  private readonly stateListeners = new Set<() => void>();
  private readonly eventListeners = new Set<(e: ChangeEvent) => void>();
  private readonly reopenListeners = new Set<() => void>();
  private readonly subs = new Set<ChatSub>();

  constructor(private readonly url: () => string = socketUrl) {}

  /* ---- state, for useSyncExternalStore ---- */

  getState = (): SocketState => this.state;

  subscribeState = (fn: () => void): (() => void) => {
    this.stateListeners.add(fn);
    return () => this.stateListeners.delete(fn);
  };

  private setState(next: SocketState) {
    if (this.state === next) return;
    this.state = next;
    this.stateListeners.forEach((fn) => fn());
  }

  /* ---- listeners ---- */

  /** Change-feed events. Holding a listener keeps the socket open. */
  onEvent(fn: (e: ChangeEvent) => void): () => void {
    this.eventListeners.add(fn);
    return this.hold(() => this.eventListeners.delete(fn));
  }

  /** Called when the socket is back after a break: whatever was missed is unknown, so refetch. */
  onReopen(fn: () => void): () => void {
    this.reopenListeners.add(fn);
    return () => this.reopenListeners.delete(fn);
  }

  /** Follow a chat's stream until the returned function is called. */
  subscribeChat(chatId: string, last: () => string | null, handler: (m: ChatMessage) => void) {
    const sub: ChatSub = { chatId, last, handler };
    this.subs.add(sub);
    const release = this.hold(() => {
      this.subs.delete(sub);
      this.send({ op: "unsub", chat: chatId });
    });
    this.send({ op: "sub", chat: chatId, last: last() ?? "" });
    return {
      /** After an overflow: subscribe again from the live edge. */
      restart: () => this.send({ op: "sub", chat: chatId, last: "" }),
      close: release,
    };
  }

  /* ---- connection ---- */

  private hold(release: () => void): () => void {
    this.users++;
    this.ensure();
    let done = false;
    return () => {
      if (done) return;
      done = true;
      release();
      if (--this.users === 0) this.shutdown();
    };
  }

  private ensure() {
    if (this.ws || this.retry !== undefined || typeof WebSocket === "undefined") return;
    this.open();
  }

  private open() {
    this.retry = undefined;
    this.setState("connecting");
    let ws: WebSocket;
    try {
      ws = new WebSocket(this.url());
    } catch {
      this.lost();
      return;
    }
    this.ws = ws;
    ws.onopen = () => {
      // "live" waits for the hub's hello, which is sent once its feeds listen.
      this.arm();
    };
    ws.onmessage = (ev) => {
      this.arm();
      let m: { t?: string; chat?: string; name?: string; id?: string; reason?: string; data?: unknown };
      try {
        m = JSON.parse(String(ev.data));
      } catch {
        return;
      }
      switch (m.t) {
        case "hello": {
          this.attempt = 0;
          const reopened = this.everOpened;
          this.everOpened = true;
          this.setState("live");
          for (const s of this.subs) this.send({ op: "sub", chat: s.chatId, last: s.last() ?? "" });
          if (reopened) this.reopenListeners.forEach((fn) => fn());
          break;
        }
        case "event":
          this.eventListeners.forEach((fn) => fn(m.data as ChangeEvent));
          break;
        case "frame":
          for (const s of this.subs) {
            if (s.chatId === m.chat) s.handler({ kind: "frame", name: String(m.name), id: String(m.id ?? ""), data: m.data });
          }
          break;
        case "overflow":
          for (const s of this.subs) {
            if (s.chatId === m.chat) s.handler({ kind: "overflow", reason: m.reason ?? "overflow" });
          }
          break;
        default:
          break; // "hb", or something newer than this console
      }
    };
    ws.onclose = () => {
      if (this.ws === ws) this.lost();
    };
    ws.onerror = () => {
      // onclose follows; nothing to add.
    };
  }

  /** Any traffic proves the link is alive; silence does not. */
  private arm() {
    clearTimeout(this.watchdog);
    this.watchdog = setTimeout(() => this.ws?.close(), SILENCE_MS);
  }

  private lost() {
    clearTimeout(this.watchdog);
    this.ws = null;
    if (this.users === 0) return;
    this.setState("offline");
    const wait = BACKOFF_MS[Math.min(this.attempt++, BACKOFF_MS.length - 1)];
    this.retry = setTimeout(() => this.open(), wait);
  }

  private shutdown() {
    clearTimeout(this.retry);
    clearTimeout(this.watchdog);
    this.retry = undefined;
    const ws = this.ws;
    this.ws = null;
    ws?.close();
    this.everOpened = false;
    this.attempt = 0;
    this.setState("connecting");
  }

  private send(msg: object) {
    if (this.ws?.readyState === WebSocket.OPEN && this.state === "live") this.ws.send(JSON.stringify(msg));
    // Not live yet: the subscription goes out on "hello" with everything else.
  }
}

/** The tab's socket. Tests build their own HubSocket with a fake URL. */
export const hubSocket = new HubSocket();
