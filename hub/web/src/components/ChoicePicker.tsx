import { useEffect, useId, useMemo, useRef, useState, type ReactNode } from "react";

import { fuzzyFilter, fuzzyMatch } from "../lib/fuzzy";

export interface Choice {
  value: string;
  label: string;
  /** muted text after the label (a provider, a size) */
  detail?: string;
  /** what the query is matched against; defaults to the label and detail */
  search?: string;
  /** a short marker at the row's right edge */
  badge?: string;
}

/** The label with the characters the query matched in bold. */
function Highlighted({ text, query }: { text: string; query: string }) {
  const match = query.trim() ? fuzzyMatch(query, text) : null;
  if (!match || match.positions.length === 0) return <>{text}</>;
  const hit = new Set(match.positions);
  const out: ReactNode[] = [];
  let run = "";
  let runHit = false;
  const flush = () => {
    if (!run) return;
    out.push(runHit ? <b key={out.length} className="font-semibold text-accent">{run}</b> : run);
    run = "";
  };
  [...text].forEach((ch, i) => {
    const h = hit.has(i);
    if (h !== runHit) flush();
    runHit = h;
    run += ch;
  });
  flush();
  return <>{out}</>;
}

/**
 * A button that opens a search-as-you-type list, like the command palette:
 * type to fuzzy-filter, arrows to move, Enter to pick, Esc to close. It stands
 * in for a <select> wherever the list is long enough to be worth searching.
 */
export default function ChoicePicker({
  value,
  choices,
  onChange,
  ariaLabel,
  title,
  placeholder = "Type to search…",
  className = "",
  disabled = false,
  children,
}: {
  value: string;
  choices: Choice[];
  onChange: (value: string) => void;
  ariaLabel: string;
  /** hover text for the trigger, and the dialog's heading */
  title?: string;
  placeholder?: string;
  /** classes for the trigger button */
  className?: string;
  disabled?: boolean;
  /** replaces the trigger's default content (the current choice's label) */
  children?: ReactNode;
}) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [active, setActive] = useState(0);
  const trigger = useRef<HTMLButtonElement>(null);
  const list = useRef<HTMLUListElement>(null);
  const listId = useId();

  const current = choices.find((c) => c.value === value);
  const shown = useMemo(
    () => fuzzyFilter(choices, query, (c) => c.search ?? `${c.label} ${c.detail ?? ""}`),
    [choices, query],
  );

  const close = () => {
    setOpen(false);
    trigger.current?.focus();
  };
  const pick = (c: Choice) => {
    setOpen(false);
    trigger.current?.focus();
    if (c.value !== value) onChange(c.value);
  };
  const show = () => {
    setQuery("");
    setActive(Math.max(0, choices.findIndex((c) => c.value === value)));
    setOpen(true);
  };

  // Keep the highlighted row in view as the arrows move it.
  useEffect(() => {
    if (!open) return;
    list.current?.children[active]?.scrollIntoView?.({ block: "nearest" });
  }, [open, active]);

  return (
    <>
      <button
        ref={trigger}
        type="button"
        aria-label={ariaLabel}
        aria-haspopup="dialog"
        aria-expanded={open}
        title={title}
        disabled={disabled}
        onClick={show}
        className={className}
      >
        {children ?? current?.label ?? value}
      </button>
      {open ? (
        <div className="fixed inset-0 z-50 flex items-start justify-center px-3 pt-[12vh]" data-testid="choice-picker">
          <div className="absolute inset-0 bg-black/50" onClick={close} aria-hidden="true" />
          <div
            role="dialog"
            aria-modal="true"
            aria-label={title ?? ariaLabel}
            className="relative w-full max-w-lg overflow-hidden rounded border border-border bg-surface shadow-lg"
          >
            <input
              autoFocus
              role="combobox"
              aria-expanded="true"
              aria-controls={listId}
              aria-activedescendant={shown[active] ? `${listId}-${active}` : undefined}
              aria-label={`search: ${ariaLabel}`}
              value={query}
              placeholder={placeholder}
              onChange={(e) => {
                setQuery(e.target.value);
                setActive(0);
              }}
              onKeyDown={(e) => {
                if (e.key === "Escape") {
                  e.preventDefault();
                  e.stopPropagation();
                  close();
                } else if (e.key === "ArrowDown" || (e.ctrlKey && e.key === "n")) {
                  e.preventDefault();
                  setActive((a) => (shown.length ? (a + 1) % shown.length : 0));
                } else if (e.key === "ArrowUp" || (e.ctrlKey && e.key === "p")) {
                  e.preventDefault();
                  setActive((a) => (shown.length ? (a + shown.length - 1) % shown.length : 0));
                } else if (e.key === "Enter") {
                  e.preventDefault();
                  if (shown[active]) pick(shown[active]);
                }
              }}
              className="min-h-[44px] w-full border-b border-border bg-bg px-3 py-2 text-sm placeholder:text-muted focus:outline-none md:min-h-0"
            />
            <ul id={listId} ref={list} role="listbox" aria-label={ariaLabel} className="max-h-[50vh] overflow-y-auto py-1">
              {shown.length === 0 ? <li className="px-3 py-2 text-sm text-muted">No match.</li> : null}
              {shown.map((c, i) => (
                <li
                  key={c.value}
                  id={`${listId}-${i}`}
                  role="option"
                  aria-selected={c.value === value}
                  onMouseEnter={() => setActive(i)}
                  // mousedown, not click: the input must not blur (and re-render the list) first.
                  onMouseDown={(e) => {
                    e.preventDefault();
                    pick(c);
                  }}
                  className={`flex min-h-[44px] cursor-pointer items-baseline gap-2 px-3 py-1.5 text-sm md:min-h-0 md:py-1 ${
                    i === active ? "bg-raised" : ""
                  }`}
                >
                  <span className="shrink-0">
                    <Highlighted text={c.label} query={query} />
                  </span>
                  {c.detail ? <span className="min-w-0 flex-1 truncate text-xs text-muted">{c.detail}</span> : <span className="flex-1" />}
                  {c.value === value ? <span aria-hidden="true" className="shrink-0 text-accent">✓</span> : null}
                  {c.badge ? <span className="shrink-0 text-[11px] text-warn">{c.badge}</span> : null}
                </li>
              ))}
            </ul>
          </div>
        </div>
      ) : null}
    </>
  );
}
