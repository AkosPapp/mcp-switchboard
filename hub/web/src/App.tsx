import { NavLink, Navigate, Route, Routes } from "react-router-dom";

import { useEventStream, type StreamState } from "./hooks/useEventStream";
import { ToastProvider } from "./components/Toast";
import Calls from "./views/Calls";
import Connections from "./views/Connections";
import Endpoints from "./views/Endpoints";

const TABS = [
  { to: "/connections", label: "Connections" },
  { to: "/calls", label: "Calls" },
  { to: "/endpoints", label: "Endpoints" },
];

/** The stream indicator, which is the only thing on screen that says the
 * console is still hearing from the hub. */
function LiveDot({ state }: { state: StreamState }) {
  const colour =
    state === "live" ? "bg-ok" : state === "connecting" ? "bg-warn" : "bg-danger";
  return (
    <div
      className="flex shrink-0 items-center gap-2 text-xs text-muted"
      title="event stream"
      aria-live="polite"
    >
      <span className={`h-2 w-2 rounded-full ${colour}`} aria-hidden="true" />
      <span className="hidden sm:inline">{state}</span>
    </div>
  );
}

export default function App() {
  const stream = useEventStream();

  return (
    <ToastProvider>
      <div className="flex h-full min-h-dvh flex-col">
        {/* Three items that must all survive a 375px viewport: the brand and
            the stream indicator keep their size, and the tabs take what is left
            and scroll if they have to. Without the explicit shrink rules the
            indicator is pushed past the right edge and the whole page scrolls
            sideways (spec.md U2). */}
        <header className="flex items-center gap-2 border-b border-border bg-surface px-3 py-2 sm:gap-3 sm:px-4">
          <div className="flex shrink-0 items-center gap-2">
            <span
              className="h-4 w-4 rounded-sm bg-accent"
              aria-hidden="true"
            />
            <span className="hidden text-sm font-semibold tracking-tight sm:inline">
              mcp-switchboard
            </span>
          </div>

          <nav className="flex min-w-0 flex-1 items-center justify-end gap-1 overflow-x-auto sm:justify-start">
            {TABS.map((tab) => (
              <NavLink
                key={tab.to}
                to={tab.to}
                className={({ isActive }) =>
                  [
                    "shrink-0 rounded px-2.5 py-1.5 text-sm transition-colors",
                    isActive
                      ? "bg-raised font-medium text-text"
                      : "text-muted hover:bg-raised hover:text-text",
                  ].join(" ")
                }
              >
                {tab.label}
              </NavLink>
            ))}
          </nav>

          <LiveDot state={stream} />
        </header>

        <main className="min-h-0 flex-1">
          <Routes>
            <Route path="/" element={<Navigate to="/connections" replace />} />
            <Route path="/connections" element={<Connections />} />
            <Route path="/calls" element={<Calls />} />
            <Route path="/endpoints" element={<Endpoints />} />
            {/* The hub serves index.html for any unmatched non-/api path, so a
                deep link that no route claims lands here rather than on a 404
                page the user cannot act on. */}
            <Route path="*" element={<Navigate to="/connections" replace />} />
          </Routes>
        </main>
      </div>
    </ToastProvider>
  );
}
