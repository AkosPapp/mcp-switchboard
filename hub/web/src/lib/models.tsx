import type { ModelInfo } from "../api/types";

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
