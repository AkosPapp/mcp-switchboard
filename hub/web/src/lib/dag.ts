/**
 * Navigation over a chat's message DAG (spec.md A4-A6, U8).
 *
 * Messages form a tree by `parentId`; the chat shows one root-to-leaf path.
 * The hub does the authoritative work (PATCH selectMessageId re-points the
 * chat at the deepest previously-active leaf); this module is what the
 * console needs to render and to predict that without a round trip.
 */

export interface DagNode {
  id: string;
  parentId: string | null;
  lastActiveChildId?: string | null;
  siblings?: { ids: string[]; index: number };
}

export interface Tree<T extends DagNode> {
  byId: Map<string, T>;
  /** children by parent id ("" for roots), in the order given (creation). */
  children: Map<string, T[]>;
}

export function indexTree<T extends DagNode>(messages: T[]): Tree<T> {
  const byId = new Map<string, T>();
  const children = new Map<string, T[]>();
  for (const m of messages) byId.set(m.id, m);
  for (const m of messages) {
    // An orphan (parent not in the set) is treated as a root rather than lost.
    const key = m.parentId !== null && byId.has(m.parentId) ? m.parentId : "";
    const list = children.get(key);
    if (list) list.push(m);
    else children.set(key, [m]);
  }
  return { byId, children };
}

/** Root-first path ending at `leafId`; empty if the leaf is unknown. Cycle safe. */
export function pathTo<T extends DagNode>(tree: Tree<T>, leafId: string | null): T[] {
  const out: T[] = [];
  const seen = new Set<string>();
  let cursor = leafId !== null ? tree.byId.get(leafId) : undefined;
  while (cursor && !seen.has(cursor.id)) {
    seen.add(cursor.id);
    out.push(cursor);
    cursor = cursor.parentId !== null ? tree.byId.get(cursor.parentId) : undefined;
  }
  return out.reverse();
}

/** Siblings of a message, from the tree (creation order). */
export function siblingsOf<T extends DagNode>(tree: Tree<T>, id: string) {
  const node = tree.byId.get(id);
  if (!node) return { ids: [id], index: 0 };
  const key = node.parentId !== null && tree.byId.has(node.parentId) ? node.parentId : "";
  const ids = (tree.children.get(key) ?? [node]).map((m) => m.id);
  return { ids, index: Math.max(0, ids.indexOf(id)) };
}

/**
 * The siblings to show for a message: the hub's own answer when it sent one,
 * else computed from a tree, else "only child".
 */
export function resolveSiblings<T extends DagNode>(node: T, tree?: Tree<T>) {
  if (node.siblings && node.siblings.ids.length > 0) return node.siblings;
  if (tree) return siblingsOf(tree, node.id);
  return { ids: [node.id], index: 0 };
}

/** The message a `‹`/`›` press selects, or null at either end. */
export function siblingTarget(
  siblings: { ids: string[]; index: number },
  delta: -1 | 1,
): string | null {
  const next = siblings.index + delta;
  if (next < 0 || next >= siblings.ids.length) return null;
  return siblings.ids[next];
}

/** Deepest leaf below `fromId`, following lastActiveChildId, else the newest child. */
export function deepestLeaf<T extends DagNode>(tree: Tree<T>, fromId: string): string {
  let cursor = tree.byId.get(fromId);
  const seen = new Set<string>();
  while (cursor && !seen.has(cursor.id)) {
    seen.add(cursor.id);
    const kids = tree.children.get(cursor.id) ?? [];
    if (kids.length === 0) return cursor.id;
    const preferred =
      (cursor.lastActiveChildId && kids.find((k) => k.id === cursor!.lastActiveChildId)) ||
      kids[kids.length - 1];
    cursor = preferred;
  }
  return fromId;
}

/** The leaf a sibling selection would land on, computed locally. */
export function selectLeaf<T extends DagNode>(tree: Tree<T>, messageId: string): string {
  return deepestLeaf(tree, messageId);
}

/**
 * Merge a persisted message (a `message_done` frame) into the shown path.
 *
 * Replaces by id; otherwise the message hangs off its parent, which drops
 * anything the path had below that parent (a regenerate replaces the old
 * answer in view). A message whose parent is not on the path cannot be placed
 * and leaves the path alone: the caller refetches.
 */
export function applyToPath<T extends DagNode>(path: T[], message: T): T[] {
  const existing = path.findIndex((m) => m.id === message.id);
  if (existing >= 0) {
    const next = path.slice();
    next[existing] = { ...message, siblings: message.siblings ?? path[existing].siblings };
    return next;
  }
  if (message.parentId === null) return path.length === 0 ? [message] : path;
  const parent = path.findIndex((m) => m.id === message.parentId);
  if (parent < 0) return path;
  return [...path.slice(0, parent + 1), message];
}
