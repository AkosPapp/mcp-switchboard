import { useStoredState } from "../../hooks/useStoredState";
import { isBoolean } from "../../lib/uiMemory";
import { useTodos, type TodoStatus } from "./api";

const ICON: Record<TodoStatus, { glyph: string; cls: string; label: string }> = {
  pending: { glyph: "○", cls: "text-muted", label: "pending" },
  in_progress: { glyph: "◐", cls: "text-accent", label: "in progress" },
  completed: { glyph: "✓", cls: "text-ok", label: "completed" },
};

/** The agent's plan (switchboard.todo.write): collapsible, hidden while empty. */
export default function TodoPanel({ chatId }: { chatId: string }) {
  const todos = useTodos(chatId).data ?? [];
  const [open, setOpen] = useStoredState("chat.plan", true, isBoolean);
  if (todos.length === 0) return null;
  const done = todos.filter((t) => t.status === "completed").length;
  const current = todos.find((t) => t.status === "in_progress");
  return (
    <section aria-label="plan" data-testid="todo-panel" className="shrink-0 border-t border-border bg-surface px-3 py-1.5 text-sm">
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setOpen(!open)}
        className="flex w-full items-center gap-2 text-left text-xs text-muted hover:text-text"
      >
        <span aria-hidden="true">{open ? "▾" : "▸"}</span>
        <span className="font-medium">Plan</span>
        <span>
          {done}/{todos.length}
        </span>
        {!open && current ? <span className="min-w-0 flex-1 truncate text-text">{current.content}</span> : null}
      </button>
      {open ? (
        <ul className="mt-1 max-h-40 space-y-0.5 overflow-y-auto">
          {todos.map((t) => (
            <li key={t.id} className="flex items-start gap-2" data-status={t.status}>
              <span aria-label={ICON[t.status].label} role="img" className={`w-4 shrink-0 text-center ${ICON[t.status].cls}`}>
                {ICON[t.status].glyph}
              </span>
              <span className={t.status === "completed" ? "text-muted line-through" : t.status === "in_progress" ? "text-text" : "text-muted"}>
                {t.content}
              </span>
            </li>
          ))}
        </ul>
      ) : null}
    </section>
  );
}
