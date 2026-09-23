import { useEffect, useState, type ComponentType } from "react";
import { Link, Navigate, Route, Routes, useLocation, useParams } from "react-router-dom";

import { useEventStream, type StreamState } from "./hooks/useEventStream";
import { useOrchestratorEnabled } from "./hooks/useOrchestratorEnabled";
import { loadRouteMemory, nextLocationForTab, restoreTarget, saveLocation, tabOf } from "./lib/routeMemory";
import { ToastProvider } from "./components/Toast";
import { InstallPrompt } from "./components/InstallPrompt";
import {
  CallsIcon,
  ChatIcon,
  ConnectionsIcon,
  EndpointsIcon,
  GraphIcon,
  MenuIcon,
  PromptsIcon,
  SettingsIcon,
} from "./components/NavIcons";
import Calls from "./views/Calls";
import Connections from "./views/Connections";
import Endpoints from "./views/Endpoints";
import Settings from "./views/Settings";
import ChatView from "./views/chat/ChatView";
import GraphView from "./views/graph/GraphView";
import PromptsView from "./views/prompts/PromptsView";

interface Tab {
  to: string;
  label: string;
  Icon: ComponentType<{ className?: string }>;
}

// Order: the conversational tools first (what you use most), then the
// operational ones, Settings last.
const ORCHESTRATOR_TABS: Tab[] = [
  { to: "/chat", label: "Chat", Icon: ChatIcon },
  { to: "/graph", label: "Graph", Icon: GraphIcon },
  { to: "/prompts", label: "Prompts", Icon: PromptsIcon },
];
const BASE_TABS: Tab[] = [
  { to: "/connections", label: "Connections", Icon: ConnectionsIcon },
  { to: "/calls", label: "Calls", Icon: CallsIcon },
  { to: "/endpoints", label: "Endpoints", Icon: EndpointsIcon },
  { to: "/settings", label: "Settings", Icon: SettingsIcon },
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

const linkClass = ({ isActive }: { isActive: boolean }) =>
  [
    "flex shrink-0 items-center gap-1.5 rounded px-2.5 py-1.5 text-sm transition-colors",
    isActive ? "bg-raised font-medium text-text" : "text-muted hover:bg-raised hover:text-text",
  ].join(" ");

/** "/": the last place visited, remembered across reloads (spec.md: the
 * browser remembers where you were under each tab). */
function RestoreRoot() {
  return <Navigate to={restoreTarget(loadRouteMemory())} replace />;
}

/** The Agents tab became Prompts; an old "/agents/<id>" link still works. */
function AgentsProfileRedirect() {
  const { profileId } = useParams();
  return <Navigate to={`/prompts/${profileId}`} replace />;
}

export default function App() {
  const stream = useEventStream();
  const orchestrator = useOrchestratorEnabled();
  const [menuOpen, setMenuOpen] = useState(false);
  const location = useLocation();
  // Every tab root remembers the last place visited under it, so its nav link
  // reopens that instead of always its bare root (spec.md: the browser
  // remembers where you were under each tab).
  useEffect(() => {
    saveLocation(location);
  }, [location.pathname, location.search]);
  // Orchestrator routes: null while the probe is in flight, so a deep link is
  // not bounced to /connections before the hub has answered.
  const orchestratorOnly = (el: JSX.Element) =>
    orchestrator === undefined ? null : orchestrator ? el : <Navigate to="/connections" replace />;

  const tabs = [...(orchestrator ? ORCHESTRATOR_TABS : []), ...BASE_TABS];
  const memory = loadRouteMemory();

  return (
    <ToastProvider>
      <div className="flex h-full min-h-dvh flex-col">
        {/* Three items that must all survive a 375px viewport: the brand and
            the stream indicator keep their size, and the tabs take what is left
            and scroll if they have to. Without the explicit shrink rules the
            indicator is pushed past the right edge and the whole page scrolls
            sideways (spec.md U2). Below sm, tab labels collapse to icons and a
            menu button opens the full list as a vertical drawer instead. */}
        <header className="flex items-center gap-2 border-b border-border bg-surface px-3 py-2 sm:gap-3 sm:px-4">
          <button
            type="button"
            className="flex h-9 w-9 shrink-0 items-center justify-center rounded text-muted hover:bg-raised hover:text-text sm:hidden"
            aria-label="Open navigation menu"
            aria-expanded={menuOpen}
            onClick={() => setMenuOpen(true)}
          >
            <MenuIcon />
          </button>

          <div className="hidden shrink-0 items-center gap-2 sm:flex">
            <span className="h-4 w-4 rounded-sm bg-accent" aria-hidden="true" />
            <span className="text-sm font-semibold tracking-tight">mcp-switchboard</span>
          </div>

          <nav className="flex min-w-0 flex-1 items-center justify-end gap-1 overflow-x-auto sm:justify-start">
            {tabs.map((tab) => (
              <Link
                key={tab.to}
                to={nextLocationForTab(tab.to, location, memory)}
                className={linkClass({ isActive: tabOf(location.pathname) === tab.to })}
                aria-label={tab.label}
              >
                <tab.Icon className="h-[18px] w-[18px] sm:hidden" />
                <span className="hidden sm:inline" aria-hidden="true">
                  {tab.label}
                </span>
              </Link>
            ))}
          </nav>

          <LiveDot state={stream} />
        </header>

        {menuOpen ? (
          <div className="fixed inset-0 z-50 flex sm:hidden" role="dialog" aria-modal="true">
            <button
              type="button"
              className="flex-1 bg-black/50"
              aria-label="Close navigation menu"
              onClick={() => setMenuOpen(false)}
            />
            <nav className="flex w-64 max-w-[80vw] flex-col gap-1 border-l border-border bg-surface p-3">
              {tabs.map((tab) => (
                <Link
                  key={tab.to}
                  to={nextLocationForTab(tab.to, location, memory)}
                  className={linkClass({ isActive: tabOf(location.pathname) === tab.to })}
                  onClick={() => setMenuOpen(false)}
                >
                  <tab.Icon />
                  {tab.label}
                </Link>
              ))}
            </nav>
          </div>
        ) : null}

        <main className="min-h-0 flex-1">
          <Routes>
            <Route path="/" element={<RestoreRoot />} />
            <Route path="/connections" element={<Connections />} />
            <Route path="/calls" element={<Calls />} />
            <Route path="/endpoints" element={<Endpoints />} />
            <Route path="/settings" element={<Settings />} />
            <Route path="/chat" element={orchestratorOnly(<ChatView />)} />
            <Route path="/chat/:chatId" element={orchestratorOnly(<ChatView />)} />
            <Route path="/graph" element={orchestratorOnly(<GraphView />)} />
            <Route path="/graph/:agentId" element={orchestratorOnly(<GraphView />)} />
            <Route path="/prompts" element={orchestratorOnly(<PromptsView />)} />
            <Route path="/prompts/:profileId" element={orchestratorOnly(<PromptsView />)} />
            {/* The Agents tab became Prompts; old links and bookmarks still work. */}
            <Route path="/agents" element={<Navigate to="/prompts" replace />} />
            <Route path="/agents/:profileId" element={<AgentsProfileRedirect />} />
            {/* The hub serves index.html for any unmatched non-/api path, so a
                deep link that no route claims lands here rather than on a 404
                page the user cannot act on. */}
            <Route path="*" element={<Navigate to="/connections" replace />} />
          </Routes>
        </main>

        <InstallPrompt />
      </div>
    </ToastProvider>
  );
}
