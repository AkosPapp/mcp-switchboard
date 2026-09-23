import { vi } from "vitest";

/** A WebSocket the test drives by hand. */
export class FakeWS {
  static instances: FakeWS[] = [];
  static OPEN = 1;
  readyState = 0;
  closed = false;
  sent: unknown[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((event: MessageEvent) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  constructor(public url: string) {
    FakeWS.instances.push(this);
  }
  send(data: string) {
    this.sent.push(JSON.parse(data));
  }
  close() {
    this.closed = true;
    this.readyState = 3;
    this.onclose?.();
  }
  serverOpen() {
    this.readyState = 1;
    this.onopen?.();
  }
  serverSend(message: unknown) {
    this.onmessage?.({ data: JSON.stringify(message) } as MessageEvent);
  }
  serverClose() {
    this.readyState = 3;
    this.onclose?.();
  }
}

export function installFakeWS() {
  FakeWS.instances = [];
  vi.stubGlobal("WebSocket", FakeWS);
}
