import { Component, type ErrorInfo, type ReactNode } from "react";

interface Props {
  children: ReactNode;
  /** Changing this (the route) clears a caught error, so navigating away recovers. */
  resetKey?: string;
}

interface State {
  error: Error | null;
  resetKey?: string;
}

/**
 * Without this, one exception in a render unmounts the whole app to a blank
 * page with the cause only in the console. It shows the message instead, and
 * lets the user navigate away or reload.
 */
export default class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null, resetKey: this.props.resetKey };

  static getDerivedStateFromError(error: Error): Partial<State> {
    return { error };
  }

  static getDerivedStateFromProps(props: Props, state: State): Partial<State> | null {
    if (props.resetKey !== state.resetKey) return { error: null, resetKey: props.resetKey };
    return null;
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error("view crashed", error, info.componentStack);
  }

  render() {
    const { error } = this.state;
    if (!error) return this.props.children;
    return (
      <div role="alert" className="m-4 rounded border border-danger p-4 text-sm">
        <p className="font-medium text-danger">This view crashed.</p>
        <pre className="mt-2 whitespace-pre-wrap break-words text-muted">{error.message}</pre>
        <button
          type="button"
          onClick={() => window.location.reload()}
          className="mt-3 min-h-[44px] rounded border border-border px-3 md:min-h-0"
        >
          Reload
        </button>
      </div>
    );
  }
}
