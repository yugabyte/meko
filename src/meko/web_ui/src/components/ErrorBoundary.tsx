// Catches a page that throws while rendering and says so, instead of leaving the screen blank. The
// app keys it on the route, so moving to another page clears it.

import { Component, type ReactNode } from "react";
import { ErrorNote } from "./ui";

export class ErrorBoundary extends Component<{ children: ReactNode }, { error?: Error }> {
  state: { error?: Error } = {};

  static getDerivedStateFromError(error: Error) {
    return { error };
  }

  render() {
    if (!this.state.error) return this.props.children;
    return (
      <div className="space-y-3 p-4 sm:p-8">
        <ErrorNote error={this.state.error} />
        <p className="text-sm text-muted-foreground">
          If Meko was just updated, restart it (<code>meko restart</code>) and reload this page.
        </p>
      </div>
    );
  }
}
