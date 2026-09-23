/** @type {import('tailwindcss').Config} */
export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  darkMode: ["variant", ':root:not([data-theme="light"]) &'],
  theme: {
    extend: {
      // Colours come from CSS custom properties (spec.md U4) so the palette can
      // follow prefers-color-scheme and still be overridden explicitly, without
      // Tailwind needing two of every utility.
      colors: {
        bg: "rgb(var(--bg) / <alpha-value>)",
        surface: "rgb(var(--surface) / <alpha-value>)",
        raised: "rgb(var(--raised) / <alpha-value>)",
        border: "rgb(var(--border) / <alpha-value>)",
        text: "rgb(var(--text) / <alpha-value>)",
        muted: "rgb(var(--muted) / <alpha-value>)",
        accent: "rgb(var(--accent) / <alpha-value>)",
        "on-accent": "rgb(var(--on-accent) / <alpha-value>)",
        ok: "rgb(var(--ok) / <alpha-value>)",
        warn: "rgb(var(--warn) / <alpha-value>)",
        danger: "rgb(var(--danger) / <alpha-value>)",
      },
      fontFamily: {
        mono: ["ui-monospace", '"JetBrains Mono"', '"Fira Code"', "SFMono-Regular", "Menlo", "Consolas", "monospace"],
      },
      // Tighter corners across the console: `rounded` is 2px, not 4px.
      borderRadius: { DEFAULT: "0.125rem", md: "0.1875rem", lg: "0.25rem", xl: "0.375rem" },
    },
  },
  plugins: [],
};
