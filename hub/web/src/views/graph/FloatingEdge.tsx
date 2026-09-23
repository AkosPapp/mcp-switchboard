import { BaseEdge, getStraightPath, Position, useInternalNode, type EdgeProps, type InternalNode, type Node } from "@xyflow/react";

/**
 * A "floating" edge (xyflow's own documented pattern for exactly this
 * situation): communication edges connect any two chats, so with a
 * dagre-laid-out tree, peers routinely sit side by side at the same depth.
 * A fixed Handle position (Top/Bottom) still anchors the edge at "bottom of
 * A" -> "top of B" in that case, which reads as a diagonal line shooting
 * past both nodes instead of a line between them. Computing each node's
 * actual intersection with the straight line between the two node centers
 * makes the edge always touch both node boundaries, regardless of layout.
 */

/** Where the straight line between the two node centers crosses `node`'s rectangle. */
function getNodeIntersection(intersectionNode: InternalNode<Node>, targetNode: InternalNode<Node>) {
  const { width, height } = intersectionNode.measured;
  const intersectionNodePosition = intersectionNode.internals.positionAbsolute;
  const targetPosition = targetNode.internals.positionAbsolute;

  const w = (width ?? 0) / 2;
  const h = (height ?? 0) / 2;

  const x2 = intersectionNodePosition.x + w;
  const y2 = intersectionNodePosition.y + h;
  const x1 = targetPosition.x + (targetNode.measured.width ?? 0) / 2;
  const y1 = targetPosition.y + (targetNode.measured.height ?? 0) / 2;

  const xx1 = (x1 - x2) / (2 * w) - (y1 - y2) / (2 * h);
  const yy1 = (x1 - x2) / (2 * w) + (y1 - y2) / (2 * h);
  // When the two nodes sit at the exact same center (shouldn't happen - the
  // layout never stacks two live nodes - but division by zero would produce
  // NaN and vanish the edge entirely), fall back to no offset.
  const denom = Math.abs(xx1) + Math.abs(yy1);
  const a = denom === 0 ? 0 : 1 / denom;
  const xx3 = a * xx1;
  const yy3 = a * yy1;
  const x = w * (xx3 + yy3) + x2;
  const y = h * (-xx3 + yy3) + y2;

  return { x, y };
}

/** Which side of `node` the intersection point landed on, for the path's curvature. */
function getEdgePosition(node: InternalNode<Node>, intersectionPoint: { x: number; y: number }) {
  const n = node.internals.positionAbsolute;
  const nx = Math.round(n.x);
  const ny = Math.round(n.y);
  const px = Math.round(intersectionPoint.x);
  const py = Math.round(intersectionPoint.y);
  const width = node.measured.width ?? 0;
  const height = node.measured.height ?? 0;

  if (px <= nx + 1) return Position.Left;
  if (px >= nx + width - 1) return Position.Right;
  if (py <= ny + 1) return Position.Top;
  if (py >= ny + height - 1) return Position.Bottom;
  return Position.Top;
}

export function getEdgeParams(source: InternalNode<Node>, target: InternalNode<Node>) {
  const sourceIntersectionPoint = getNodeIntersection(source, target);
  const targetIntersectionPoint = getNodeIntersection(target, source);

  const sourcePos = getEdgePosition(source, sourceIntersectionPoint);
  const targetPos = getEdgePosition(target, targetIntersectionPoint);

  return {
    sx: sourceIntersectionPoint.x,
    sy: sourceIntersectionPoint.y,
    tx: targetIntersectionPoint.x,
    ty: targetIntersectionPoint.y,
    sourcePos,
    targetPos,
  };
}

export default function FloatingCommunicationEdge({
  id,
  source,
  target,
  markerEnd,
  style,
  label,
  interactionWidth,
}: EdgeProps) {
  const sourceNode = useInternalNode(source);
  const targetNode = useInternalNode(target);

  if (!sourceNode || !targetNode) return null;

  const { sx, sy, tx, ty } = getEdgeParams(sourceNode, targetNode);
  const [edgePath, labelX, labelY] = getStraightPath({
    sourceX: sx,
    sourceY: sy,
    targetX: tx,
    targetY: ty,
  });

  return (
    <BaseEdge
      id={id}
      path={edgePath}
      markerEnd={markerEnd}
      style={style}
      label={label}
      labelX={labelX}
      labelY={labelY}
      interactionWidth={interactionWidth}
    />
  );
}
