import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from "react";

/**
 * Transient messages, for the things that are worth saying once and not worth a
 * dialog: "copied", "dropped keys not in the schema", "restart requested".
 */

interface ToastValue {
  show: (message: string) => void;
}

const ToastContext = createContext<ToastValue>({ show: () => {} });

export function useToast(): ToastValue {
  return useContext(ToastContext);
}

export function ToastProvider({ children }: { children: ReactNode }) {
  const [message, setMessage] = useState<string | null>(null);

  const show = useCallback((text: string) => {
    setMessage(text);
    window.setTimeout(() => {
      // Clear only if nothing newer replaced it, so a second toast is not cut
      // short by the first one's timer.
      setMessage((current) => (current === text ? null : current));
    }, 3200);
  }, []);

  const value = useMemo(() => ({ show }), [show]);

  return (
    <ToastContext.Provider value={value}>
      {children}
      {message !== null && (
        <div
          role="status"
          className="pointer-events-none fixed inset-x-0 bottom-4 z-50 mx-auto w-fit max-w-[90vw] rounded-md border border-border bg-raised px-3 py-2 text-sm shadow-lg"
          style={{ marginBottom: "env(safe-area-inset-bottom)" }}
        >
          {message}
        </div>
      )}
    </ToastContext.Provider>
  );
}
