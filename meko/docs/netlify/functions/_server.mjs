// Local dev adapter. Runs the shared handler as a standalone Node HTTP
// server so Docker dev can hit /api/kapa/chat without deploying to
// Netlify. Listens on PORT (default 8787); the browser connects directly
// through the kapa.chat_endpoint development setting.

import http from 'node:http';
import { handleKapaChat } from './_kapa-handler.mjs';

const PORT = Number(process.env.PORT || 8787);

const server = http.createServer(async (req, res) => {
  if (req.method === 'OPTIONS') {
    res.writeHead(204, {
      'Access-Control-Allow-Origin': '*',
      'Access-Control-Allow-Methods': 'POST, OPTIONS',
      'Access-Control-Allow-Headers': 'Content-Type',
    });
    res.end();
    return;
  }

  if (!(req.method === 'POST' && (req.url === '/api/kapa/chat' || req.url === '/'))) {
    res.writeHead(404, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ error: 'Not found' }));
    return;
  }

  const chunks = [];
  for await (const chunk of req) chunks.push(chunk);
  const raw = Buffer.concat(chunks).toString('utf8');

  let payload;
  try {
    payload = JSON.parse(raw || '{}');
  } catch {
    res.writeHead(400, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ error: 'Invalid JSON body' }));
    return;
  }

  const env = {
    KAPA_API_KEY: process.env.KAPA_API_KEY,
    KAPA_PROJECT_ID: process.env.KAPA_PROJECT_ID,
    KAPA_INTEGRATION_ID: process.env.KAPA_INTEGRATION_ID,
    KAPA_SOURCE_GROUP_IDS: process.env.KAPA_SOURCE_GROUP_IDS,
  };

  const result = await handleKapaChat(payload, env);

  res.writeHead(result.status, {
    'Content-Type': result.contentType,
    'Cache-Control': 'no-store',
    'X-Accel-Buffering': 'no',
    'Access-Control-Allow-Origin': '*',
  });

  if (typeof result.body === 'string') {
    res.end(result.body);
    return;
  }

  const reader = result.body.getReader();
  const cancelReader = () => {
    void reader.cancel().catch(() => {});
  };
  res.once('close', cancelReader);
  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      res.write(value);
    }
  } catch (err) {
    if (!res.destroyed && !res.writableEnded) {
      // Upstream error mid-stream — best-effort error frame.
      res.write(`event: error\ndata: ${JSON.stringify({ error: err.message })}\n\n`);
    }
  } finally {
    res.off('close', cancelReader);
    await reader.cancel().catch(() => {});
    if (!res.writableEnded) res.end();
  }
});

server.listen(PORT, '0.0.0.0', () => {
  // eslint-disable-next-line no-console
  console.log(`[kapa-proxy] listening on 0.0.0.0:${PORT}`);
});
