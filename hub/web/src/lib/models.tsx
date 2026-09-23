import type { ModelInfo } from "../api/types";
import type { ReactNode } from "react";
import ChoicePicker, { type Choice } from "../components/ChoicePicker";

/** 32768 -> "32k", 131072 -> "128k", 1048576 -> "1M". */
export function formatContext(tokens: number): string {
  if (tokens >= 1024 * 1024) {
    const m = tokens / (1024 * 1024);
    return `${Number.isInteger(m) ? m : m.toFixed(1)}M`;
  }
  if (tokens >= 1024) return `${Math.round(tokens / 1024)}k`;
  return String(tokens);
}

export const modelKey = (m: { provider: string; model: string }) => `${m.provider}/${m.model}`;

/** `gpt-oss:20b · 32k ctx · no tools` (context and marker only when known). */
export function modelOptionLabel(m: ModelInfo): string {
  const parts = [modelKey(m)];
  if (typeof m.contextWindow === "number" && m.contextWindow > 0) {
    parts.push(`${formatContext(m.contextWindow)} ctx`);
  }
  if (m.supportsTools === false) parts.push("no tool support");
  return parts.join(" · ");
}

/** True when the picked model explicitly does not advertise tool calling. */
export function lacksTools(models: ModelInfo[], key: string): boolean {
  return models.some((m) => modelKey(m) === key && m.supportsTools === false);
}

/** The <option>s of a model picker; discovered models are grouped apart when both kinds exist. */
export function ModelOptions({ models }: { models: ModelInfo[] }) {
  const option = (m: ModelInfo) => (
    <option key={modelKey(m)} value={modelKey(m)}>
      {modelOptionLabel(m)}
    </option>
  );
  const discovered = models.filter((m) => m.discovered);
  if (discovered.length === 0 || discovered.length === models.length) return <>{models.map(option)}</>;
  return (
    <>
      <optgroup label="Declared">{models.filter((m) => !m.discovered).map(option)}</optgroup>
      <optgroup label="Discovered">{discovered.map(option)}</optgroup>
    </>
  );
}

/** Warning line shown under a picker whose current choice lacks tool calling. */
export function NoToolsWarning({ models, value }: { models: ModelInfo[]; value: string }) {
  if (!lacksTools(models, value)) return null;
  return (
    <span role="note" data-testid="no-tools-warning" className="mt-1 block text-danger">
      this model does not advertise tool calling
    </span>
  );
}

/** Where a model came from and what it can do, as the muted text of a picker row. */
function modelDetail(m: ModelInfo): string {
  const parts = [m.provider];
  if (typeof m.contextWindow === "number" && m.contextWindow > 0) parts.push(`${formatContext(m.contextWindow)} ctx`);
  if (m.discovered) parts.push("discovered");
  return parts.join(" · ");
}

/**
 * The model picker: a searchable list (fuzzy, like a command palette) in place
 * of a <select>. `value` is a model key ("provider/model") or "" for `emptyLabel`,
 * the "use the default" choice, which is listed first when given.
 */
export function ModelPicker({
  models,
  value,
  onChange,
  emptyLabel,
  ariaLabel,
  className = "",
  disabled,
  children,
}: {
  models: ModelInfo[];
  value: string;
  onChange: (key: string) => void;
  /** e.g. "gpt-oss (chat default)"; omit when a model must be chosen */
  emptyLabel?: string;
  ariaLabel: string;
  className?: string;
  disabled?: boolean;
  children?: ReactNode;
}) {
  const choices: Choice[] = models.map((m) => ({
    value: modelKey(m),
    label: m.model,
    detail: modelDetail(m),
    search: `${m.model} ${m.provider}`,
    badge: m.supportsTools === false ? "no tools" : undefined,
  }));
  if (emptyLabel !== undefined) choices.unshift({ value: "", label: emptyLabel, search: emptyLabel });
  // A value the list does not know (a model since removed) still shows as itself.
  if (value !== "" && !choices.some((c) => c.value === value)) {
    choices.push({ value, label: value.includes("/") ? value.slice(value.indexOf("/") + 1) : value, detail: "not configured", search: value });
  }
  const label = choices.find((c) => c.value === value)?.label ?? value;
  return (
    <ChoicePicker
      value={value}
      choices={choices}
      onChange={onChange}
      ariaLabel={ariaLabel}
      title="Pick a model"
      placeholder="Search models…"
      className={className}
      disabled={disabled}
    >
      {children ?? (
        <>
          <span className="min-w-0 truncate">{label}</span>
          <span aria-hidden="true" className="shrink-0 text-muted">▾</span>
        </>
      )}
    </ChoicePicker>
  );
}
