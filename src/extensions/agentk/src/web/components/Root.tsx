import { useEffect, useState } from 'react';
import { mekoAuth, type MekoAuth } from '../signin';
import { App } from './App';

/**
 * AgentK has no login of its own: Meko, which serves it, decides who reaches the page.
 * What Meko says about this browser decides whether there's a Sign out.
 */
export function Root() {
  const [auth, setAuth] = useState<MekoAuth | null>(null);
  useEffect(() => {
    void mekoAuth().then(setAuth);
  }, []);
  return <App mekoAuth={auth} />;
}
