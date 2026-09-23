import { useState } from "react";
import { Link } from "react-router-dom";

import { useQueryClient } from "@tanstack/react-query";

import { useToast } from "../../components/Toast";
import { ancestry, formatAge, formatCost, formatTokens, modelLabel } from "../../lib/graphLayout";
import { deleteChat } from "../chat/api";
import type { GraphChatView } from "./chatNodes";
import ChatSettings from "./ChatSettings";
import { messageOf } from "./hooks";
import PermissionsList from "./PermissionsList";
import { buttonClass, Chip, StatusDot } from "./ui";

/** Descendants (transitively) of `id`, for the delete confirmation. */
function countDescendants(graph: GraphChatView, id: string): number {
  const children = new Map<string, string[]>();
  for (const a of graph.agents) {
    if (a.parentId && a.deletedAt === null) children.set(a.parentId, [...(children.get(a.parentId) ?? []), a.id]);
  }
  let n = 0;
  const walk = (x: string) => {
    for (const c of children.get(x) ?? []) {
      n++;
      walk(c);
    }
  };
  walk(id);
  return n;
}

/**
 * The detail panel: a side panel from md up, a bottom sheet below it (U13).
 * The placement is pure CSS so the same tree serves both.
 */
export default function ChatPanel({
  graph,
  agentId,
  onClose,
  onSpawnChild,
  onDeleted,
}: {
  graph: GraphChatView;
  agentId: string;
  onClose: () => void;
  onSpawnChild: (parentId: string) => void;
  onDeleted: () => void;
}) {
  const toast = useToast();
  const client = useQueryClient();
  const [deleting, setDeleting] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const agent = graph.agents.find((a) => a.id === agentId);
  if (!agent) return null;

  const path = ancestry(graph.agents, agent.id);
  const descendants = countDescendants(graph, agent.id);

  return (
    <aside
      aria-label={`Chat ${agent.name}`}
      data-testid="graph-panel"
      className={[
        "z-30 flex flex-col overflow-hidden border-border bg-surface",
        // Bottom sheet on phones, side panel from md.
        "fixed inset-x-0 bottom-0 max-h-[62dvh] rounded-t-lg border-t shadow-2xl",
        "md:static md:max-h-none md:w-[26rem] md:shrink-0 md:rounded-none md:border-l md:border-t-0 md:shadow-none",
      ].join(" ")}
      style={{ paddingBottom: "env(safe-area-inset-bottom)" }}
    >
      <header className="flex items-center gap-2 border-b border-border px-4 py-2">
        <StatusDot status={agent.status} />
        <h2 className="min-w-0 flex-1 truncate text-base font-semibold">{agent.name}</h2>
        <button
          type="button"
          onClick={onClose}
          aria-label="Close panel"
          className="flex min-h-[44px] min-w-[44px] items-center justify-center rounded text-lg hover:bg-raised"
        >
          ×
        </button>
      </header>

      <div className="min-h-0 flex-1 divide-y divide-border overflow-y-auto">
        <section className="space-y-2 px-4 py-3 text-sm">
          <div className="flex flex-wrap items-center gap-1.5">
            <Chip>{modelLabel(agent.model)}</Chip>
            <Chip>depth {agent.depth}</Chip>
            <Chip>{agent.status}</Chip>
            {agent.project && <Chip>project {agent.project}</Chip>}
            <Chip>active {formatAge(agent.lastActivityAt)}</Chip>
          </div>
          <div className="text-xs text-muted">
            {formatTokens(agent.tokenTotal)} tokens · {formatCost(agent.costTotalMicros)} (this chat and
            its sub-chats) · {agent.unreadMail} unread
          </div>
          {path.length > 1 && (
            <div className="text-xs text-muted">{path.map((p) => p.name).join(" › ")}</div>
          )}
          <div className="flex flex-wrap gap-2 pt-1">
            <Link to={`/chat/${agent.chatId}`} className={`${buttonClass("primary")} inline-flex items-center`}>
              Open chat
            </Link>
            <button type="button" className={buttonClass()} onClick={() => onSpawnChild(agent.id)}>
              Spawn sub-chat
            </button>
          </div>
        </section>

        <PermissionsList key={`perm-${agent.id}`} agent={agent} graph={graph} />
        <ChatSettings key={`set-${agent.id}`} agent={agent} />

        <section className="space-y-2 px-4 py-3">
          <h3 className="text-sm font-semibold">Delete</h3>
          {!confirmDelete ? (
            <button type="button" className={buttonClass("danger")} onClick={() => setConfirmDelete(true)}>
              Delete chat…
            </button>
          ) : (
            <div role="alertdialog" aria-label="Confirm delete" className="rounded border border-danger p-3 text-sm">
              <p>
                Delete <strong>{agent.name}</strong>
                {descendants > 0 ? (
                  <>
                    {" "}
                    and its {descendants} sub-chat{descendants === 1 ? "" : "s"}
                  </>
                ) : null}
                ? Their runs are cancelled.
              </p>
              <div className="mt-2 flex gap-2">
                <button
                  type="button"
                  className={buttonClass("danger")}
                  disabled={deleting}
                  onClick={() => {
                    setDeleting(true);
                    deleteChat(agent.chatId)
                      .then(() => {
                        for (const key of ["graph", "agents", "chats"]) client.invalidateQueries({ queryKey: [key] });
                        setConfirmDelete(false);
                        onDeleted();
                      })
                      .catch((error) => toast.show(messageOf(error)))
                      .finally(() => setDeleting(false));
                  }}
                >
                  Confirm delete
                </button>
                <button type="button" className={buttonClass()} onClick={() => setConfirmDelete(false)}>
                  Cancel
                </button>
              </div>
            </div>
          )}
        </section>
      </div>
    </aside>
  );
}
