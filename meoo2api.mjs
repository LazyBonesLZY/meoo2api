#!/usr/bin/env node
// meoo2api — Meoo 模型网关的 OpenAI 兼容层
// 上游: https://api.meoo.host/meoo-ai/compatible-mode/v1 (项目服务 AK)
import { createServer } from 'node:http';
import { appendFileSync, existsSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { randomUUID, timingSafeEqual, createHash } from 'node:crypto';

const HERE = dirname(fileURLToPath(import.meta.url));

if (existsSync(join(HERE, '.env'))) {
  for (const line of readFileSync(join(HERE, '.env'), 'utf8').split(/\r?\n/)) {
    const m = line.match(/^\s*([A-Z0-9_]+)\s*=\s*(.*?)\s*$/);
    if (m && process.env[m[1]] === undefined) process.env[m[1]] = m[2].replace(/^["']|["']$/g, '');
  }
}

const AK = process.env.MEOO_PROJECT_AK || '';
const BASE = (process.env.MEOO_AI_BASE || 'https://api.meoo.host/meoo-ai/compatible-mode/v1').replace(/\/+$/, '');
const KEYS = (process.env.MEOO2API_KEYS || '').split(',').map((s) => s.trim()).filter(Boolean);
const PORT = Number(process.env.PORT || 8791);
const BIND = process.env.BIND || '127.0.0.1';
const TIMEOUT = Number(process.env.UPSTREAM_TIMEOUT || 180000);
const MAX_BODY = Number(process.env.MAX_BODY_BYTES || 8 * 1024 * 1024);
const LOG = process.env.LOG || join(HERE, 'meoo2api.log');

if (!AK) {
  console.error('缺少 MEOO_PROJECT_AK，例如: MEOO_PROJECT_AK=meoo_project_xxx node meoo2api.mjs');
  process.exit(1);
}

const NATIVE = [
  'qwen3.8-flash', 'qwen3.8-max', 'qwen3.7-max', 'qwen3.7-plus',
  'deepseek-v4.1-flash-1017', 'deepseek-v4-pro-0813', 'deepseek-v4-flash-0731',
  'glm-5.2-fast-preview', 'glm-5.2',
];

const ALIASES = {
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

const DEFAULT_MODEL = 'qwen3.8-flash';
const resolveModel = (m) => (m ? ALIASES[String(m).trim().toLowerCase()] || String(m).trim() : DEFAULT_MODEL);
const knownModel = (id) => NATIVE.includes(id) || Object.hasOwn(ALIASES, String(id).toLowerCase());

let logOk = true;
try { writeFileSync(LOG, ''); } catch { logOk = false; }
const log = (o) => { if (logOk) { try { appendFileSync(LOG, JSON.stringify(o) + '\n'); } catch { logOk = false; } } };

const CORS = {
  'Access-Control-Allow-Origin': '*',
  'Access-Control-Allow-Headers': 'Authorization, Content-Type, OpenAI-Beta, X-Request-Id',
  'Access-Control-Allow-Methods': 'GET, POST, OPTIONS',
  'Access-Control-Max-Age': '86400',
};

function sendJson(res, status, obj) {
  const body = JSON.stringify(obj);
  res.writeHead(status, {
    'Content-Type': 'application/json; charset=utf-8',
    'Content-Length': Buffer.byteLength(body),
    'Cache-Control': 'no-store',
    ...CORS,
  });
  res.end(body);
}

const oaError = (res, status, message, type = 'invalid_request_error', code, param) =>
  sendJson(res, status, { error: { message, type, ...(param ? { param } : {}), ...(code ? { code } : {}) } });

function clientAuth(req) {
  if (!KEYS.length) return { ok: true };
  const tok = (req.headers.authorization || '').replace(/^Bearer\s+/i, '').trim();
  if (!tok) return { ok: false, message: '缺少 Authorization 头' };
  const h = (s) => createHash('sha256').update(s).digest();
  return KEYS.some((k) => timingSafeEqual(h(k), h(tok))) ? { ok: true } : { ok: false, message: '无效的 API key' };
}

function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    let size = 0;
    req.on('data', (c) => {
      size += c.length;
      if (size > MAX_BODY) {
        reject(Object.assign(new Error('请求体过大'), { status: 413 }));
        req.destroy();
        return;
      }
      chunks.push(c);
    });
    req.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
    req.on('error', reject);
  });
}

class UpstreamError extends Error {
  constructor(status, type, code, message) {
    super(message);
    this.status = status;
    this.type = type;
    this.code = code;
  }
}

async function callUpstream(path, payload, { stream = false } = {}) {
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), TIMEOUT);
  let lastErr;
  try {
    for (let attempt = 0; attempt < 2; attempt++) {
      try {
        const r = await fetch(`${BASE}${path}`, {
          method: 'POST',
          headers: {
            Authorization: `Bearer ${AK}`,
            'Content-Type': 'application/json',
            Accept: stream ? 'text/event-stream' : 'application/json',
          },
          body: JSON.stringify(payload),
          signal: ctrl.signal,
        });
        if (r.ok) return r;
        const text = await r.text();
        let parsed;
        try { parsed = JSON.parse(text); } catch { /* 非 JSON 错误体 */ }
        const err = parsed?.error || {};
        if ((r.status === 429 || r.status >= 500) && attempt === 0) {
          const ra = Number(r.headers.get('retry-after'));
          await new Promise((s) => setTimeout(s, Number.isFinite(ra) ? Math.min(ra * 1000, 3000) : 600));
          lastErr = new UpstreamError(r.status, err.type || 'upstream_error', err.code, err.message || text.slice(0, 300));
          continue;
        }
        throw new UpstreamError(
          r.status,
          err.type || (r.status === 401 ? 'authentication_error' : 'invalid_request_error'),
          err.code,
          err.message || text.slice(0, 300) || `上游 HTTP ${r.status}`,
        );
      } catch (e) {
        if (e instanceof UpstreamError) throw e;
        lastErr = new UpstreamError(
          502,
          'api_error',
          e.name === 'AbortError' ? 'upstream_timeout' : 'upstream_unreachable',
          e.name === 'AbortError' ? `上游超时(${TIMEOUT}ms)` : `上游请求失败: ${e.message}`,
        );
      }
    }
    throw lastErr;
  } finally {
    clearTimeout(timer);
  }
}

