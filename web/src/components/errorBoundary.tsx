import { RotateCw } from "lucide-react";
import * as React from "react";
import { Button } from "@/components/ui/button";

/** A render error anywhere shows a plain page with a way back, never a blank screen. */
export class ErrorBoundary extends React.Component<{ children: React.ReactNode }, { failed: boolean }> {
  state = { failed: false };

  static getDerivedStateFromError() {
    return { failed: true };
  }

  componentDidCatch(error: unknown) {
    console.error("render failed", error instanceof Error ? error.name : "unknown");
  }

  render() {
    if (!this.state.failed) return this.props.children;
    return (
      <div role="alert" className="flex min-h-dvh items-center justify-center bg-background px-6">
        <div className="w-full max-w-md text-center">
          <p className="label-mono text-accent">pgmandate</p>
          <h1 className="mt-2 font-serif text-3xl font-medium">Something went wrong</h1>
          <p className="mt-3 text-sm text-muted">The page could not be shown. Nothing you saved is lost. Reload and try again.</p>
          <Button className="mt-6" onClick={() => window.location.reload()}>
            <RotateCw /> Reload
          </Button>
        </div>
      </div>
    );
  }
}
