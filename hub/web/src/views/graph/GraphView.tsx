import {
  Background,
  Controls,
  ConnectionMode,
  MarkerType,
  MiniMap,
  ReactFlow,
  ReactFlowProvider,
  useReactFlow,
  type Connection,
  type Edge as FlowEdge,
  type EdgeMouseHandler,
  type NodeMouseHandler,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import { useEffect, useMemo, useRef, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";

import { forgetTab } from "../../lib/routeMemory";

import { useConnections } from "../../api/queries";
import { useToast } from "../../components/Toast";
import { useStoredState } from "../../hooks/useStoredState";
import { visibleAgents } from "../../lib/graphFilter";
import { layoutGraph } from "../../lib/graphLayout";
import { isBoolean } from "../../lib/uiMemory";
import { useChats } from "../chat/api";
import { attachChats, connectionsByLabel } from "./chatNodes";
import ChatNode, { type ChatFlowNode } from "./ChatNode";
import ChatPanel from "./ChatPanel";
import FloatingCommunicationEdge from "./FloatingEdge";
import { messageOf, useGraph, useSetEdge } from "./hooks";
import SpawnDialog from "./SpawnDialog";
import { buttonClass } from "./ui";

const NODE_TYPES = { chat: ChatNode };
// Structural edges keep xyflow's built-in smoothstep renderer; communication
// edges use the floating-edge renderer (FloatingEdge.tsx) so a straight line
// between any two nodes - side by side or stacked - always actually touches
// both node rectangles instead of anchoring to a fixed Handle position.
const EDGE_TYPES = { communication: FloatingCommunicationEdge };

/** A clock for the "last activity" ages. Re-renders locally; touches no network. */
function useNow(intervalMs: number): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), intervalMs);
    return () => window.clearInterval(id);
  }, [intervalMs]);
  return now;
}

// Structural (parent -> child) edges are solid, thick tree lines. Communication
// edges (U15/D14: any two chats, symmetric) are drawn as a straight line
// directly between the two nodes instead, and are always dashed so the two
// kinds read apart from each other at a glance, regardless of allowed/denied.
const STRUCTURAL_STYLE = { stroke: "rgb(var(--muted))", strokeWidth: 3 };
const ALLOWED_STYLE = { stroke: "rgb(var(--accent))", strokeWidth: 2, strokeDasharray: "7 4" };
const DENIED_STYLE = { stroke: "rgb(var(--muted))", strokeWidth: 2, strokeDasharray: "2 4" };