// 上游只接受 n=1，且 stream_options 必须与 stream 成对
function shapeChat(body, overrideModel) {
  const out = { ...body };
  out.model = overrideModel || resolveModel(body.model);
  if (!Array.isArray(out.messages)) out.messages = [];
  if (out.max_completion_tokens !== undefined && out.max_tokens === undefined) {
    out.max_tokens = out.max_completion_tokens;
    delete out.max_completion_tokens;
  }
  if (out.stream !== true) delete out.stream_options;
  else if (out.stream_options && typeof out.stream_options === 'object') {
    out.stream_options = { include_usage: true, ...out.stream_options };
  }
  out.n = 1;
  return out;
}

async function chatWithN(payload, n) {
  const results = await Promise.all(
    Array.from({ length: n }, () => callUpstream('/chat/completions', payload).then((r) => r.json())),
  );
  const first = results[0];
  const sum = (k) => results.reduce((a, d) => a + Number(d?.usage?.[k] || 0), 0);
  return {
    ...first,
    choices: results.flatMap((d, i) => (d.choices || []).map((c) => ({ ...c, index: i }))),
    usage: first.usage
      ? { ...first.usage, prompt_tokens: sum('prompt_tokens'), completion_tokens: sum('completion_tokens'), total_tokens: sum('total_tokens') }
      : undefined,
  };
}

const modelCard = (id, owner = 'meoo') => ({ id, object: 'model', created: 0, owned_by: owner });

