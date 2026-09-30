// Netlify Function adapter. Serves the shared handler at /api/kapa/chat.

import { handleKapaChat } from './_kapa-handler.mjs';

export default async (request, _context) => {
  if (request.method !== 'POST') {
    return new Response(JSON.stringify({ error: 'POST only' }), {
      status: 405,
      headers: { 'Content-Type': 'application/json' },
    });
  }

  let payload;
  try {
    payload = await request.json();
  } catch {
    return new Response(JSON.stringify({ error: 'Invalid JSON body' }), {
      status: 400,
      headers: { 'Content-Type': 'application/json' },
    });
  }

  const env = {
    KAPA_API_KEY: process.env.KAPA_API_KEY,
    KAPA_PROJECT_ID: process.env.KAPA_PROJECT_ID,
    KAPA_INTEGRATION_ID: process.env.KAPA_INTEGRATION_ID,
    KAPA_SOURCE_GROUP_IDS: process.env.KAPA_SOURCE_GROUP_IDS,
  };

  const result = await handleKapaChat(payload, env);

  return new Response(result.body, {
    status: result.status,
    headers: {
      'Content-Type': result.contentType,
      'Cache-Control': 'no-store',
      'X-Accel-Buffering': 'no',
    },
  });
};

export const config = {
  path: '/api/kapa/chat',
};
