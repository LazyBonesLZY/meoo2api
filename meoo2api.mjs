#!/usr/bin/env node
// meoo2api — Meoo 模型网关的 OpenAI 兼容层 + 管理控制台
import { createServer } from 'node:http';
import { appendFileSync, existsSync, mkdirSync, readFileSync, renameSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { randomUUID, randomBytes, timingSafeEqual, createHash, createHmac } from 'node:crypto';

const HERE = dirname(fileURLToPath(import.meta.url));
const DATA = join(HERE, 'data');

if (existsSync(join(HERE, '.env'))) {
  for (const line of readFileSync(join(HERE, '.env'), 'utf8').split(/\r?\n/)) {
    const m = line.match(/^\s*([A-Z0-9_]+)\s*=\s*(.*?)\s*$/);
    if (m && process.env[m[1]] === undefined) process.env[m[1]] = m[2].replace(/^["']|["']$/g, '');
  }
}

const BASE = (process.env.MEOO_AI_BASE || 'https://api.meoo.host/meoo-ai/compatible-mode/v1').replace(/\/+$/, '');
const PORT = Number(process.env.PORT || 8791);
const BIND = process.env.BIND || '127.0.0.1';
const LOG = process.env.LOG || join(HERE, 'meoo2api.log');
const MAX_BODY = Number(process.env.MAX_BODY_BYTES || 8 * 1024 * 1024);
const BOOT_AK = process.env.MEOO_PROJECT_AK || '';
const BOOT_KEYS = (process.env.MEOO2API_KEYS || '').split(',').map((s) => s.trim()).filter(Boolean);
const BYOK_RE = /^meoo_project_[A-Za-z0-9._-]{8,}$/;

const NATIVE = [
  'qwen3.8-flash', 'qwen3.8-max', 'qwen3.7-max', 'qwen3.7-plus',
  'deepseek-v4.1-flash-1017', 'deepseek-v4-pro-0813', 'deepseek-v4-flash-0731',
  'glm-5.2-fast-preview', 'glm-5.2',
];
const DEFAULT_ALIASES = {
  'deepseek v4.1 flash': 'deepseek-v4.1-flash-1017',
  'deepseek-v4.1-flash': 'deepseek-v4.1-flash-1017',
  'deepseek-v4-1-flash': 'deepseek-v4.1-flash-1017',
  'deepseek-flash': 'deepseek-v4.1-flash-1017',
  'deepseek v4 flash': 'deepseek-v4-flash-0731',
  'deepseek-v4-flash': 'deepseek-v4-flash-0731',
  'deepseek v4 pro': 'deepseek-v4-pro-0813',
  'deepseek-v4-pro': 'deepseek-v4-pro-0813',
  qwen: 'qwen3.8-flash',
  'qwen-max': 'qwen3.8-max',
  glm: 'glm-5.2',
};

/* ───────── 状态 ───────── */

const sha = (s) => createHash('sha256').update(s).digest('hex');
const hourKey = (d = new Date()) => {
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}`;
};

let state;

function defaultState() {
  return {
    version: 1,
    config: {
      timeoutMs: 180000,
      defaultMaxTokens: 800,
      cooldownSec: 60,
      hardCooldownSec: 900,
      sticky: false,
      maxLogs: 2000,
      maxUsageHours: 168,
      aliases: { ...DEFAULT_ALIASES },
      hiddenModels: [],
      adminUser: 'admin',
      adminPass: '',
    },
    keys: [],
    clientKeys: [],
    logs: [],
    usage: {},
  };
}

function loadState() {
  mkdirSync(DATA, { recursive: true, mode: 0o700 });
  const f = join(DATA, 'state.json');
  if (existsSync(f)) {
    try {
      const s = JSON.parse(readFileSync(f, 'utf8'));
      state = { ...defaultState(), ...s, config: { ...defaultState().config, ...(s.config || {}) } };
    } catch {
      state = defaultState();
    }
  } else {
    state = defaultState();
  }
  if (!state.config.adminPass) {
    const pw = process.env.MEOO2API_ADMIN_PASSWORD || randomBytes(9).toString('base64url');
    state.config.adminPass = sha(pw);
    writeFileSync(join(DATA, 'admin-password.txt'), pw + '\n', { mode: 0o600 });
    console.log(`\n  管理端密码：${pw}   （已写入 ${join(DATA, 'admin-password.txt')}）\n`);
  }
  if (!state.keys.length && BOOT_AK) {
    state.keys.push(newKey({ label: 'bootstrap', key: BOOT_AK }));
  }
  if (!state.clientKeys.length && BOOT_KEYS.length) {
    for (const k of BOOT_KEYS) state.clientKeys.push(newClientKey({ label: 'bootstrap', key: k }));
  }
  saveState(true);
}

let dirty = false;
function saveState(now = false) {
  if (!now && !dirty) return;
  dirty = false;
  const f = join(DATA, 'state.json');
  const tmp = f + '.tmp';
  try {
    writeFileSync(tmp, JSON.stringify(state, null, 2), { mode: 0o600 });
    renameSync(tmp, f);
  } catch (e) {
    console.error('保存状态失败:', e.message);
  }
}

const mask = (k) => (k ? `${k.slice(0, 14)}…${k.slice(-4)}` : '');
const newKey = (o = {}) => ({
  id: 'k_' + randomBytes(6).toString('hex'),
  label: o.label || '未命名',
  key: o.key || '',
  enabled: o.enabled !== false,
  weight: Number(o.weight) || 1,
  note: o.note || '',
  createdAt: Date.now(),
  health: { state: 'unknown', checkedAt: 0, latencyMs: 0, error: '' },
  stats: { req: 0, ok: 0, fail: 0, pt: 0, ct: 0, lastUsed: 0 },
  cooldownUntil: 0,
});
const newClientKey = (o = {}) => ({
  id: 'c_' + randomBytes(6).toString('hex'),
  label: o.label || '默认',
  key: o.key || 'sk-' + randomBytes(16).toString('hex'),
  enabled: o.enabled !== false,
  createdAt: Date.now(),
  stats: { req: 0, ok: 0, fail: 0, pt: 0, ct: 0, lastUsed: 0 },
});

loadState();
setInterval(() => saveState(), 5000).unref();

/* ───────── 日志 / 用量 ───────── */

let logOk = true;
try { writeFileSync(LOG, ''); } catch { logOk = false; }

function record(entry) {
  const row = { id: 'r_' + randomBytes(5).toString('hex'), ts: Date.now(), ...entry };
  state.logs.unshift(row);
  if (state.logs.length > state.config.maxLogs) state.logs.length = state.config.maxLogs;

  const h = hourKey();
  const u = (state.usage[h] ||= { req: 0, ok: 0, fail: 0, pt: 0, ct: 0, lat: [], models: {} });
  u.req++;
  if (row.status < 400) u.ok++; else u.fail++;
  u.pt += row.pt || 0;
  u.ct += row.ct || 0;
  if (row.ms) { u.lat.push(row.ms); if (u.lat.length > 500) u.lat.shift(); }
  if (row.model) u.models[row.model] = (u.models[row.model] || 0) + 1;

  const hours = Object.keys(state.usage).sort();
  while (hours.length > state.config.maxUsageHours) delete state.usage[hours.shift()];
  dirty = true;

  if (logOk) {
    try {
      appendFileSync(LOG, JSON.stringify({
        ts: new Date(row.ts).toISOString(), rid: row.rid, method: row.method, path: row.path,
        status: row.status, ms: row.ms, model: row.model, key: row.keyLabel, tokens: [row.pt, row.ct],
      }) + '\n');
    } catch { logOk = false; }
  }
}

const pct = (arr, p) => {
  if (!arr.length) return 0;
  const s = [...arr].sort((a, b) => a - b);
  return s[Math.min(s.length - 1, Math.floor((p / 100) * s.length))];
};

/* ───────── key 池 ───────── */

let rrIndex = 0;
let stickyId = null;

function usableKeys() {
  const now = Date.now();
  return state.keys.filter((k) => k.enabled && k.key && k.cooldownUntil <= now);
}

function pickKey() {
  const list = usableKeys();
  if (!list.length) return null;
  if (state.config.sticky && stickyId) {
    const hit = list.find((k) => k.id === stickyId);
    if (hit) return hit;
  }
  const pool = [];
  for (const k of list) for (let i = 0; i < Math.max(1, Math.min(5, k.weight)); i++) pool.push(k);
  const k = pool[rrIndex++ % pool.length];
  stickyId = k.id;
  return k;
}

function markKey(k, ok, { soft = false } = {}) {
  if (!k || k.id === 'byok') return;
  k.stats.req++;
  k.stats.lastUsed = Date.now();
  if (ok) {
    k.stats.ok++;
    k.cooldownUntil = 0;
  } else {
    k.stats.fail++;
    k.cooldownUntil = Date.now() + (soft ? state.config.cooldownSec : state.config.hardCooldownSec) * 1000;
  }
  dirty = true;
}

/* ───────── SSE / 上游 ───────── */

const CORS = {
  'Access-Control-Allow-Origin': '*',
  'Access-Control-Allow-Headers': 'Authorization, Content-Type, X-Request-Id',
  'Access-Control-Allow-Methods': 'GET, POST, PATCH, DELETE, OPTIONS',
  'Access-Control-Max-Age': '86400',
};

function sendJson(res, status, obj) {
  const body = JSON.stringify(obj);
  res.writeHead(status, { 'Content-Type': 'application/json; charset=utf-8', 'Content-Length': Buffer.byteLength(body), 'Cache-Control': 'no-store', ...CORS });
  res.end(body);
}
const oaError = (res, status, message, type = 'invalid_request_error', code, param) =>
  sendJson(res, status, { error: { message, type, ...(param ? { param } : {}), ...(code ? { code } : {}) } });

class UpstreamError extends Error {
  constructor(status, type, code, message) { super(message); this.status = status; this.type = type; this.code = code; }
}

async function upstreamCall(path, payload, key, { stream = false, timeoutMs } = {}) {
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), timeoutMs || state.config.timeoutMs);
  try {
    const r = await fetch(`${BASE}${path}`, {
      method: 'POST',
      headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json', Accept: stream ? 'text/event-stream' : 'application/json' },
      body: JSON.stringify(payload),
      signal: ctrl.signal,
    });
    if (r.ok) return r;
    const text = await r.text();
    let parsed; try { parsed = JSON.parse(text); } catch {}
    const err = parsed?.error || {};
    throw new UpstreamError(r.status, err.type || 'invalid_request_error', err.code, err.message || text.slice(0, 300));
  } catch (e) {
    if (e instanceof UpstreamError) throw e;
    throw new UpstreamError(502, 'api_error', e.name === 'AbortError' ? 'upstream_timeout' : 'upstream_unreachable', e.name === 'AbortError' ? '上游超时' : e.message);
  } finally {
    clearTimeout(timer);
  }
}

/* ───────── 管理端鉴权 ───────── */

const tokens = new Map();
const adminToken = () => {
  const t = randomBytes(24).toString('base64url');
  tokens.set(t, Date.now() + 12 * 3600 * 1000);
  return t;
};
const adminOk = (req) => {
  const t = (req.headers.authorization || '').replace(/^Bearer\s+/i, '').replace(/^admin:/, '');
  const exp = tokens.get(t);
  if (!exp) return false;
  if (exp < Date.now()) { tokens.delete(t); return false; }
  return true;
};

/* ───────── 请求体 ───────── */

function readBody(req) {
  return new Promise((resolve, reject) => {
    const c = []; let n = 0;
    req.on('data', (x) => {
      n += x.length;
      if (n > MAX_BODY) { reject(Object.assign(new Error('请求体过大'), { status: 413 })); req.destroy(); return; }
      c.push(x);
    });
    req.on('end', () => resolve(Buffer.concat(c).toString('utf8')));
    req.on('error', reject);
  });
}

const resolveModel = (m) => {
  if (!m) return 'qwen3.8-flash';
  const k = String(m).trim().toLowerCase();
  return state.config.aliases[k] || String(m).trim();
};
const knownModel = (id) => NATIVE.includes(id) || Object.hasOwn(state.config.aliases, String(id).toLowerCase());
const modelCard = (id, owner = 'meoo') => ({ id, object: 'model', created: 0, owned_by: owner });

function shapeChat(body, overrideModel) {
  const out = { ...body };
  out.model = overrideModel || resolveModel(body.model);
  if (!Array.isArray(out.messages)) out.messages = [];
  if (out.max_completion_tokens !== undefined && out.max_tokens === undefined) {
    out.max_tokens = out.max_completion_tokens;
    delete out.max_completion_tokens;
  }
  if (out.max_tokens === undefined) out.max_tokens = state.config.defaultMaxTokens;
  if (out.stream !== true) delete out.stream_options;
  else if (out.stream_options && typeof out.stream_options === 'object') out.stream_options = { include_usage: true, ...out.stream_options };
  out.n = 1;
  return out;
}

async function chatWithN(payload, n, key) {
  const rs = await Promise.all(Array.from({ length: n }, () => upstreamCall('/chat/completions', payload, key).then((r) => r.json())));
  const first = rs[0];
  const sum = (k) => rs.reduce((a, d) => a + Number(d?.usage?.[k] || 0), 0);
  return {
    ...first,
    choices: rs.flatMap((d, i) => (d.choices || []).map((c) => ({ ...c, index: i }))),
    usage: first.usage ? { ...first.usage, prompt_tokens: sum('prompt_tokens'), completion_tokens: sum('completion_tokens'), total_tokens: sum('total_tokens') } : undefined,
  };
}

async function pipeSse(res, upstream, model) {
  res.writeHead(200, { 'Content-Type': 'text/event-stream; charset=utf-8', 'Cache-Control': 'no-cache, no-transform', Connection: 'keep-alive', 'X-Accel-Buffering': 'no', ...CORS });
  const reader = upstream.body.getReader();
  const dec = new TextDecoder();
  let buf = '';
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let i;
      while ((i = buf.indexOf('\n\n')) !== -1) {
        const evt = buf.slice(0, i); buf = buf.slice(i + 2);
        if (!evt.trim()) continue;
        const out = [];
        for (const line of evt.split('\n')) {
          if (!line.startsWith('data:')) { out.push(line); continue; }
          const d = line.slice(5).trim();
          if (d === '[DONE]') { out.push('data: [DONE]'); continue; }
          try { const o = JSON.parse(d); if (model && !o.model) o.model = model; out.push(`data: ${JSON.stringify(o)}`); }
          catch { out.push(line); }
        }
        res.write(out.join('\n') + '\n\n');
      }
    }
    if (buf.trim()) res.write(buf);
  } catch (e) {
    res.write(`data: ${JSON.stringify({ error: { message: e.message, type: 'api_error', code: 'stream_interrupted' } })}\n\n`);
  }
  res.end();
}

/* ───────── 探活 ───────── */

async function probeKey(k) {
  const t0 = Date.now();
  try {
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), 15000);
    const r = await fetch(`${BASE}/models`, { headers: { Authorization: `Bearer ${k.key}` }, signal: ctrl.signal });
    clearTimeout(timer);
    const ms = Date.now() - t0;
    if (r.ok) {
      k.health = { state: 'ok', checkedAt: Date.now(), latencyMs: ms, error: '' };
      k.cooldownUntil = 0;
    } else {
      const txt = await r.text();
      let msg = txt.slice(0, 160);
      try { msg = JSON.parse(txt)?.error?.message || msg; } catch {}
      k.health = { state: r.status === 401 || r.status === 403 ? 'invalid' : 'error', checkedAt: Date.now(), latencyMs: ms, error: `HTTP ${r.status} ${msg}` };
      if (r.status === 401 || r.status === 403) k.cooldownUntil = Date.now() + state.config.hardCooldownSec * 1000;
    }
  } catch (e) {
    k.health = { state: 'error', checkedAt: Date.now(), latencyMs: 0, error: e.message };
  }
  dirty = true;
  return k.health;
}

/* ───────── 管理 API ───────── */

async function adminRoute(req, res, p, url) {
  if (p === '/admin/api/login' && req.method === 'POST') {
    let body; try { body = JSON.parse((await readBody(req)) || '{}'); } catch { return sendJson(res, 400, { error: 'bad json' }); }
    const user = String(body.username || 'admin');
    const pass = String(body.password || '');
    if (user === state.config.adminUser && sha(pass) === state.config.adminPass) {
      return sendJson(res, 200, { ok: true, token: adminToken() });
    }
    return sendJson(res, 401, { ok: false, error: '用户名或密码错误' });
  }

  if (!adminOk(req)) return sendJson(res, 401, { error: 'unauthorized' });

  const viewKey = (k, reveal) => ({ ...k, key: reveal ? k.key : mask(k.key), hasKey: Boolean(k.key) });

  if (p === '/admin/api/state' && req.method === 'GET') {
    const reveal = url.searchParams.get('reveal') === '1';
    const now = Date.now();
    return sendJson(res, 200, {
      config: { ...state.config, adminPass: undefined },
      keys: state.keys.map((k) => viewKey(k, reveal)).map((k) => ({ ...k, cooling: Math.max(0, Math.ceil((k.cooldownUntil - now) / 1000)) })),
      clientKeys: state.clientKeys.map((c) => ({ ...c, key: reveal ? c.key : `${c.key.slice(0, 6)}…${c.key.slice(-4)}` })),
      models: NATIVE,
      alerts: buildAlerts(),
      uptime: process.uptime(),
    });
  }

  if (p === '/admin/api/keys' && req.method === 'POST') {
    let b; try { b = JSON.parse((await readBody(req)) || '{}'); } catch { return sendJson(res, 400, { error: 'bad json' }); }
    if (!BYOK_RE.test(String(b.key || '').trim())) return sendJson(res, 400, { error: '项目服务 AK 格式不对（meoo_project_…）' });
    const k = newKey({ label: b.label, key: b.key.trim(), weight: b.weight, note: b.note });
    state.keys.push(k); dirty = true; saveState(true);
    return sendJson(res, 200, viewKey(k, false));
  }

  const km = p.match(/^\/admin\/api\/keys\/([^/]+)(\/.*)?$/);
  if (km) {
    const k = state.keys.find((x) => x.id === km[1]);
    if (!k) return sendJson(res, 404, { error: 'not found' });
    const sub = km[2] || '';
    if (sub === '/probe' && req.method === 'POST') return sendJson(res, 200, await probeKey(k));
    if (sub === '/reveal' && req.method === 'GET') return sendJson(res, 200, { key: k.key });
    if (req.method === 'PATCH') {
      let b; try { b = JSON.parse((await readBody(req)) || '{}'); } catch { return sendJson(res, 400, { error: 'bad json' }); }
      for (const f of ['label', 'note', 'weight', 'enabled']) if (b[f] !== undefined) k[f] = b[f];
      if (b.key !== undefined && BYOK_RE.test(String(b.key))) k.key = String(b.key).trim();
      if (b.resetCooldown) k.cooldownUntil = 0;
      dirty = true; saveState(true);
      return sendJson(res, 200, viewKey(k, false));
    }
    if (req.method === 'DELETE') {
      state.keys = state.keys.filter((x) => x.id !== k.id); dirty = true; saveState(true);
      return sendJson(res, 200, { ok: true });
    }
  }

  if (p === '/admin/api/probe-all' && req.method === 'POST') {
    const results = await Promise.all(state.keys.map(async (k) => ({ id: k.id, label: k.label, ...(await probeKey(k)) })));
    saveState(true);
    return sendJson(res, 200, { results });
  }

  if (p === '/admin/api/client-keys' && req.method === 'POST') {
    let b; try { b = JSON.parse((await readBody(req)) || '{}'); } catch { return sendJson(res, 400, { error: 'bad json' }); }
    const c = newClientKey({ label: b.label, key: b.key });
    state.clientKeys.push(c); dirty = true; saveState(true);
    return sendJson(res, 200, { ...c, key: c.key });
  }
  const crm = p.match(/^\/admin\/api\/client-keys\/([^/]+)\/reveal$/);
  if (crm && req.method === 'GET') {
    const c = state.clientKeys.find((x) => x.id === crm[1]);
    return c ? sendJson(res, 200, { key: c.key }) : sendJson(res, 404, { error: 'not found' });
  }
  const cm = p.match(/^\/admin\/api\/client-keys\/([^/]+)$/);
  if (cm) {
    const c = state.clientKeys.find((x) => x.id === cm[1]);
    if (!c) return sendJson(res, 404, { error: 'not found' });
    if (req.method === 'PATCH') {
      let b; try { b = JSON.parse((await readBody(req)) || '{}'); } catch { return sendJson(res, 400, { error: 'bad json' }); }
      for (const f of ['label', 'enabled']) if (b[f] !== undefined) c[f] = b[f];
      dirty = true; saveState(true);
      return sendJson(res, 200, { ...c, key: `${c.key.slice(0, 6)}…${c.key.slice(-4)}` });
    }
    if (req.method === 'DELETE') {
      state.clientKeys = state.clientKeys.filter((x) => x.id !== c.id); dirty = true; saveState(true);
      return sendJson(res, 200, { ok: true });
    }
  }

  if (p === '/admin/api/aliases') {
    if (req.method === 'POST') {
      let b; try { b = JSON.parse((await readBody(req)) || '{}'); } catch { return sendJson(res, 400, { error: 'bad json' }); }
      const name = String(b.name || '').trim().toLowerCase(), target = String(b.target || '').trim();
      if (!name || !target) return sendJson(res, 400, { error: '缺 name 或 target' });
      state.config.aliases[name] = target; dirty = true; saveState(true);
      return sendJson(res, 200, { ok: true });
    }
    if (req.method === 'DELETE') {
      const name = String(url.searchParams.get('name') || '').toLowerCase();
      delete state.config.aliases[name]; dirty = true; saveState(true);
      return sendJson(res, 200, { ok: true });
    }
  }

  if (p === '/admin/api/config' && req.method === 'PATCH') {
    let b; try { b = JSON.parse((await readBody(req)) || '{}'); } catch { return sendJson(res, 400, { error: 'bad json' }); }
    const cfg = state.config;
    for (const f of ['timeoutMs', 'defaultMaxTokens', 'cooldownSec', 'hardCooldownSec', 'maxLogs', 'maxUsageHours']) {
      if (b[f] !== undefined && Number.isFinite(Number(b[f]))) cfg[f] = Number(b[f]);
    }
    if (b.sticky !== undefined) cfg.sticky = Boolean(b.sticky);
    if (Array.isArray(b.hiddenModels)) cfg.hiddenModels = b.hiddenModels.map(String);
    if (b.adminPassword) { cfg.adminPass = sha(String(b.adminPassword)); tokens.clear(); }
    dirty = true; saveState(true);
    return sendJson(res, 200, { ok: true });
  }

  if (p === '/admin/api/logs' && req.method === 'GET') {
    const limit = Math.min(500, Number(url.searchParams.get('limit') || 100));
    const status = url.searchParams.get('status');
    const model = url.searchParams.get('model');
    const q = url.searchParams.get('q');
    let rows = state.logs;
    if (status === 'ok') rows = rows.filter((r) => r.status < 400);
    if (status === 'fail') rows = rows.filter((r) => r.status >= 400);
    if (model) rows = rows.filter((r) => r.model === model);
    if (q) rows = rows.filter((r) => (r.rid || '').includes(q) || (r.keyLabel || '').includes(q));
    return sendJson(res, 200, { total: rows.length, rows: rows.slice(0, limit) });
  }

  if (p === '/admin/api/usage' && req.method === 'GET') {
    const hours = Object.keys(state.usage).sort().slice(-Number(url.searchParams.get('hours') || 24));
    return sendJson(res, 200, {
      hours: hours.map((h) => {
        const u = state.usage[h];
        return { hour: h, req: u.req, ok: u.ok, fail: u.fail, pt: u.pt, ct: u.ct, p50: pct(u.lat, 50), p95: pct(u.lat, 95), models: u.models };
      }),
    });
  }

  if (p === '/admin/api/export' && req.method === 'GET') {
    return sendJson(res, 200, { ...state, config: { ...state.config, adminPass: undefined } });
  }

  return sendJson(res, 404, { error: 'unknown admin path' });
}

function buildAlerts() {
  const out = [];
  const now = Date.now();
  const bad = state.keys.filter((k) => k.health.state === 'invalid');
  if (bad.length) out.push({ level: 'bad', text: `${bad.length} 个 key 鉴权失败：${bad.map((k) => k.label).join(', ')}` });
  const cooling = state.keys.filter((k) => k.cooldownUntil > now);
  if (cooling.length) out.push({ level: 'warn', text: `${cooling.length} 个 key 处于冷却中` });
  if (!usableKeys().length) out.push({ level: 'bad', text: '没有可用 key，请求会失败' });
  if (!state.keys.length) out.push({ level: 'warn', text: 'key 池为空，只能 BYOK' });
  const recent = state.logs.slice(0, 20);
  const fails = recent.filter((r) => r.status >= 500).length;
  if (fails >= 3) out.push({ level: 'warn', text: `最近 20 次请求有 ${fails} 次 5xx` });
  return out;
}

/* ───────── OpenAI 路由 ───────── */

function clientKeyLookup(tok) {
  return state.clientKeys.find((c) => c.enabled && c.key === tok);
}

function resolveCaller(req) {
  const tok = (req.headers.authorization || '').replace(/^Bearer\s+/i, '').trim();
  if (!tok) return { error: '缺少 Authorization 头' };
  if (BYOK_RE.test(tok)) return { byok: tok, label: 'BYOK' };
  if (tok.startsWith('meoo_ak_')) return { error: '个人 AK 不能调用模型网关，请用项目服务 AK（meoo_project_…）' };
  const ck = clientKeyLookup(tok);
  if (ck) return { clientKey: ck, label: ck.label };
  if (!state.keys.length && !state.clientKeys.length) return { error: '服务未配置 key（管理台添加 key 或使用 BYOK）' };
  return { error: '无效的 API key' };
}

function takeKeyFor(caller) {
  if (caller.byok) return { key: caller.byok, entry: null, label: 'BYOK' };
  const k = pickKey();
  if (!k) return null;
  return { key: k.key, entry: k, label: k.label };
}

const server = createServer(async (req, res) => {
  const rid = req.headers['x-request-id'] || randomUUID();
  const t0 = Date.now();
  const url = new URL(req.url || '/', `http://${BIND}`);
  const p = url.pathname.replace(/\/+$/, '') || '/';
  res.setHeader('X-Request-Id', rid);

  if (req.method === 'OPTIONS') { res.writeHead(204, CORS); return res.end(); }

  const finish = (status, extra = {}) => record({ rid, method: req.method, path: p, status, ms: Date.now() - t0, ...extra });

  try {
    if ((req.method === 'GET' || req.method === 'HEAD') && (p === '/' || p === '/webui.html' || p === '/index.html')) {
      const html = readFileSync(join(HERE, 'webui.html'), 'utf8');
      res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8', 'Cache-Control': 'no-store' });
      return res.end(html);
    }

    if (p === '/healthz') {
      const usable = usableKeys();
      return sendJson(res, 200, {
        ok: true, upstream: BASE, byok: true,
        keys: state.keys.length, usable: usable.length, clientKeys: state.clientKeys.length,
        models: NATIVE.length, uptime: Math.round(process.uptime()),
        alerts: buildAlerts(),
      });
    }

    if (p.startsWith('/admin/api/')) return adminRoute(req, res, p, url);

    /* ---- OpenAI ---- */
    const caller = resolveCaller(req);
    if (caller.error) {
      finish(401);
      return oaError(res, 401, caller.error, 'authentication_error', 'invalid_api_key');
    }
    const target = takeKeyFor(caller);
    if (!target) {
      finish(503);
      return oaError(res, 503, '当前没有可用 key（全部禁用或在冷却中）', 'api_error', 'no_available_key');
    }

    if (req.method === 'GET' && (p === '/v1/models' || p === '/models')) {
      finish(200);
      const hidden = new Set(state.config.hiddenModels);
      return sendJson(res, 200, {
        object: 'list',
        data: [
          ...NATIVE.filter((m) => !hidden.has(m)).map((id) => modelCard(id)),
          ...Object.keys(state.config.aliases).filter((m) => !hidden.has(m)).map((id) => modelCard(id, 'meoo-alias')),
        ],
      });
    }

    const mm = p.match(/^\/v1\/models\/(.+)$/);
    if (req.method === 'GET' && mm) {
      const id = decodeURIComponent(mm[1]);
      if (!knownModel(id) || state.config.hiddenModels.includes(id)) {
        finish(404);
        return oaError(res, 404, `模型 ${id} 不存在`, 'invalid_request_error', 'model_not_found', 'model');
      }
      finish(200);
      return sendJson(res, 200, modelCard(NATIVE.includes(id) ? id : state.config.aliases[id.toLowerCase()], NATIVE.includes(id) ? 'meoo' : 'meoo-alias'));
    }

    if (req.method === 'POST' && (p === '/v1/chat/completions' || p === '/chat/completions')) {
      let body;
      try { body = JSON.parse((await readBody(req)) || '{}'); } catch { finish(400); return oaError(res, 400, '请求体不是合法 JSON'); }
      if (!Array.isArray(body.messages) || !body.messages.length) {
        finish(400);
        return oaError(res, 400, "'messages' is a required property", 'invalid_request_error', 'missing_messages', 'messages');
      }
      const n = Math.max(1, Math.min(Number(body.n) || 1, 4));
      const payload = shapeChat(body);
      if (n > 1 && payload.stream === true) {
        finish(400);
        return oaError(res, 400, 'n>1 不支持 stream=true', 'invalid_request_error', 'invalid_parameter_error', 'n');
      }
      try {
        if (n > 1) {
          const merged = await chatWithN(payload, n, target.key);
          markKey(target.entry, true);
          finish(200, { model: payload.model, pt: merged.usage?.prompt_tokens, ct: merged.usage?.completion_tokens, keyLabel: target.label, byok: Boolean(caller.byok) });
          merged.model ||= payload.model;
          return sendJson(res, 200, merged);
        }
        const r = await upstreamCall('/chat/completions', payload, target.key, { stream: payload.stream === true });
        markKey(target.entry, true);
        if (payload.stream === true) {
          finish(200, { model: payload.model, keyLabel: target.label, byok: Boolean(caller.byok) });
          return pipeSse(res, r, payload.model);
        }
        const data = await r.json();
        data.model ||= payload.model;
        finish(200, { model: data.model, pt: data.usage?.prompt_tokens, ct: data.usage?.completion_tokens, keyLabel: target.label, byok: Boolean(caller.byok) });
        return sendJson(res, 200, data);
      } catch (e) {
        markKey(target.entry, false, { soft: e.status === 429 || e.status >= 500 });
        finish(e.status || 502, { model: payload.model, keyLabel: target.label, error: e.message });
        return oaError(res, e.status || 502, e.message, e.type, e.code);
      }
    }

    if (req.method === 'POST' && (p === '/v1/completions' || p === '/completions')) {
      let body;
      try { body = JSON.parse((await readBody(req)) || '{}'); } catch { finish(400); return oaError(res, 400, '请求体不是合法 JSON'); }
      const prompt = Array.isArray(body.prompt) ? body.prompt.join('\n') : String(body.prompt ?? '');
      if (!prompt) { finish(400); return oaError(res, 400, "'prompt' is a required property", 'invalid_request_error', 'missing_prompt', 'prompt'); }
      const payload = shapeChat({ model: body.model, messages: [{ role: 'user', content: prompt }], max_tokens: body.max_tokens, temperature: body.temperature, top_p: body.top_p, stop: body.stop });
      try {
        const data = await (await upstreamCall('/chat/completions', payload, target.key)).json();
        markKey(target.entry, true);
        finish(200, { model: payload.model, pt: data.usage?.prompt_tokens, ct: data.usage?.completion_tokens, keyLabel: target.label, byok: Boolean(caller.byok) });
        return sendJson(res, 200, {
          id: data.id || `cmpl-${randomUUID()}`, object: 'text_completion',
          created: data.created || Math.floor(Date.now() / 1000), model: data.model || payload.model,
          choices: (data.choices || []).map((c) => ({ text: c.message?.content ?? '', index: c.index ?? 0, logprobs: null, finish_reason: c.finish_reason ?? 'stop' })),
          usage: data.usage,
        });
      } catch (e) {
        markKey(target.entry, false, { soft: e.status === 429 || e.status >= 500 });
        finish(e.status || 502, { keyLabel: target.label });
        return oaError(res, e.status || 502, e.message, e.type, e.code);
      }
    }

    if (req.method === 'POST' && (p === '/v1/responses' || p === '/responses')) {
      let body;
      try { body = JSON.parse((await readBody(req)) || '{}'); } catch { finish(400); return oaError(res, 400, '请求体不是合法 JSON'); }
      const input = body.input;
      const messages = Array.isArray(input)
        ? input.map((m) => (typeof m === 'string' ? { role: 'user', content: m } : { role: m.role || 'user', content: typeof m.content === 'string' ? m.content : (m.content || []).map((c) => c.text || '').join('') }))
        : [{ role: 'user', content: String(input ?? '') }];
      if (typeof body.instructions === 'string') messages.unshift({ role: 'system', content: body.instructions });
      const payload = shapeChat({ model: body.model, messages, max_tokens: body.max_output_tokens });
      try {
        const data = await (await upstreamCall('/chat/completions', payload, target.key)).json();
        markKey(target.entry, true);
        finish(200, { model: payload.model, pt: data.usage?.prompt_tokens, ct: data.usage?.completion_tokens, keyLabel: target.label, byok: Boolean(caller.byok) });
        const text = data.choices?.[0]?.message?.content ?? '';
        return sendJson(res, 200, {
          id: `resp_${randomUUID().replace(/-/g, '')}`, object: 'response',
          created_at: data.created || Math.floor(Date.now() / 1000), status: 'completed',
          model: data.model || payload.model,
          output: [{ id: `msg_${randomUUID().replace(/-/g, '')}`, type: 'message', role: 'assistant', status: 'completed', content: [{ type: 'output_text', text, annotations: [] }] }],
          output_text: text,
          usage: data.usage ? { input_tokens: data.usage.prompt_tokens, output_tokens: data.usage.completion_tokens, total_tokens: data.usage.total_tokens } : undefined,
        });
      } catch (e) {
        markKey(target.entry, false, { soft: e.status === 429 || e.status >= 500 });
        finish(e.status || 502, { keyLabel: target.label });
        return oaError(res, e.status || 502, e.message, e.type, e.code);
      }
    }

    if (req.method === 'POST' && ['/v1/embeddings', '/embeddings', '/v1/audio/speech', '/v1/images/generations', '/v1/moderations'].includes(p)) {
      finish(404);
      return oaError(res, 404, `上游不提供 ${p}`, 'invalid_request_error', 'unsupported_endpoint');
    }

    finish(404);
    return oaError(res, 404, `未知路径 ${req.method} ${p}`, 'invalid_request_error', 'unknown_url');
  } catch (e) {
    finish(500, { error: e.message });
    if (!res.headersSent) sendJson(res, 500, { error: { message: e.message, type: 'api_error' } });
  }
});

server.listen(PORT, BIND, () => {
  const pwFile = join(DATA, 'admin-password.txt');
  console.log(`meoo2api   http://${BIND}:${PORT}/`);
  console.log(`  upstream ${BASE}`);
  console.log(`  keys     ${state.keys.length} 池内 / ${state.clientKeys.length} 客户端`);
  console.log(`  admin    ${existsSync(pwFile) ? '密码见 ' + pwFile : 'env MEOO2API_ADMIN_PASSWORD'}`);
  console.log(`  log      ${LOG}`);
});

for (const sig of ['SIGINT', 'SIGTERM']) {
  process.on(sig, () => { saveState(true); server.close(() => process.exit(0)); setTimeout(() => process.exit(0), 4000).unref(); });
}
