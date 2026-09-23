import { useEffect, useId, useState } from "react";

type Mermaid = typeof import("mermaid").default;
let loading: Promise<Mermaid> | undefined;

/** mermaid is large, so it is fetched the first time a diagram is shown. */
function loadMermaid(): Promise<Mermaid> {
  loading ??= import("mermaid").then((m) => {
    const cs = getComputedStyle(document.documentElement);
    const c = (name: string) => `rgb(${cs.getPropertyValue(name).trim().replace(/ /g, ",")})`;
    m.default.initialize({
      startOnLoad: false,
      securityLevel: "strict",
      theme: "base",
      themeVariables: {
        background: c("--bg"),
        primaryColor: c("--raised"),
        primaryTextColor: c("--text"),
        primaryBorderColor: c("--accent"),
        lineColor: c("--muted"),
        secondaryColor: c("--surface"),
        tertiaryColor: c("--surface"),
        textColor: c("--text"),
        fontFamily: "inherit",
      },
    });
    return m.default;
  });
  return loading;
}

/** Rendered diagrams by source, so a remount shows the last good drawing at once. */
const rendered = new Map<string, string>();

/**
 * A mermaid diagram. While a reply is still streaming the source is often
 * incomplete and will not parse; in that case (or if mermaid fails to load)
 * the source is shown as plain code instead of an error.
 */
export default function MermaidDiagram({ source }: { source: string }) {
  const id = "mmd-" + useId().replace(/[^a-zA-Z0-9]/g, "");
  const [svg, setSvg] = useState<string | null>(() => rendered.get(source) ?? null);

  useEffect(() => {
    const cached = rendered.get(source);
    if (cached !== undefined) {
      setSvg(cached);
      return;
    }
    let live = true;
    const timer = window.setTimeout(() => {
      loadMermaid()
        .then((m) => m.render(id, source))
        .then((r) => {
          rendered.set(source, r.svg);
          if (live) setSvg(r.svg);
        })
        .catch(() => {
          document.getElementById("d" + id)?.remove();
        });
    }, 200);
    return () => {
      live = false;
      window.clearTimeout(timer);
    };
  }, [id, source]);

  if (svg === null) {
    return <pre className="overflow-x-auto p-2 font-mono text-xs leading-relaxed">{source}</pre>;
  }
  return (
    <div
      data-testid="mermaid"
      className="overflow-x-auto p-2 [&_svg]:mx-auto [&_svg]:h-auto [&_svg]:max-w-full"
      dangerouslySetInnerHTML={{ __html: svg }}
    />
  );
}