function Canvas() {
  const { agentId } = useParams();
  const navigate = useNavigate();
  const toast = useToast();
  const graph = useGraph();
  const setEdge = useSetEdge();
  const flow = useReactFlow();
  const now = useNow(30_000);
  const [spawn, setSpawn] = useState<{ parentId: string } | null>(null);
  // Every chat, archived ones included: a node exists for each execution record that has one.
  const chats = useChats({ includeArchived: true });
  // For the client/host badge on each node (U15 nodes-need-more-info): the same
  // label join ChatThread/ChatList already do between a chat's clientLabel and
  // the live connection that serves it.
  const connections = useConnections();

  const [runningOnly, setRunningOnly] = useStoredState("graph.runningOnly", true, isBoolean);
  // Click-a-node-then-click-another linking (U15 rework): no handle to aim for,
  // just tap the source chat and then the chat to connect it to.
  const [linkMode, setLinkMode] = useState(false);
  const [linkFrom, setLinkFrom] = useState<string | null>(null);

  const view = useMemo(
    () => (graph.data && chats.data ? attachChats(graph.data, chats.data.chats) : null),
    [graph.data, chats.data],
  );

  const visible = useMemo(
    () => (view ? visibleAgents(view.agents, agentId, runningOnly) : null),
    [view, agentId, runningOnly],
  );

  // Edges to hidden agents drop out inside layoutGraph (it draws an edge only
  // when both ends are in the list it is given).
  const layout = useMemo(
    () => (view && visible ? layoutGraph(visible.agents, view.edges) : null),
    [view, visible],
  );

  const connByLabel = useMemo(
    () => connectionsByLabel(connections.data?.connections ?? []),
    [connections.data],
  );

  const nodes: ChatFlowNode[] = useMemo(
    () =>
      (layout?.nodes ?? []).map((n) => ({
        id: n.id,
        type: "chat" as const,
        position: { x: n.x, y: n.y },
        data: {
          agent: n.agent,
          now,
          connection: n.agent.clientLabel ? connByLabel.get(n.agent.clientLabel) : undefined,
          connectionsLoaded: connections.data !== undefined,
          linkModeOn: linkMode,
          isLinkSource: linkFrom === n.id,
        },
        selected: n.id === agentId,
        draggable: false,
      })),
    [layout, now, agentId, connByLabel, connections.data, linkMode, linkFrom],
  );

  const edges: FlowEdge[] = useMemo(
    () =>
      (layout?.edges ?? []).map((e) =>
        e.kind === "structural"
          ? {
              id: e.id,
              source: e.source,
              target: e.target,
              type: "smoothstep",
              style: STRUCTURAL_STYLE,
              selectable: false,
              focusable: false,
              deletable: false,
              ariaLabel: "structural parent to child edge",
              data: { kind: e.kind },
            }
          : {
              id: e.id,
              source: e.source,
              target: e.target,
              // Floating, not the tree's smoothstep/bezier and not a fixed-handle
              // straight edge either: a communication edge runs directly between
              // the two nodes' actual boundaries, any side, computed from where
              // the nodes are (FloatingEdge.tsx), so it never reads as a spawn
              // (parent -> child) relationship (U15/D14) and never shoots past
              // the nodes when they sit side by side at the same tree depth.
              type: "communication",
              style: e.allowed ? ALLOWED_STYLE : DENIED_STYLE,
              markerEnd: {
                type: MarkerType.ArrowClosed,
                color: e.allowed ? "rgb(var(--accent))" : "rgb(var(--muted))",
              },
              interactionWidth: 28,
              deletable: false,
              label: e.allowed ? undefined : "denied",
              ariaLabel: `communication edge ${e.source} to ${e.target}, ${e.allowed ? "allowed" : "denied"}`,
              data: { kind: e.kind, allowed: e.allowed },
            },
      ),
    [layout],
  );

  // Fit on first data, when the picture goes from empty to non-empty, when the
  // filter is toggled, and when a phone's sheet resizes the canvas. Not on every
  // agent or status change: a refit each time would fight the user's pan/zoom.
  const hasNodes = (layout?.nodes.length ?? 0) > 0;
  const hasSelection = Boolean(agentId);
  useEffect(() => {
    if (!hasNodes) return;
    const t = window.setTimeout(() => flow.fitView({ duration: 200, padding: 0.2 }), 50);
    return () => window.clearTimeout(t);
  }, [hasNodes, hasSelection, runningOnly, flow]);

  // Any two chats can be linked now (D14: edges are symmetric, not limited to a
  // subtree), so the only pair that must never get a second, redundant edge is
  // one already joined by the structural (parent -> child) line - in either
  // direction, since a communication edge has no fixed direction.
  const alreadyStructural = (a: string, b: string) =>
    (layout?.edges ?? []).some(
      (e) => e.kind === "structural" && ((e.source === a && e.target === b) || (e.source === b && e.target === a)),
    );

  const link = (from: string, to: string) => {
    if (from === to) return;
    if (alreadyStructural(from, to)) {
      toast.show("They are already linked by the structural edge.");
      return;
    }
    setEdge.mutate(
      { from, to, allowed: true },
      { onError: (error) => toast.show(messageOf(error)) },
    );
  };

  const onNodeClick: NodeMouseHandler = (_e, node) => {
    if (linkMode) {
      if (!linkFrom) {
        setLinkFrom(node.id);
        return;
      }
      if (linkFrom === node.id) {
        setLinkFrom(null);
        return;
      }
      link(linkFrom, node.id);
      setLinkFrom(null);
      return;
    }
    navigate(`/graph/${node.id}`);
  };

  const onEdgeClick: EdgeMouseHandler = (_e, edge) => {
    const data = edge.data as { kind?: string; allowed?: boolean } | undefined;
    if (data?.kind !== "communication") {
      toast.show("The parent-child edge is structural. Delete the sub-chat instead.");
      return;
    }
    setEdge.mutate(
      { from: edge.source, to: edge.target, allowed: !data.allowed },
      { onError: (error) => toast.show(messageOf(error)) },
    );
  };

  // The drag-a-handle gesture still works (ChatNode's handles now cover the
  // whole node), as a second way to make the same connection click-mode makes.
  const onConnect = (c: Connection) => {
    if (!c.source || !c.target) return;
    link(c.source, c.target);
  };

  // A remembered chat that no longer exists: checked once, against the first
  // graph that loads, so a chat spawned a moment ago is never bounced.
  const checked = useRef(false);
  useEffect(() => {
    if (checked.current || !view) return;
    checked.current = true;
    if (agentId && !view.agents.some((a) => a.id === agentId && a.deletedAt === null)) {
      forgetTab("/graph");
      navigate("/graph", { replace: true });
    }
  }, [view, agentId, navigate]);

  if (graph.isPending || chats.isPending) return <p className="p-4 text-sm text-muted">Loading graph…</p>;
  if (graph.isError || chats.isError) {
    return (
      <p role="alert" className="p-4 text-sm text-danger">
        Could not load the graph: {messageOf(graph.error ?? chats.error)}
      </p>
    );
  }

  const data = view!;
  const selected = agentId ? data.agents.find((a) => a.id === agentId && a.deletedAt === null) : undefined;
  const spawnParent = spawn ? (data.agents.find((a) => a.id === spawn.parentId) ?? null) : null;

  return (
    <div className="flex h-full min-h-0 flex-col md:flex-row">
      <div
        className={`relative min-w-0 flex-1 ${selected ? "max-md:h-[34dvh] max-md:flex-none" : "min-h-[50dvh]"}`}
        data-testid="graph-canvas"
      >
        <div className="absolute left-2 top-2 z-10 flex gap-2">
          <div className="flex items-center gap-2 rounded border border-border bg-surface/90 px-2 text-xs">
            <button
              type="button"
              role="switch"
              aria-checked={runningOnly}
              aria-label="Running only"
              data-testid="graph-running-only"
              className={`${buttonClass()} min-h-[44px] md:min-h-[36px]`}
              onClick={() => setRunningOnly(!runningOnly)}
            >
              {runningOnly ? "Running only" : "All chats"}
            </button>
            <span data-testid="graph-counts" className="text-muted">
              {visible!.runningTrees} running
              {runningOnly && ` · ${visible!.hiddenAgents} hidden`}
            </span>
            {visible!.selectedKeptIdle && <span className="text-muted">· not running</span>}
          </div>
          <button
            type="button"
            role="switch"
            aria-checked={linkMode}
            aria-label="Link chats"
            data-testid="graph-link-mode"
            className={`${buttonClass(linkMode ? "primary" : "default")} min-h-[44px] md:min-h-[36px]`}
            onClick={() => {
              setLinkMode(!linkMode);
              setLinkFrom(null);
            }}
          >
            {linkFrom ? "Tap the other chat…" : linkMode ? "Linking (tap two chats)" : "Link chats"}
          </button>
        </div>
        {nodes.length === 0 ? (
          runningOnly && visible!.hiddenAgents > 0 ? (
            <div className="p-4 pt-20 text-sm text-muted">
              <p>No chats are running. Show all chats to browse older ones.</p>
              <button
                type="button"
                className={`${buttonClass()} mt-2 min-h-[44px] md:min-h-[36px]`}
                onClick={() => setRunningOnly(false)}
              >
                Show all chats
              </button>
            </div>
          ) : (
            <p className="p-4 pt-20 text-sm text-muted">
              No chats yet. Start one from the Chat tab.
            </p>
          )
        ) : (
          <ReactFlow
            nodes={nodes}
            edges={edges}
            nodeTypes={NODE_TYPES}
            edgeTypes={EDGE_TYPES}
            onNodeClick={onNodeClick}
            onEdgeClick={onEdgeClick}
            onConnect={onConnect}
            onPaneClick={() => {
              if (linkFrom) {
                setLinkFrom(null);
                return;
              }
              if (agentId) navigate("/graph");
            }}
            connectionMode={ConnectionMode.Loose}
            nodesDraggable={false}
            deleteKeyCode={null}
            fitView
            minZoom={0.2}
            maxZoom={1.6}
            zoomOnPinch
            panOnDrag
          >
            <Background />
            <Controls showInteractive={false} />
            <MiniMap pannable zoomable className="!hidden md:!block" />
          </ReactFlow>
        )}
        <Legend />
      </div>

      {selected && (
        <ChatPanel
          graph={data}
          agentId={selected.id}
          onClose={() => navigate("/graph")}
          onSpawnChild={(id) => setSpawn({ parentId: id })}
          onDeleted={() => navigate("/graph")}
        />
      )}
      {agentId && !selected && (
        <p role="status" className="absolute right-3 top-14 rounded border border-border bg-surface px-3 py-2 text-sm">
          That chat no longer exists.
        </p>
      )}

      {spawn && spawnParent && (
        <SpawnDialog
          parent={spawnParent}
          onClose={() => setSpawn(null)}
          onCreated={(id) => {
            setSpawn(null);
            navigate(`/graph/${id}`);
          }}
        />
      )}
    </div>
  );
}

function Legend() {
  return (
    <div className="pointer-events-none absolute bottom-2 left-2 z-10 hidden rounded border border-border bg-surface/90 px-2 py-1 text-xs text-muted sm:block">
      <span className="mr-3">━ parent → child</span>
      <span className="mr-3 text-accent">╌ allowed connection</span>
      <span>·· denied (click an edge to toggle; Link chats or drag a node to add)</span>
    </div>
  );
}

export default function GraphView() {
  return (
    <ReactFlowProvider>
      <Canvas />
    </ReactFlowProvider>
  );
}
