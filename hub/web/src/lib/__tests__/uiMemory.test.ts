import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { forgetMemory, isString, readMemory, writeMemory } from "../uiMemory";

beforeEach(() => localStorage.clear());
afterEach(() => vi.restoreAllMocks());

describe("uiMemory", () => {
  it("round-trips a value under a versioned key", () => {
    writeMemory("a", { x: 1 });
    expect(localStorage.getItem("mcpsb.ui.v1.a")).toBe('{"v":1,"d":{"x":1}}');
    expect(readMemory("a", (v) => v as { x: number }, { x: 0 })).toEqual({ x: 1 });
  });

  it("falls back on a missing key", () => {
    expect(readMemory("nope", isString, "dflt")).toBe("dflt");
  });

  it("falls back on corrupt JSON", () => {
    localStorage.setItem("mcpsb.ui.v1.a", "{not json");
    expect(readMemory("a", isString, "dflt")).toBe("dflt");
  });

  it("falls back on a version mismatch or a bare value", () => {
    localStorage.setItem("mcpsb.ui.v1.a", '{"v":2,"d":"new"}');
    expect(readMemory("a", isString, "dflt")).toBe("dflt");
    localStorage.setItem("mcpsb.ui.v1.a", '"bare"');
    expect(readMemory("a", isString, "dflt")).toBe("dflt");
    localStorage.setItem("mcpsb.ui.v1.a", "null");
    expect(readMemory("a", isString, "dflt")).toBe("dflt");
  });

  it("falls back when the value fails validation", () => {
    localStorage.setItem("mcpsb.ui.v1.a", '{"v":1,"d":42}');
    expect(readMemory("a", isString, "dflt")).toBe("dflt");
  });

  it("never throws when storage throws", () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("denied");
    });
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("quota");
    });
    vi.spyOn(Storage.prototype, "removeItem").mockImplementation(() => {
      throw new Error("denied");
    });
    expect(readMemory("a", isString, "dflt")).toBe("dflt");
    expect(() => writeMemory("a", "x")).not.toThrow();
    expect(() => forgetMemory("a")).not.toThrow();
  });

  it("never throws when the localStorage accessor itself throws", () => {
    vi.spyOn(globalThis, "localStorage", "get").mockImplementation(() => {
      throw new Error("SecurityError");
    });
    expect(readMemory("a", isString, "dflt")).toBe("dflt");
    expect(() => writeMemory("a", "x")).not.toThrow();
  });

  it("forgets a key", () => {
    writeMemory("a", "x");
    forgetMemory("a");
    expect(readMemory("a", isString, "dflt")).toBe("dflt");
  });
});
