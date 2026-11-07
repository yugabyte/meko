// Catches a page that throws while rendering and says so, instead of React leaving the window blank
// (on a projector, mid-talk). The app keys it on the page's path, so moving to another page clears it.

import { Component, type ReactNode } from 'react';

export class ErrorBoundary extends Component<{ children: ReactNode }, { error?: Error }> {
  state: { error?: Error } = {};

  static getDerivedStateFromError(error: Error) {
    return { error };
  }

  render() {
    if (!this.state.error) return this.props.children;
    return (
      <div role="alert" className="space-y-2 p-6 text-sm">
        <p className="font-semibold">This page stopped: {this.state.error.message}</p>
        <p className="text-muted-foreground">Reload it. If Present was just updated, restart it (meko stop, then meko start) first.</p>
      </div>
    );
  }
}
