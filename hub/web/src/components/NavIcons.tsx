/**
 * Small inline SVG icons for the top nav (spec.md U2): one per tab, plus the
 * mobile menu glyph. Hand-written rather than a dependency - the app has none
 * for icons, and seven glyphs do not earn one. 18x18, stroke-based, inherit
 * `currentColor` so they follow the nav link's own text colour (active/hover).
 */

type IconProps = { className?: string };

const base = "h-[18px] w-[18px]";
const common = {
  viewBox: "0 0 24 24",
  fill: "none",
  stroke: "currentColor",
  strokeWidth: 1.8,
  strokeLinecap: "round" as const,
  strokeLinejoin: "round" as const,
  "aria-hidden": true,
};

export function ChatIcon({ className }: IconProps) {
  return (
    <svg className={className ?? base} {...common}>
      <path d="M4 5h16v11H8l-4 4V5Z" />
    </svg>
  );
}

export function GraphIcon({ className }: IconProps) {
  return (
    <svg className={className ?? base} {...common}>
      <circle cx="6" cy="6" r="2.5" />
      <circle cx="18" cy="6" r="2.5" />
      <circle cx="12" cy="18" r="2.5" />
      <path d="M8 7.5 16 7.5M7.5 8 11 16M16.5 8 13 16" />
    </svg>
  );
}

export function PromptsIcon({ className }: IconProps) {
  return (
    <svg className={className ?? base} {...common}>
      <path d="M5 4h11l3 3v13H5V4Z" />
      <path d="M9 10h6M9 14h6M9 18h3" />
    </svg>
  );
}

export function ConnectionsIcon({ className }: IconProps) {
  return (
    <svg className={className ?? base} {...common}>
      <path d="M9 3v4M15 3v4M6 7h12l-1 5a5 5 0 0 1-10 0L6 7Z" />
      <path d="M12 16v5" />
    </svg>
  );
}

export function CallsIcon({ className }: IconProps) {
  return (
    <svg className={className ?? base} {...common}>
      <path d="M4 12h4l2-4 4 8 2-4h4" />
    </svg>
  );
}

export function EndpointsIcon({ className }: IconProps) {
  return (
    <svg className={className ?? base} {...common}>
      <rect x="4" y="4" width="16" height="6" rx="1.5" />
      <rect x="4" y="14" width="16" height="6" rx="1.5" />
      <path d="M8 7h.01M8 17h.01" strokeWidth={2.4} />
    </svg>
  );
}

export function SettingsIcon({ className }: IconProps) {
  return (
    <svg className={className ?? base} {...common}>
      <circle cx="12" cy="12" r="3" />
      <path d="M19.4 13.5a1.7 1.7 0 0 0 .34 1.87l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.7 1.7 0 0 0-1.87-.34 1.7 1.7 0 0 0-1.04 1.56V19.5a2 2 0 1 1-4 0v-.09a1.7 1.7 0 0 0-1.04-1.56 1.7 1.7 0 0 0-1.87.34l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.7 1.7 0 0 0 .34-1.87 1.7 1.7 0 0 0-1.56-1.04H4.5a2 2 0 1 1 0-4h.09a1.7 1.7 0 0 0 1.56-1.04 1.7 1.7 0 0 0-.34-1.87l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.7 1.7 0 0 0 1.87.34H10.5a1.7 1.7 0 0 0 1.04-1.56V4.5a2 2 0 1 1 4 0v.09a1.7 1.7 0 0 0 1.04 1.56 1.7 1.7 0 0 0 1.87-.34l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.7 1.7 0 0 0-.34 1.87V10.5a1.7 1.7 0 0 0 1.56 1.04H19.5a2 2 0 1 1 0 4h-.09a1.7 1.7 0 0 0-1.56 1.04Z" />
    </svg>
  );
}

export function MenuIcon({ className }: IconProps) {
  return (
    <svg className={className ?? base} {...common}>
      <path d="M4 6h16M4 12h16M4 18h16" />
    </svg>
  );
}
