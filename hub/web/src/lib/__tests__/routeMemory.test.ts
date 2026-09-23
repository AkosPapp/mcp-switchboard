import { beforeEach, describe, expect, it } from "vitest";

import {
  EMPTY_ROUTE_MEMORY,
  forgetTab,
  loadRouteMemory,
  nextLocationForTab,
  remember,
  restoreTarget,
  saveLocation,
  tabOf,
} from "../routeMemory";

const at = (pathname: string, search = "") => ({ pathname, search });

beforeEach(() => localStorage.clear());

describe("tabOf", () => {
  it("maps paths to their tab", () => {
    expect(tabOf("/chat/abc")).toBe("/chat");
    expect(tabOf("/chat")).toBe("/chat");
    expect(tabOf("/chatty")).toBeNull();
    expect(tabOf("/")).toBeNull();
  });
});

describe("nextLocationForTab", () => {
  const memory = {
    tabs: { "/chat": "/chat/abc", "/connections": "/connections?connection=c&server=s&tool=t" },
    last: null,
  };

  it("goes to the remembered location of another tab", () => {
    expect(nextLocationForTab("/chat", at("/connections"), memory)).toBe("/chat/abc");
    expect(nextLocationForTab("/connections", at("/chat/abc"), memory)).toBe(
      "/connections?connection=c&server=s&tool=t",
    );
  });

  it("goes to the bare root when the tab is already current", () => {
    expect(nextLocationForTab("/chat", at("/chat/abc"), memory)).toBe("/chat");
    expect(nextLocationForTab("/chat", at("/chat"), memory)).toBe("/chat");
  });

  it("goes to the bare tab when nothing is remembered", () => {
    expect(nextLocationForTab("/graph", at("/chat"), memory)).toBe("/graph");
    expect(nextLocationForTab("/graph", at("/chat"), EMPTY_ROUTE_MEMORY)).toBe("/graph");
  });

  it("ignores a remembered location that is not under the tab", () => {
    const bad = { tabs: { "/chat": "/graph/x", "/calls": "//evil.example" }, last: null };
    expect(nextLocationForTab("/chat", at("/calls"), bad)).toBe("/chat");
    expect(nextLocationForTab("/calls", at("/chat"), bad)).toBe("/calls");
  });
});

describe("remember / persistence", () => {
  it("records per tab and drops prefilled args", () => {
    const m = remember(EMPTY_ROUTE_MEMORY, at("/connections", "?tool=t&args=%7B%7D&server=s"));
    expect(m.tabs["/connections"]).toBe("/connections?tool=t&server=s");
    expect(m.last).toBe("/connections?tool=t&server=s");
  });

  it("does not record a location outside the tabs", () => {
    expect(remember(EMPTY_ROUTE_MEMORY, at("/"))).toBe(EMPTY_ROUTE_MEMORY);
  });

  it("saves and loads, and tolerates junk", () => {
    saveLocation(at("/chat/abc"));
    expect(loadRouteMemory().tabs["/chat"]).toBe("/chat/abc");
    localStorage.setItem(
      "mcpsb.ui.v1.route",
      JSON.stringify({ v: 1, d: { tabs: { "/chat": 5, "/graph": "/graph/g" }, last: 7 } }),
    );
    expect(loadRouteMemory()).toEqual({ tabs: { "/graph": "/graph/g" }, last: null });
    localStorage.setItem("mcpsb.ui.v1.route", "garbage");
    expect(loadRouteMemory()).toEqual(EMPTY_ROUTE_MEMORY);
  });

  it("forgets a tab, and last when it pointed there", () => {
    saveLocation(at("/connections"));
    saveLocation(at("/chat/gone"));
    forgetTab("/chat");
    const m = loadRouteMemory();
    expect(m.tabs["/chat"]).toBeUndefined();
    expect(m.tabs["/connections"]).toBe("/connections");
    expect(m.last).toBeNull();
  });
});

describe("restoreTarget", () => {
  it("defaults to connections", () => {
    expect(restoreTarget(EMPTY_ROUTE_MEMORY)).toBe("/connections");
    expect(restoreTarget({ tabs: {}, last: "/nowhere" })).toBe("/connections");
    expect(restoreTarget({ tabs: {}, last: "//evil.example" })).toBe("/connections");
  });

  it("returns the last location", () => {
    expect(restoreTarget({ tabs: {}, last: "/chat/abc" })).toBe("/chat/abc");
  });
});

describe("the Agents tab became Prompts", () => {
  const store = (d: unknown) => localStorage.setItem("mcpsb.ui.v1.route", JSON.stringify({ v: 1, d }));

  it("maps a remembered /agents location onto /prompts, tab key and last included", () => {
    store({ tabs: { "/agents": "/agents/p1", "/chat": "/chat/abc" }, last: "/agents/p1" });
    const memory = loadRouteMemory();
    expect(memory.tabs).toEqual({ "/prompts": "/prompts/p1", "/chat": "/chat/abc" });
    expect(memory.last).toBe("/prompts/p1");
    expect(restoreTarget(memory)).toBe("/prompts/p1");
    expect(nextLocationForTab("/prompts", at("/calls"), memory)).toBe("/prompts/p1");
  });

  it("maps the bare root and leaves lookalikes alone", () => {
    store({ tabs: { "/agents": "/agents" }, last: "/agentsmith" });
    const memory = loadRouteMemory();
    expect(memory.tabs).toEqual({ "/prompts": "/prompts" });
    expect(memory.last).toBe("/agentsmith");
    expect(tabOf("/prompts/x")).toBe("/prompts");
    expect(tabOf("/agents/x")).toBeNull();
  });

  it("rewrites the stored form the next time a location is saved", () => {
    store({ tabs: { "/agents": "/agents/p1" }, last: "/agents/p1" });
    saveLocation(at("/calls"));
    expect(localStorage.getItem("mcpsb.ui.v1.route")).not.toContain("/agents");
  });
});
