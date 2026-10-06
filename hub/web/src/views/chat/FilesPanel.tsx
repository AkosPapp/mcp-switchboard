import { useStoredState } from "../../hooks/useStoredState";
import { isBoolean } from "../../lib/uiMemory";
import type { Message } from "./types";

/** Files the agent touched, newest activity last. */
export interface TouchedFile {
  path: string;
  /** write | edit | move | delete */
  op: string;
}

const WRITE = ["file_write", "write_file", "edit_file", "file_edit", "patch", "file_patch", "create_file"];
const MOVE = ["file_move", "move_file", "dir_move", "rename_file"];
const DEL = ["file_delete", "delete_file", "remove_file", "dir_remove", "dir_delete"];

/** An MCP tool is surfaced to a chat as `label__server__tool`; match the tail. */
const isKind = (name: string, kinds: string[]) => {
  const leaf = name.includes("__") ? name.slice(name.lastIndexOf("__") + 2) : name;
  return kinds.includes(leaf);
};

/**
 * Derive the changed-files list from the transcript's tool calls (I14, the
 * OpenCode left bar's second half). Pure so it can be tested; unknown
 * argument shapes are simply skipped, and a file that is later deleted keeps
 * its latest state (so the list reads as the story of the work).
 */
export function touchedFiles(messages: readonly Message[]): TouchedFile[] {
  const byPath = new Map<string, TouchedFile>();
  for (const m of messages) {
    if (m.role !== "assistant" || !m.toolCalls) continue;
    for (const call of m.toolCalls) {
      const args = (call.arguments ?? {}) as Record<string, unknown>;
      const str = (v: unknown) => (typeof v === "string" && v ? v : "");
      if (isKind(call.name, WRITE)) {
        const p = str(args.path) || str(args.file_path) || str(args.file);
        if (p) byPath.set(p, { path: p, op: byPath.get(p) ? "edit" : "write" });
      } else if (isKind(call.name, MOVE)) {
        const from = str(args.src) || str(args.source) || str(args.from) || str(args.path);
        const to = str(args.dst) || str(args.destination) || str(args.to) || str(args.new_path);
        if (from) byPath.delete(from);
        if (to) byPath.set(to, { path: to, op: "move" });
      } else if (isKind(call.name, DEL)) {
        const p = str(args.path) || str(args.file_path) || str(args.src);
        if (p) byPath.set(p, { path: p, op: "delete" });
      }
    }
  }
  return [...byPath.values()];
}

const OP_LABEL: Record<string, string> = { write: "new", edit: "edit", move: "moved", delete: "gone" };

/** The chat's changed files (from tool calls), collapsible, hidden while empty. */
export default function FilesPanel({ messages }: { messages: readonly Message[] }) {
  const files = touchedFiles(messages);
  const [open, setOpen] = useStoredState("chat.files", true, isBoolean);
  if (files.length === 0) return null;
  return (
    <section aria-label="files" data-testid="files-panel" className="shrink-0 border-t border-border bg-surface px-3 py-1.5 text-sm">
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setOpen(!open)}
        className="flex w-full items-center gap-2 text-left text-xs text-muted hover:text-text"
      >
        <span aria-hidden="true">{open ? "▾" : "▸"}</span>
        <span className="font-medium">Files</span>
        <span>{files.length}</span>
      </button>
      {open ? (
        <ul className="mt-1 max-h-40 space-y-0.5 overflow-y-auto">
          {files.map((f) => (
            <li key={f.path} className="flex items-center gap-2 text-xs" data-op={f.op}>
              <span className="w-10 shrink-0 text-muted">{OP_LABEL[f.op] ?? f.op}</span>
              <span className="min-w-0 flex-1 truncate font-mono text-text" title={f.path}>
                {f.path}
              </span>
            </li>
          ))}
        </ul>
      ) : null}
    </section>
  );
}
