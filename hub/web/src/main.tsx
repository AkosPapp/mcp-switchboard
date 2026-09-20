import React from "react";
import ReactDOM from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter } from "react-router-dom";

import App from "./App";
import "./index.css";

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // The event stream pushes invalidations (spec.md U3), so polling would be
      // redundant work; refetching on focus covers the case where the stream
      // dropped while the tab was in the background.
      refetchOnWindowFocus: true,
      refetchInterval: false,
      staleTime: 5_000,
      retry: 1,
    },
  },
});

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <QueryClientProvider client={queryClient}>
      {/* The future flags are opt-ins for v7 behaviour that React Router
          otherwise warns about on every render; turning them on now keeps the
          console's console output worth reading. */}
      <BrowserRouter future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
        <App />
      </BrowserRouter>
    </QueryClientProvider>
  </React.StrictMode>,
);
