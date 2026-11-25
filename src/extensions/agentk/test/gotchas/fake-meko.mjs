// A fake Meko for run.sh: the REST routes the gotchas workflow calls, kept in memory.
//
// Run as `node fake-meko.mjs PORTFILE`: it listens on a free port on 127.0.0.1 and writes the port
// to PORTFILE. Besides Meko's routes it has GET /_state (what it holds and what it saw) and POST
// /_seed (a datapack, or a document in one, to set up a test). FAKE_MEKO_PAGE is the most
// documents a page of the list holds (default 2, so paging is tested), FAKE_MEKO_READ=refuse
// refuses download-url, and FAKE_MEKO_STATUS_FAIL=<n> answers the first n document-status checks
// with a proxy's 502.
import { createHash, randomUUID } from 'node:crypto';
import { renameSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:http';

const TOKEN = 'Bearer test-token';
const PAGE = Number(process.env.FAKE_MEKO_PAGE || 2);
let statusFails = Number(process.env.FAKE_MEKO_STATUS_FAIL || 0);

const packs = new Map(); // id -> {name, agent_id, description}
const docs = new Map(); // id -> {id, datapack, filename, key, content, hash, uploaded_at, status}
const tickets = new Map(); // ticket -> {key, datapack, filename}
const downloads = new Map(); // ticket -> key
const calls = [];
const violations = [];
let clock = 0;

function now() {
  clock += 1;
  return `2026-01-01T00:${String(Math.floor(clock / 60)).padStart(2, '0')}:${String(clock % 60).padStart(2, '0')}Z`;
}

function addDoc(pack, filename, content, uploadedAt, status = 'COMPLETED') {
  const d = {
    id: randomUUID(), datapack: pack, filename, key: `${pack}/${randomUUID()}/${filename}`, content,
    hash: createHash('sha256').update(content).digest('hex'), uploaded_at: uploadedAt || now(), status,
  };
  docs.set(d.id, d);
  return d;
}

const byUpload = (a, b) => (a.uploaded_at < b.uploaded_at ? -1 : a.uploaded_at > b.uploaded_at ? 1 : 0);

async function route(req, body, port) {
  const u = new URL(req.url, 'http://127.0.0.1');
  const parts = u.pathname.replace(/^\/+|\/+$/g, '').split('/');
  const json = () => JSON.parse(body.toString() || '{}');
  calls.push(`${req.method} ${u.pathname}`);
  const ok = (data, status = 200) => [status, { data }];
  const err = (status, code, message) => [status, { error: { code, message } }];

  if (parts[0] === '_state') {
    return ok({ packs: Object.fromEntries(packs), docs: [...docs.values()].sort(byUpload), calls, violations });
  }
  if (parts[0] === '_seed') {
    const b = json();
    let pack = [...packs].find(([, p]) => p.name === b.datapack_name)?.[0];
    if (!pack) {
      pack = randomUUID();
      packs.set(pack, { name: b.datapack_name, agent_id: 'seed', description: b.description ?? '' });
    }
    if (!('filename' in b)) return ok({ datapack_id: pack });
    return ok(addDoc(pack, b.filename, b.content, b.uploaded_at, b.status));
  }
  if (parts[0] === 'upload') {
    if (req.headers.authorization) violations.push('the upload POST carried an Authorization header');
    const t = tickets.get(parts[1]);
    tickets.delete(parts[1]);
    if (!t) return [403, '<Error><Code>AccessDenied</Code></Error>', 'application/xml'];
    const form = await new Request('http://127.0.0.1/', {
      method: 'POST', headers: { 'content-type': req.headers['content-type'] ?? '' }, body,
    }).formData();
    const file = form.get('file');
    if (form.get('key') !== t.key || !(file instanceof Blob)) {
      violations.push(`the upload's fields or file were wrong: ${JSON.stringify([...form.keys()])}`);
      return [400, '<Error><Code>InvalidArgument</Code></Error>', 'application/xml'];
    }
    const d = addDoc(t.datapack, t.filename, await file.text());
    d.key = t.key;
    return [204, ''];
  }
  if (parts[0] === 'download') {
    const key = downloads.get(parts[1]);
    const d = [...docs.values()].find((x) => x.key === key);
    return d ? [200, d.content, 'text/markdown'] : [404, 'gone', 'text/plain'];
  }

  if (parts[0] !== 'api' || parts[1] !== 'v1') return [404, 'no such page', 'text/plain'];
  if (req.headers.authorization !== TOKEN) return err(401, 'UNAUTHORIZED', 'a valid API key is required');
  const p = parts.slice(2);
  if (p.length === 1 && p[0] === 'datapacks' && req.method === 'GET') {
    return ok([...packs].map(([id, x]) => ({ datapack_id: id, datapack_name: x.name, description: x.description })));
  }
  if (p.length === 1 && p[0] === 'datapacks' && req.method === 'POST') {
    const b = json();
    if (b.agent_id !== 'meko_agent') return err(404, 'NOT_FOUND', 'agent not found');
    const id = randomUUID();
    packs.set(id, { name: b.name, agent_id: b.agent_id, description: b.description ?? '' });
    return ok({ datapack_id: id, datapack_name: b.name }, 201);
  }
  if (p.length === 2 && p[0] === 'datapacks' && req.method === 'PATCH') {
    if (!packs.has(p[1])) return err(404, 'NOT_FOUND', 'datapack not found');
    packs.get(p[1]).description = json().description;
    return ok({ message: 'datapack updated' });
  }
  if (p.length < 4 || p[0] !== 'datapacks' || p[2] !== 'knowledge-bases' || !packs.has(p[1])) {
    return err(404, 'NOT_FOUND', 'no such route or datapack');
  }
  const pack = p[1];
  const rest = p.slice(3).join('/');
  if (rest === 'upload-url' && req.method === 'POST') {
    const b = json();
    const same = [...docs.values()].find((d) => d.datapack === pack && d.hash === b.content_hash);
    if (same) return ok({ already_uploaded: true, key: same.key });
    const key = `${pack}/${randomUUID()}/${b.filename}`;
    const t = randomUUID();
    tickets.set(t, { key, datapack: pack, filename: b.filename });
    return ok({ url: `http://127.0.0.1:${port}/upload/${t}`, fields: { key, policy: 'test-policy' }, key });
  }
  if (rest === 'document-status' && req.method === 'GET') {
    if (statusFails > 0) {
      statusFails -= 1;
      return [502, '<html>Bad Gateway</html>', 'text/html'];
    }
    const key = u.searchParams.get('document_key') ?? '';
    if ([...docs.values()].some((d) => d.key === key)) return ok({ document_key: key, state: 'completed', message: 'indexed' });
    return err(404, 'NOT_FOUND', 'unknown key');
  }
  if (rest === 'documents' && req.method === 'GET') {
    const page = Number(u.searchParams.get('page') || 1);
    const size = Math.min(Number(u.searchParams.get('page_size') || 50), PAGE);
    const mine = [...docs.values()].filter((d) => d.datapack === pack).sort(byUpload);
    const items = mine.slice((page - 1) * size, page * size).map((d) => ({
      id: d.id, filename: d.filename, document_uri: `local://${d.key}`, document_name: d.key,
      uploaded_at: d.uploaded_at, status: d.status,
    }));
    return ok({ items, total: mine.length, page, page_size: size, has_more: page * size < mine.length });
  }
  if (p.length === 5 && p[3] === 'documents' && req.method === 'DELETE') {
    if (!docs.delete(p[4])) return err(404, 'NOT_FOUND', 'document not found in this datapack');
    return ok({ document_id: p[4], chunks_deleted: 1, s3_deleted: true });
  }
  if (rest === 'download-url' && req.method === 'POST') {
    if (process.env.FAKE_MEKO_READ === 'refuse') return err(403, 'FORBIDDEN', 'not with an API key');
    const key = String(json().document_uri).replace(/^local:\/\//, '');
    const t = randomUUID();
    downloads.set(t, key);
    return ok({ url: `http://127.0.0.1:${port}/download/${t}`, key, expires_in: 300 });
  }
  return err(404, 'NOT_FOUND', 'no such route');
}

const server = createServer((req, res) => {
  const chunks = [];
  req.on('data', (c) => chunks.push(c));
  req.on('end', () => {
    route(req, Buffer.concat(chunks), server.address().port).then(([status, body, type = 'application/json']) => {
      const raw = typeof body === 'string' ? body : JSON.stringify(body);
      res.writeHead(status, status === 204 ? {} : { 'Content-Type': type, 'Content-Length': Buffer.byteLength(raw) });
      res.end(status === 204 ? undefined : raw);
    }, (e) => {
      res.writeHead(500, { 'Content-Type': 'text/plain' });
      res.end(String(e));
    });
  });
});
server.listen(0, '127.0.0.1', () => {
  writeFileSync(`${process.argv[2]}.tmp`, String(server.address().port));
  renameSync(`${process.argv[2]}.tmp`, process.argv[2]);
});
