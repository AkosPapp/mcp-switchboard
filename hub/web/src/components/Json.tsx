/** Pretty-printed JSON, the way both the call detail and the tool result show
 * a payload. Kept in one place so they cannot drift apart. */
export function pretty(value: unknown): string {
  if (value === undefined) return "";
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return String(value);
  }
}

export default function Json({ value, className = "" }: { value: unknown; className?: string }) {
  return (
    <pre
      className={`overflow-auto rounded border border-border bg-raised p-2 font-mono text-xs leading-relaxed ${className}`}
    >
      {pretty(value)}
    </pre>
  );
}