async function pipeSse(res, upstream, model) {
  res.writeHead(200, {
    'Content-Type': 'text/event-stream; charset=utf-8',
    'Cache-Control': 'no-cache, no-transform',
    Connection: 'keep-alive',
    'X-Accel-Buffering': 'no',
    ...CORS,
  });
  const reader = upstream.body.getReader();
  const dec = new TextDecoder();
  let buf = '';
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let idx;
      while ((idx = buf.indexOf('\n\n')) !== -1) {
        const evt = buf.slice(0, idx);
        buf = buf.slice(idx + 2);
        if (!evt.trim()) continue;
        const out = [];
        for (const line of evt.split('\n')) {
          if (!line.startsWith('data:')) { out.push(line); continue; }
          const data = line.slice(5).trim();
          if (data === '[DONE]') { out.push('data: [DONE]'); continue; }
          try {
            const obj = JSON.parse(data);
            if (model && !obj.model) obj.model = model;
            out.push(`data: ${JSON.stringify(obj)}`);
          } catch { out.push(line); }
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

const server = createServer(async (req, res) => {
  const rid = req.headers['x-request-id'] || randomUUID();
  const t0 = Date.now();
  const p = new URL(req.url || '/', `http://${BIND}`).pathname.replace(/\/+$/, '') || '/';
  res.setHeader('X-Request-Id', rid);
  res.on('finish', () => log({ ts: new Date().toISOString(), rid, method: req.method, path: p, status: res.statusCode, ms: Date.now() - t0 }));

  if (req.method === 'OPTIONS') {
    res.writeHead(204, CORS);
    return res.end();
  }

  if (p === '/healthz') {
    let status = 'unknown';
    try {
      const ctrl = new AbortController();
      const t = setTimeout(() => ctrl.abort(), 8000);
      const r = await fetch(`${BASE}/models`, { headers: { Authorization: `Bearer ${AK}` }, signal: ctrl.signal });
      clearTimeout(t);
      status = `http ${r.status}`;
    } catch (e) { status = `error: ${e.message}`; }
    return sendJson(res, 200, { ok: true, upstream: BASE, upstreamStatus: status, keysRequired: KEYS.length > 0 });
  }

  const auth = clientAuth(req);
  if (!auth.ok) return oaError(res, 401, auth.message, 'authentication_error', 'invalid_api_key');

  if (req.method === 'GET' && (p === '/v1/models' || p === '/models')) {
    return sendJson(res, 200, {
      object: 'list',
      data: [
        ...NATIVE.map((id) => modelCard(id)),
        ...Object.keys(ALIASES).map((id) => modelCard(id, 'meoo-alias')),
      ],
    });
  }

  const mm = p.match(/^\/v1\/models\/(.+)$/);
  if (req.method === 'GET' && mm) {
    const id = decodeURIComponent(mm[1]);
    if (!knownModel(id)) return oaError(res, 404, `模型 ${id} 不存在`, 'invalid_request_error', 'model_not_found', 'model');
    return sendJson(res, 200, modelCard(NATIVE.includes(id) ? id : ALIASES[id.toLowerCase()], NATIVE.includes(id) ? 'meoo' : 'meoo-alias'));
  }

  if (req.method === 'POST' && (p === '/v1/chat/completions' || p === '/chat/completions')) {
    let body;
    try { body = JSON.parse((await readBody(req)) || '{}'); }
    catch (e) { return oaError(res, e.status || 400, e.status ? e.message : '请求体不是合法 JSON'); }
    if (!Array.isArray(body.messages) || !body.messages.length) {
      return oaError(res, 400, "'messages' is a required property", 'invalid_request_error', 'missing_messages', 'messages');
    }
    const n = Math.max(1, Math.min(Number(body.n) || 1, 4));
    const payload = shapeChat(body);
    if (n > 1) {
      if (payload.stream === true) {
        return oaError(res, 400, 'n>1 不支持 stream=true', 'invalid_request_error', 'invalid_parameter_error', 'n');
      }
      try {
        const merged = await chatWithN(payload, n);
        merged.model ||= payload.model;
        return sendJson(res, 200, merged);
      } catch (e) { return oaError(res, e.status || 502, e.message, e.type, e.code); }
    }
    let r;
    try { r = await callUpstream('/chat/completions', payload, { stream: payload.stream === true }); }
    catch (e) { return oaError(res, e.status || 502, e.message, e.type, e.code); }
    if (payload.stream === true) return pipeSse(res, r, payload.model);
    const data = await r.json();
    data.model ||= payload.model;
    return sendJson(res, 200, data);
  }

  if (req.method === 'POST' && (p === '/v1/completions' || p === '/completions')) {
    let body;
    try { body = JSON.parse((await readBody(req)) || '{}'); }
    catch (e) { return oaError(res, e.status || 400, e.status ? e.message : '请求体不是合法 JSON'); }
    const prompt = Array.isArray(body.prompt) ? body.prompt.join('\n') : String(body.prompt ?? '');
    if (!prompt) return oaError(res, 400, "'prompt' is a required property", 'invalid_request_error', 'missing_prompt', 'prompt');
    const payload = shapeChat({
      model: body.model,
      messages: [{ role: 'user', content: prompt }],
      max_tokens: body.max_tokens,
      temperature: body.temperature,
      top_p: body.top_p,
      stop: body.stop,
    });
    const n = Math.max(1, Math.min(Number(body.n) || 1, 4));
    let data;
    try {
      data = n > 1 ? await chatWithN(payload, n) : await (await callUpstream('/chat/completions', payload)).json();
    } catch (e) { return oaError(res, e.status || 502, e.message, e.type, e.code); }
    return sendJson(res, 200, {
      id: data.id || `cmpl-${randomUUID()}`,
      object: 'text_completion',
      created: data.created || Math.floor(Date.now() / 1000),
      model: data.model || payload.model,
      choices: (data.choices || []).map((c) => ({
        text: c.message?.content ?? '',
        index: c.index ?? 0,
        logprobs: null,
        finish_reason: c.finish_reason ?? 'stop',
      })),
      usage: data.usage,
    });
  }

  if (req.method === 'POST' && (p === '/v1/responses' || p === '/responses')) {
    let body;
    try { body = JSON.parse((await readBody(req)) || '{}'); }
    catch (e) { return oaError(res, e.status || 400, e.status ? e.message : '请求体不是合法 JSON'); }
    const input = body.input;
    const messages = Array.isArray(input)
      ? input.map((m) => (typeof m === 'string'
        ? { role: 'user', content: m }
        : { role: m.role || 'user', content: typeof m.content === 'string' ? m.content : (m.content || []).map((c) => c.text || '').join('') }))
      : [{ role: 'user', content: String(input ?? '') }];
    if (typeof body.instructions === 'string') messages.unshift({ role: 'system', content: body.instructions });
    const payload = shapeChat({ model: body.model, messages, max_tokens: body.max_output_tokens });
    let data;
    try { data = await (await callUpstream('/chat/completions', payload)).json(); }
    catch (e) { return oaError(res, e.status || 502, e.message, e.type, e.code); }
    const text = data.choices?.[0]?.message?.content ?? '';
    return sendJson(res, 200, {
      id: `resp_${randomUUID().replace(/-/g, '')}`,
      object: 'response',
      created_at: data.created || Math.floor(Date.now() / 1000),
      status: 'completed',
      model: data.model || payload.model,
      output: [{
        id: `msg_${randomUUID().replace(/-/g, '')}`,
        type: 'message',
        role: 'assistant',
        status: 'completed',
        content: [{ type: 'output_text', text, annotations: [] }],
      }],
      output_text: text,
      usage: data.usage
        ? { input_tokens: data.usage.prompt_tokens, output_tokens: data.usage.completion_tokens, total_tokens: data.usage.total_tokens }
        : undefined,
    });
  }

  if (req.method === 'POST' && ['/v1/embeddings', '/embeddings', '/v1/audio/speech', '/v1/images/generations', '/v1/moderations'].includes(p)) {
    return oaError(res, 404, `上游不提供 ${p}`, 'invalid_request_error', 'unsupported_endpoint');
  }

  return oaError(res, 404, `未知路径 ${req.method} ${p}`, 'invalid_request_error', 'unknown_url');
});

server.listen(PORT, BIND, () => {
  console.log(`meoo2api  http://${BIND}:${PORT}/v1`);
  console.log(`  upstream ${BASE}`);
  console.log(`  auth     ${KEYS.length ? `${KEYS.length} key(s)` : 'open'}`);
  console.log(`  log      ${LOG}`);
});

for (const sig of ['SIGINT', 'SIGTERM']) {
  process.on(sig, () => {
    server.close(() => process.exit(0));
    setTimeout(() => process.exit(0), 5000).unref();
  });
}
