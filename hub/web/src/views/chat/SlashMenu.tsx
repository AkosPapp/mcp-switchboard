import type { Skill } from "../prompts/skills";

/** The skills a "/prefix" at the start of a message could mean, best first. */
export function matchSkills(skills: Skill[], text: string): Skill[] {
  const m = /^\/([a-z0-9_-]*)$/.exec(text);
  if (!m) return [];
  const q = m[1];
  const starts = skills.filter((k) => k.name.startsWith(q));
  const inside = skills.filter((k) => !k.name.startsWith(q) && (k.name.includes(q) || k.description.toLowerCase().includes(q)));
  return q === "" ? skills : [...starts, ...inside];
}

/** The / menu above the composer: pick a skill to run. */
export default function SlashMenu({
  skills,
  active,
  onPick,
  onHover,
}: {
  skills: Skill[];
  active: number;
  onPick: (skill: Skill) => void;
  onHover: (index: number) => void;
}) {
  return (
    <ul
      role="listbox"
      aria-label="skills"
      data-testid="slash-menu"
      className="absolute inset-x-0 bottom-full z-10 mb-1 max-h-56 overflow-y-auto rounded border border-border bg-surface py-1 shadow"
    >
      {skills.map((k, i) => (
        <li key={k.id} role="option" aria-selected={i === active}>
          <button
            type="button"
            // mousedown, not click: the textarea must not lose focus first.
            onMouseDown={(e) => {
              e.preventDefault();
              onPick(k);
            }}
            onMouseEnter={() => onHover(i)}
            className={`flex min-h-[44px] w-full items-baseline gap-2 px-2 py-1 text-left md:min-h-0 ${i === active ? "bg-raised" : ""}`}
          >
            <span className="shrink-0 font-mono text-sm text-accent">/{k.name}</span>
            <span className="min-w-0 truncate text-xs text-muted">{k.description}</span>
          </button>
        </li>
      ))}
    </ul>
  );
}
