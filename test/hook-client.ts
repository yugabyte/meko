import { request } from 'node:http';

/** Posts a hook payload over the unix socket, like the hook forwarder script does. Resolves to the status code. */
export function postHook(sock: string, path: string, token: string | null, body: unknown): Promise<number> {
  return new Promise((resolve, reject) => {
    const headers = { 'Content-Type': 'application/json', ...(token ? { 'X-AK-Token': token } : {}) };
    const req = request({ socketPath: sock, path, method: 'POST', headers }, (res) => {
      res.resume();
      res.on('end', () => resolve(res.statusCode ?? 0));
    });
    req.on('error', reject);
    req.end(JSON.stringify(body));
  });
}
