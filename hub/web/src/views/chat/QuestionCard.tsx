import { useEffect, useState } from "react";

import { clock, remainingSeconds } from "./format";
import type { Question } from "./types";

interface Picked {
  options: string[];
  other: string;
}

/** What the user has answered so far: chosen options, then their own words. */
const answerOf = (p: Picked): string[] => [...p.options, p.other.trim()].filter(Boolean);

/** The model asked the user something and its run is waiting (switchboard.user.ask). */
export default function QuestionCard({
  questions,
  expiresAt,
  onAnswer,
  onSkip,
  busy = false,
}: {
  questions: Question[];
  expiresAt: string;
  onAnswer: (answers: string[][]) => void | Promise<void>;
  onSkip: () => void | Promise<void>;
  busy?: boolean;
}) {
  const [picked, setPicked] = useState<Picked[]>(() => questions.map(() => ({ options: [], other: "" })));
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(id);
  }, []);
  const left = remainingSeconds(expiresAt, now);

  const update = (i: number, next: Partial<Picked>) =>
    setPicked((all) => all.map((p, j) => (j === i ? { ...p, ...next } : p)));
  const toggle = (i: number, q: Question, label: string) => {
    const p = picked[i];
    if (q.multiSelect) {
      update(i, { options: p.options.includes(label) ? p.options.filter((x) => x !== label) : [...p.options, label] });
    } else {
      // Single choice: this option replaces the last one and any typed answer.
      update(i, { options: p.options[0] === label ? [] : [label], other: "" });
    }
  };
  const complete = picked.every((p) => answerOf(p).length > 0);

  return (
    <form
      role="group"
      aria-label="question from the model"
      data-testid="question-card"
      className="my-2 space-y-4 rounded border border-accent bg-surface p-3 text-sm"
      onSubmit={(e) => {
        e.preventDefault();
        if (complete && !busy) void onAnswer(picked.map(answerOf));
      }}
    >
      {questions.map((q, i) => (
        <fieldset key={i} className="space-y-1.5">
          <legend className="mb-1 flex flex-wrap items-baseline gap-2">
            {q.header ? <span className="rounded bg-accent/15 px-1.5 py-0.5 text-[11px] font-medium text-accent">{q.header}</span> : null}
            <span className="font-medium">{q.question}</span>
          </legend>
          {q.multiSelect ? <p className="text-xs text-muted">Pick any that apply.</p> : null}
          {(q.options ?? []).map((o) => {
            const on = picked[i].options.includes(o.label);
            return (
              <label
                key={o.label}
                className={`flex min-h-[44px] cursor-pointer items-start gap-2 rounded border px-2 py-1.5 md:min-h-0 ${
                  on ? "border-accent bg-raised" : "border-border hover:bg-raised"
                }`}
              >
                <input
                  type={q.multiSelect ? "checkbox" : "radio"}
                  name={`q${i}`}
                  className="mt-0.5"
                  checked={on}
                  onChange={() => toggle(i, q, o.label)}
                  onClick={() => {
                    // A radio cannot be unchecked by clicking it; let a second click clear it.
                    if (!q.multiSelect && on) toggle(i, q, o.label);
                  }}
                />
                <span>
                  {o.label}
                  {o.description ? <span className="block text-xs text-muted">{o.description}</span> : null}
                </span>
              </label>
            );
          })}
          <input
            aria-label={`your own answer to: ${q.question}`}
            placeholder={q.options?.length ? "Something else…" : "Your answer"}
            value={picked[i].other}
            onChange={(e) => update(i, q.multiSelect ? { other: e.target.value } : { other: e.target.value, options: [] })}
            className="min-h-[44px] w-full rounded border border-border bg-bg px-2 py-1 placeholder:text-muted focus:border-accent md:min-h-0"
          />
        </fieldset>
      ))}
      <div className="flex flex-wrap items-center gap-2">
        <button
          type="submit"
          disabled={!complete || busy}
          className="min-h-[44px] rounded bg-accent px-3 py-1.5 font-medium text-on-accent disabled:opacity-40 sm:min-h-0"
        >
          Send answer{questions.length > 1 ? "s" : ""}
        </button>
        <button
          type="button"
          disabled={busy}
          onClick={() => void onSkip()}
          className="min-h-[44px] rounded border border-border px-3 py-1.5 sm:min-h-0"
        >
          Skip
        </button>
        <span className="ml-auto text-xs text-muted" aria-live="off">
          {left > 0 ? `waits ${clock(left)}` : "expired"}
        </span>
      </div>
    </form>
  );
}
