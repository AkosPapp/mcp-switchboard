/**
 * Fuzzy matching for pickers (the ctrl+shift+p kind): the query's characters
 * must appear in the text in order, and matches score higher when they are
 * consecutive, start a word, or start the text. Case-insensitive.
 */

export interface FuzzyMatch {
  score: number;
  /** indexes into the text of the matched characters, ascending */
  positions: number[];
}

const isWordStart = (text: string, i: number): boolean => {
  if (i === 0) return true;
  const prev = text[i - 1];
  const cur = text[i];
  if (!/[a-z0-9]/i.test(prev)) return true;
  return prev === prev.toLowerCase() && cur !== cur.toLowerCase(); // camelCase hump
};

/** Null when `query` is not a subsequence of `text`; an empty query matches everything. */
export function fuzzyMatch(query: string, text: string): FuzzyMatch | null {
  const q = query.replace(/\s+/g, "").toLowerCase();
  if (q === "") return { score: 0, positions: [] };
  const t = text.toLowerCase();
  const n = q.length;
  const m = t.length;
  if (n > m) return null;

  // best[i][j]: best score with q[i] matched at t[j]; from[i][j]: where q[i-1] was.
  const NEG = -Infinity;
  const best: number[][] = Array.from({ length: n }, () => new Array<number>(m).fill(NEG));
  const from: number[][] = Array.from({ length: n }, () => new Array<number>(m).fill(-1));
  for (let i = 0; i < n; i++) {
    let runningBest = NEG; // best[i-1][k] for k < j - 1, with its index
    let runningAt = -1;
    for (let j = 0; j < m; j++) {
      if (i > 0 && j >= 2 && best[i - 1][j - 2] > runningBest) {
        runningBest = best[i - 1][j - 2];
        runningAt = j - 2;
      }
      if (t[j] !== q[i]) continue;
      const bonus = (isWordStart(text, j) ? 4 : 0) + (j === 0 ? 3 : 0);
      if (i === 0) {
        best[i][j] = 1 + bonus - Math.min(j, 8) * 0.1; // an earlier start reads as a better match
        continue;
      }
      // Consecutive with the previous match, or a gap (gaps cost a little).
      const adjacent = best[i - 1][j - 1] === NEG || j === 0 ? NEG : best[i - 1][j - 1] + 1 + 5 + bonus;
      const gapped = runningBest === NEG ? NEG : runningBest + 1 + bonus - Math.min(j - runningAt - 1, 10) * 0.2;
      if (adjacent >= gapped && adjacent !== NEG) {
        best[i][j] = adjacent;
        from[i][j] = j - 1;
      } else if (gapped !== NEG) {
        best[i][j] = gapped;
        from[i][j] = runningAt;
      }
    }
  }
  let end = -1;
  let score = NEG;
  for (let j = 0; j < m; j++) {
    if (best[n - 1][j] > score) {
      score = best[n - 1][j];
      end = j;
    }
  }
  if (end < 0) return null;
  const positions = new Array<number>(n);
  for (let i = n - 1, j = end; i >= 0; i--) {
    positions[i] = j;
    j = from[i][j];
  }
  return { score, positions };
}

/** Items matching `query`, best first (input order among equals); all of them, in order, for an empty query. */
export function fuzzyFilter<T>(items: readonly T[], query: string, textOf: (item: T) => string): T[] {
  if (query.trim() === "") return [...items];
  const scored: { item: T; score: number; index: number }[] = [];
  items.forEach((item, index) => {
    const match = fuzzyMatch(query, textOf(item));
    if (match) scored.push({ item, score: match.score, index });
  });
  scored.sort((a, b) => b.score - a.score || a.index - b.index);
  return scored.map((s) => s.item);
}
