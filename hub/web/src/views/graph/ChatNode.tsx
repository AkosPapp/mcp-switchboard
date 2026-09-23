import { Handle, Position, type Node, type NodeProps } from "@xyflow/react";
import { memo } from "react";

import type { ConnectionInfo } from "../../api/types";
import ClientBadge from "../../components/ClientBadge";
import {
  formatAge,
  formatCost,
  formatTokens,
  modelLabel,
  NODE_HEIGHT,
  NODE_WIDTH,
} from "../../lib/graphLayout";
import type { GraphChat } from "./chatNodes";
import { Chip, StatusDot } from "./ui";

export interface ChatNodeData extends Record<string, unknown> {
  agent: GraphChat;
  now: number;
  /** The live connection behind `agent.clientLabel`, when one is still open. */
  connection?: ConnectionInfo;
  /** Connections have loaded at least once, so "no match" can mean "offline". */
  connectionsLoaded?: boolean;
  /** U15 rework: link mode is on, so the whole node is a connect target/source. */
  linkModeOn?: boolean;
  /** This node is the pending "from" end of a link being made by click-click. */
  isLinkSource?: boolean;
}

export type ChatFlowNode = Node<ChatNodeData, "chat">;

/**
 * Handles stay for drag-to-connect, but they no longer are the only way in:
 * they now cover the whole node so a drag can start (or land) anywhere on it,
 * not just on a small dot (U15). `connectOnClick` (on by default) also gives
 * click-a-node-then-click-another as a second gesture through the same
 * elements; GraphView additionally offers an explicit "Link chats" mode built
 * on plain node clicks for anyone who finds drag fiddly on a phone.
 *
 * `!transform-none` matters as much as the sizing classes: xyflow's own
 * `.react-flow__handle-top`/`-bottom` rules set `transform: translate(-50%,
 * ...50%)` to center the normal small dot on its anchor point. Left in place,
 * that transform is still resolved against this now-full-node-sized handle's
 * own box, so it shoves the "invisible full-node" handle away from the node
 * entirely (each one offset by half the node's own size) instead of merely
 * recentering a dot - leaving a real gap over the node where no handle
 * receives the pointer at all, so a drag starting there falls through to the
 * pane and pans the canvas instead of starting a connection. (A previous
 * attempt at this used `!translate-0`, which is not a real Tailwind utility -
 * it compiles to no CSS rule at all - so it silently left that transform in
 * place.)
 */
const HANDLE = "!inset-0 !h-full !w-full !transform-none !rounded-md !border-0 !bg-transparent !opacity-0";

function ChatNodeView({ data, selected }: NodeProps<ChatFlowNode>) {
  const { agent, now, connection, connectionsLoaded, linkModeOn, isLinkSource } = data;
  const running = agent.status === "running";
  return (
    <div
      data-testid={`graph-node-${agent.id}`}
      aria-label={`chat ${agent.name}, ${agent.status}`}
      style={{ width: NODE_WIDTH, height: NODE_HEIGHT }}
      className={[
        "relative flex flex-col gap-1 overflow-hidden rounded-md border bg-surface px-3 py-2 text-left shadow-sm",
        isLinkSource
          ? "border-ok ring-2 ring-ok"
          : selected
            ? "border-accent ring-2 ring-accent"
            : "border-border",
        linkModeOn && !isLinkSource ? "border-dashed" : "",
        running ? "animate-pulse" : "",
      ].join(" ")}
    >
      <Handle type="target" position={Position.Top} className={HANDLE} />
      <div className="flex items-center gap-2">
        <StatusDot status={agent.status} />
        <span className="min-w-0 flex-1 truncate text-sm font-semibold">{agent.name}</span>
        {agent.unreadMail > 0 && (
          <span
            className="rounded-full bg-accent px-1.5 text-xs font-medium text-bg"
            title={`${agent.unreadMail} unread mailbox message(s)`}
            aria-label={`${agent.unreadMail} unread`}
          >
            {agent.unreadMail}
          </span>
        )}
      </div>
      {agent.clientLabel && (
        <div className="min-w-0 text-xs">
          <ClientBadge
            label={agent.clientLabel}
            environment={connection?.client.environment}
            connected={connectionsLoaded ? connection !== undefined : undefined}
            compact
          />
        </div>
      )}
      <div className="flex items-center gap-1.5">
        <Chip title="model">{modelLabel(agent.model)}</Chip>
        <Chip title="depth">d{agent.depth}</Chip>
        <span className="ml-auto text-xs text-muted" title={agent.lastActivityAt}>
          {formatAge(agent.lastActivityAt, now)}
        </span>
      </div>
      <div className="flex items-center gap-2 text-xs text-muted">
        <span>{formatTokens(agent.tokenTotal)} tok</span>
        <span>{formatCost(agent.costTotalMicros)}</span>
        <span className="ml-auto">{agent.status}</span>
      </div>
      {running && agent.currentTool ? (
        <div className="truncate font-mono text-xs text-ok" title={agent.currentTool}>
          {agent.currentTool}
        </div>
      ) : null}
      <Handle type="source" position={Position.Bottom} className={HANDLE} />
    </div>
  );
}

export default memo(ChatNodeView);
