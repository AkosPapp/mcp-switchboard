import { Background, Handle, Position, ReactFlow, ReactFlowProvider, type Edge, type Node, type NodeProps } from "@xyflow/react";
import { memo, useEffect, useMemo } from "react";
import "@xyflow/react/dist/style.css";

import { buildOverview, OVERVIEW_NODE_HEIGHT, OVERVIEW_NODE_WIDTH, type OverviewNode } from "../../lib/overview";
import { useToast } from "../../components/Toast";
import { patchChat, useInvalidateChat, useMessageTree } from "./api";

type FlowNode = Node<OverviewNode & Record<string, unknown>, "turn">;

const HIDDEN_HANDLE = { opacity: 0, pointerEvents: "none" as const, width: 1, height: 1, minWidth: 1, minHeight: 1 };

const TurnNode = memo(function TurnNode({ data }: NodeProps<FlowNode>) {
  return (
    <div
      data-testid="overview-node"
      data-message-id={data.id}
      data-active={data.active}
      style={{ width: OVERVIEW_NODE_WIDTH, height: OVERVIEW_NODE_HEIGHT }}
      className={`cursor-pointer overflow-hidden rounded border px-2 py-1 text-left ${
        data.active ? "border-accent bg-raised" : "border-border bg-surface opacity-70 hover:opacity-100"
      } ${data.leaf ? "ring-1 ring-accent" : ""}`}
    >
      <Handle type="target" position={Position.Top} style={HIDDEN_HANDLE} isConnectable={false} />
      <p className={`truncate text-[11px] font-semibold ${data.role === "user" ? "text-accent" : "text-ok"}`}>{data.label}</p>
      <p className="line-clamp-2 text-[11px] leading-tight text-muted">{data.snippet}</p>
      <Handle type="source" position={Position.Bottom} style={HIDDEN_HANDLE} isConnectable={false} />
    </div>
  );
});
const nodeTypes = { turn: TurnNode };

function Flow({ chatId, onSelected }: { chatId: string; onSelected?: () => void }) {
  const tree = useMessageTree(chatId);
  const invalidate = useInvalidateChat();
  const toast = useToast();

  const overview = useMemo(
    () => buildOverview(tree.data?.messages ?? [], tree.data?.activeLeafId ?? null),
    [tree.data],
  );
  const nodes = useMemo<FlowNode[]>(
    () => overview.nodes.map((n) => ({ id: n.id, type: "turn", position: { x: n.x, y: n.y }, data: { ...n } })),
    [overview],
  );
  const edges = useMemo<Edge[]>(
    () =>
      overview.edges.map((e) => ({
        id: e.id,
        source: e.source,
        target: e.target,
        style: { stroke: e.active ? "rgb(var(--accent))" : "rgb(var(--border))", strokeWidth: e.active ? 1.5 : 1 },
      })),
    [overview],
  );

  // Refetch on a new message: the thread invalidates the messages key, which
  // covers this query too; this covers a stream that never touched it.
  const count = overview.nodes.length;
  useEffect(() => {
    void tree.refetch();
    // only a change in size should refetch
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [count]);

  if (tree.isError) return <p className="p-3 text-xs text-danger">{(tree.error as Error).message}</p>;
  if (count === 0) return <p className="p-3 text-xs text-muted">No messages yet.</p>;

  return (
    <ReactFlow
      // A different tree size is a different picture: fit it again.
      key={count}
      nodes={nodes}
      edges={edges}
      nodeTypes={nodeTypes}
      fitView
      fitViewOptions={{ padding: 0.1, maxZoom: 1 }}
      minZoom={0.2}
      nodesDraggable={false}
      nodesConnectable={false}
      elementsSelectable={false}
      proOptions={{ hideAttribution: true }}
      onNodeClick={(_, node) => {
        patchChat(chatId, { selectMessageId: node.id }).then(
          () => {
            invalidate(chatId);
            onSelected?.();
          },
          (e) => toast.show(e instanceof Error ? e.message : String(e)),
        );
      }}
    >
      <Background gap={16} size={1} />
    </ReactFlow>
  );
}

/** The message tree as a graph; click a turn to switch the thread to its branch. */
export default function OverviewPanel({ chatId, onSelected }: { chatId: string; onSelected?: () => void }) {
  return (
    <div className="flex h-full min-h-0 flex-col bg-surface" data-testid="overview-panel">
      <p className="border-b border-border px-3 py-1.5 text-xs text-muted">Overview · click a turn to open its branch</p>
      <div className="min-h-0 flex-1">
        <ReactFlowProvider>
          <Flow chatId={chatId} onSelected={onSelected} />
        </ReactFlowProvider>
      </div>
    </div>
  );
}
