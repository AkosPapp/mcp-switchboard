/** Small formatters for the chat badges. Money is integer micros (L5). */

export function formatCost(micros: number): string {
  if (!micros) return "$0";
  const dollars = micros / 1_000_000;
  return dollars < 0.01 ? `$${dollars.toFixed(4)}` : `$${dollars.toFixed(2)}`;
}

export function formatTokens(n: number): string {
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toFixed(n < 10_000 ? 1 : 0)}k`;
  return `${(n / 1_000_000).toFixed(1)}M`;
}

export function modelLabel(model: { provider?: string; model?: string } | null | undefined): string {
  return model?.model ?? "";
}

/** Text of a message for display and copy: text blocks joined. */
export function messageText(content: { type: string; text?: string }[]): string {
  return content
    .filter((b) => b.type === "text" && typeof b.text === "string")
    .map((b) => b.text)
    .join("\n\n");
}

export function remainingSeconds(expiresAt: string, now: number): number {
  const t = Date.parse(expiresAt);
  if (Number.isNaN(t)) return 0;
  return Math.max(0, Math.ceil((t - now) / 1000));
}

export function clock(seconds: number): string {
  const m = Math.floor(seconds / 60);
  const s = seconds % 60;
  return `${m}:${String(s).padStart(2, "0")}`;
}

/** Strip the \u0002..\u0003 match markers of a search snippet into segments. */
export function snippetParts(snippet: string): { text: string; hit: boolean }[] {
  const parts: { text: string; hit: boolean }[] = [];
  let hit = false;
  for (const piece of snippet.split(/([\u0002\u0003])/)) {
    if (piece === "\u0002") hit = true;
    else if (piece === "\u0003") hit = false;
    else if (piece) parts.push({ text: piece, hit });
  }
  return parts;
}
