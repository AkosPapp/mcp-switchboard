import "@testing-library/jest-dom/vitest";

// jsdom has no EventSource, and the console opens one at mount. A stub keeps
// every component test from having to think about the stream.
class StubEventSource {
  onopen: (() => void) | null = null;
  onmessage: ((event: MessageEvent) => void) | null = null;
  onerror: (() => void) | null = null;
  addEventListener() {}
  close() {}
}

if (typeof globalThis.EventSource === "undefined") {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  (globalThis as any).EventSource = StubEventSource;
}
