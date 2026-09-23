import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router-dom";

import { renderView } from "../../__tests__/helpers";
import { NODE_HEIGHT, NODE_WIDTH } from "../../../lib/graphLayout";
import GraphView from "../GraphView";
import { agentFixture, edgeFixture, graphFixture } from "../fixtures";
import { connectionFixture, stubGraphHub } from "./helpers";

// jsdom has no ResizeObserver, which @xyflow/react uses both to size the
// canvas and to "measure" each node before it will draw edges to/from it.
// Every chat node is the same fixed NODE_WIDTH x NODE_HEIGHT (graphLayout.ts),
// so reporting that size for whatever gets observed is enough to unblock edge
// rendering without a real layout engine.
class StubResizeObserver {
  #cb: ResizeObserverCallback;
  constructor(cb: ResizeObserverCallback) {
    this.#cb = cb;
  }
  observe(target: Element) {
    this.#cb(
      [{ target, contentRect: { width: NODE_WIDTH, height: NODE_HEIGHT } } as ResizeObserverEntry],
      this as unknown as ResizeObserver,
    );
  }
  unobserve() {}
  disconnect() {}
}
// jsdom also has no DOMMatrixReadOnly, which the same measuring path reads for
// the current transform; xyflow only reads .m22 (the scale) off it here.
class StubDOMMatrixReadOnly {
  m22 = 1;
  constructor(_init?: string) {}
}

beforeEach(() => {
  vi.stubGlobal("ResizeObserver", StubResizeObserver);
  vi.stubGlobal("DOMMatrixReadOnly", StubDOMMatrixReadOnly);
});
afterEach(() => vi.unstubAllGlobals());

// jsdom never lays anything out, so offsetWidth/offsetHeight are always 0;
// xyflow treats a zero-sized node as unmeasured and never gives it handle
// bounds, so no edge to or from it is ever drawn (see updateNodeInternals'
// `dimensions.width && dimensions.height` guard). Every chat node here is the
// same fixed size, so reporting that size for every element is enough.
let offsetWidth: PropertyDescriptor | undefined;
let offsetHeight: PropertyDescriptor | undefined;
beforeAll(() => {
  offsetWidth = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetWidth");
  offsetHeight = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetHeight");
  Object.defineProperty(HTMLElement.prototype, "offsetWidth", { configurable: true, get: () => NODE_WIDTH });
  Object.defineProperty(HTMLElement.prototype, "offsetHeight", { configurable: true, get: () => NODE_HEIGHT });
});
afterAll(() => {
  if (offsetWidth) Object.defineProperty(HTMLElement.prototype, "offsetWidth", offsetWidth);
  if (offsetHeight) Object.defineProperty(HTMLElement.prototype, "offsetHeight", offsetHeight);
});

/**
 * a1 is root, a2 is its structural child (as graphFixture already wires up).
 * a3 is a second, unrelated root - so any edge between a1/a3 or a2/a3 is a
 * communication edge (D14: any two chats, not just within a subtree), never a
 * structural one.
 */
/**
 * Real GraphView reads the selected chat off the route (`useParams`), so the
 * canvas is rendered through the same "/graph" and "/graph/:agentId" routes
 * the app uses, rather than as a bare element - otherwise navigate() would
 * change the URL but `agentId` would never update.
 */
function renderGraph(entry = "/graph") {
  return renderView(
    <Routes>
      <Route path="/graph" element={<GraphView />} />
      <Route path="/graph/:agentId" element={<GraphView />} />
    </Routes>,
    entry,
  );
}

function threeChats() {
  return graphFixture({
    agents: [
      agentFixture({ id: "a1", name: "root", clientLabel: "laptop", status: "running" }),
      agentFixture({ id: "a2", parentId: "a1", name: "worker", depth: 1, status: "running" }),
      agentFixture({ id: "a3", name: "other root", status: "running" }),
    ],
    edges: [edgeFixture("a1", "a2")],
  });
}

describe("Graph nodes show client info", () => {
  it("shows the host label, project and environment chip from the live connection", async () => {
    stubGraphHub(threeChats(), undefined, [
      connectionFixture({ label: "laptop", client: { name: "c", version: "1", instance: "i", label: "laptop", environment: { kinds: ["direnv"], project: "agent-reverse-proxy" } } }),
    ]);
    renderGraph();

    const badge = await screen.findByTestId("client-badge");
    expect(badge.textContent).toContain("laptop");
    expect(badge.textContent).toContain("agent-reverse-proxy");
    expect(badge.querySelector('[data-kind="direnv"]')).toBeTruthy();
  });

  it("renders nothing extra for a chat with no client", async () => {
    stubGraphHub(threeChats(), undefined, []);
    renderGraph();
    await screen.findByTestId("graph-node-a1");
    // a2 and a3 have no clientLabel: only a1's badge should exist.
    expect(screen.getAllByTestId("client-badge")).toHaveLength(1);
  });
});

describe("Graph edges", () => {
  it("draws the structural edge as a solid smoothstep and a communication edge straight and dashed", async () => {
    const graph = threeChats();
    graph.edges.push(edgeFixture("a1", "a3", true));
    const { container } = stubAndRender(graph);
    await screen.findByTestId("graph-node-a3");

    await waitFor(() => {
      expect(container.querySelector(".react-flow__edge-smoothstep")).toBeTruthy();
      // "communication" is FloatingEdge.tsx, registered under that edge type
      // name (not xyflow's built-in "straight") so a side-by-side pair of
      // chats gets a line that actually touches both nodes instead of
      // anchoring to a fixed Handle position; xyflow still classes it
      // react-flow__edge-<type> for any registered type, built-in or custom.
      expect(container.querySelector(".react-flow__edge-communication")).toBeTruthy();
    });
    const structural = container.querySelector(".react-flow__edge-smoothstep")!;
    const communication = container.querySelector(".react-flow__edge-communication")!;

    const commPath = communication!.querySelector<SVGPathElement>("path.react-flow__edge-path");
    expect(commPath?.style.strokeDasharray).toBeTruthy();
    const structPath = structural!.querySelector<SVGPathElement>("path.react-flow__edge-path");
    expect(structPath?.style.strokeDasharray).toBeFalsy();
  });

  function stubAndRender(graph: ReturnType<typeof threeChats>) {
    stubGraphHub(graph, undefined, []);
    return renderGraph();
  }
});

describe("Linking chats without a handle", () => {
  it("connects two unrelated chats by tapping Link chats, then each node in turn", async () => {
    const { calls } = stubGraphHub(threeChats(), undefined, []);
    renderGraph();

    await screen.findByTestId("graph-node-a1");
    await userEvent.click(screen.getByRole("switch", { name: "Link chats" }));
    fireEvent.click(screen.getByTestId("graph-node-a1"));
    fireEvent.click(screen.getByTestId("graph-node-a3"));

    await waitFor(() => expect(calls.some((c) => c.method === "PUT" && c.path === "graph/edges/a1/a3")).toBe(true));
    const put = calls.find((c) => c.method === "PUT" && c.path === "graph/edges/a1/a3")!;
    expect(put.body).toEqual({ allowed: true });
    // Clicking nodes while linking must not have navigated to open the panel.
    expect(screen.queryByTestId("graph-panel")).toBeNull();
  });

  it("does not navigate on a plain node click when link mode is off", async () => {
    stubGraphHub(threeChats(), undefined, []);
    renderGraph();
    await screen.findByTestId("graph-node-a1");
    fireEvent.click(screen.getByTestId("graph-node-a1"));
    expect(await screen.findByTestId("graph-panel")).toBeTruthy();
  });
});
