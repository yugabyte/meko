import { useCallback, useEffect, useState } from 'react';
import type { AuthStatus } from '../../shared/types';
import { api, errorMessage, setUnauthorizedHandler } from '../api';
import { disposeAllTerminals } from '../terminals';
import { App } from './App';
import { LoginScreen, SetupScreen } from './Auth';
import { Busy } from './common';

export function Root() {
  const [auth, setAuth] = useState<AuthStatus | null>(null);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      setAuth(await api.authStatus());
      setError(null);
    } catch (e) {
      setError(errorMessage(e));
    }
  }, []);

  useEffect(() => {
    refresh();
    setUnauthorizedHandler(() => {
      disposeAllTerminals();
      setAuth((a) => (a ? { ...a, loggedIn: false } : a));
    });
    return () => setUnauthorizedHandler(null);
  }, [refresh]);

  if (error && !auth) {
    return (
      <div className="auth-page">
        <div className="auth-card">
          <h1>Can’t reach the server</h1>
          <p className="muted">{error}</p>
          <button className="btn primary" onClick={refresh}>
            Try again
          </button>
        </div>
      </div>
    );
  }
  if (!auth) return <div className="boot"><Busy label="Loading" /></div>;
  if (auth.setupRequired) return <SetupScreen reset={auth.reset} onDone={refresh} />;
  if (!auth.loggedIn) return <LoginScreen onDone={refresh} />;
  return (
    <App
      auth={auth}
      onLoggedOut={() => {
        disposeAllTerminals();
        refresh();
      }}
    />
  );
}
