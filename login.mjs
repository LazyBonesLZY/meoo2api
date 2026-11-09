#!/usr/bin/env node
// login.mjs — 打开浏览器完成 Meoo 手机号登录（阿里云 SSO），用 CDP 取 Cookie 并验证
// 用法: node login.mjs [--api URL] [--password PW] [--label 名称] [--keep] [--headless]
import { spawn } from 'node:child_process';
import { existsSync, mkdirSync, rmSync } from 'node:fs';
import { homedir } from 'node:os';
import { join } from 'node:path';

const arg = (n, d) => {
  const i = process.argv.indexOf('--' + n);
  return i > -1 ? process.argv[i + 1] : d;
};
const flag = (n) => process.argv.includes('--' + n);

const CDP_PORT = Number(arg('cdp-port', 9223));
const PROFILE = arg('profile', join(homedir(), '.meoo-login-profile'));
const LABEL = arg('label', '手机号登录');
const STDOUT_ONLY = flag('stdout');
const IMPORT_TOKEN = arg('token', '');
const USE_OAUTH = flag('oauth');
const OAUTH_PORT = Number(arg('oauth-port', 18765));
const API = (arg('api', '') || '').replace(/\/+$/, '');
const ADMIN_PW = arg('password', process.env.MEOO2API_ADMIN_PASSWORD || '');
const USE_SSO = !process.argv.includes('--no-sso');
const TIMEOUT_MS = Number(arg('timeout', 300000));

const CANDIDATES = ['/usr/bin/google-chrome', '/usr/bin/google-chrome-stable', '/usr/bin/chromium',
  '/usr/bin/chromium-browser', '/snap/bin/chromium'];
const CHROME = process.env.CHROME_BIN
  || CANDIDATES.find((p) => existsSync(p))
  || 'google-chrome';

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const log = (...a) => console.log(...a);

if (flag('fresh') && existsSync(PROFILE)) rmSync(PROFILE, { recursive: true, force: true });
mkdirSync(PROFILE, { recursive: true });

async function ssoUrl() {
  // 直接走阿里云 SSO 授权页，省掉 meoo 首页一跳
  const r = await fetch('https://meoo.com/api/auth/login_redirect?redirect=%2F', { redirect: 'manual' });
  const loc = r.headers.get('location');
  if (loc) return loc;
  const body = await r.text().catch(() => '');
  const m = body.match(/https:\/\/account\.aliyun\.com[^"'\s]+/);
  return m ? m[0] : 'https://meoo.com/';
}

const START = await (async () => {
  const explicit = arg('url', '');
  if (explicit) return explicit;
  if (!USE_SSO) return 'https://meoo.com/';
  try {
    const u = await ssoUrl();
    console.log('SSO 授权地址: ' + u.slice(0, 120) + '…');
    return u;
  } catch {
    return 'https://meoo.com/';
  }
})();

const args = [
  `--remote-debugging-port=${CDP_PORT}`,
  `--user-data-dir=${PROFILE}`,
  '--no-first-run',
  '--no-default-browser-check',
  '--disable-features=Translate',
  flag('headless') ? '--headless=new' : '--start-maximized',
  START,
];

const chrome = spawn(CHROME, args, { stdio: 'ignore', detached: false });
chrome.on('error', (e) => { console.error('启动 Chrome 失败:', e.message); process.exit(1); });

let ws = null;
let seq = 0;
const pending = new Map();

function send(method, params = {}) {
  return new Promise((resolve, reject) => {
    const id = ++seq;
    pending.set(id, { resolve, reject });
    ws.send(JSON.stringify({ id, method, params }));
    setTimeout(() => {
      if (pending.has(id)) { pending.delete(id); reject(new Error(method + ' timeout')); }
    }, 15000).unref();
  });
}

async function connect() {
  for (let i = 0; i < 60; i++) {
    try {
      const r = await fetch(`http://127.0.0.1:${CDP_PORT}/json/list`);
      const list = await r.json();
      const page = list.find((t) => t.type === 'page');
      if (page?.webSocketDebuggerUrl) {
        ws = new WebSocket(page.webSocketDebuggerUrl);
        await new Promise((res, rej) => { ws.onopen = res; ws.onerror = () => rej(new Error('ws error')); });
        ws.onmessage = (ev) => {
          const msg = JSON.parse(ev.data);
          if (msg.id && pending.has(msg.id)) {
            const { resolve, reject } = pending.get(msg.id);
            pending.delete(msg.id);
            msg.error ? reject(new Error(msg.error.message)) : resolve(msg.result);
          }
        };
        await send('Network.enable');
        return page;
      }
    } catch { /* 还没起来 */ }
    await sleep(500);
  }
  throw new Error('无法连接 Chrome DevTools');
}

function cookieLine(cookies) {
  return cookies.filter((c) => /(^|\.)meoo\.com$/.test(c.domain)).map((c) => `${c.name}=${c.value}`).join('; ');
}

async function checkCookie(cookie) {
  const r = await fetch('https://meoo.com/api/mc/entitlement/init', {
    headers: {
      Cookie: cookie,
      Accept: 'application/json',
      Referer: 'https://meoo.com/',
      'User-Agent': 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154 Safari/537.36',
    },
  });
  const text = await r.text();
  let d; try { d = JSON.parse(text); } catch {}
  return { status: r.status, ok: Boolean(r.ok && d?.success), body: d };
}

async function readMeooCookie() {
  for (let i = 0; i < 20; i++) {
    try {
      const list = await (await fetch(`http://127.0.0.1:${CDP_PORT}/json/list`)).json();
      const page = list.find((x) => x.type === 'page');
      if (page) {
        const ws = new WebSocket(page.webSocketDebuggerUrl);
        await new Promise((res, rej) => { ws.onopen = res; ws.onerror = () => rej(new Error('ws')); });
        const result = await new Promise((res) => {
          ws.onmessage = (e) => { const m = JSON.parse(e.data); if (m.id === 1) res(m.result); };
          ws.send(JSON.stringify({ id: 1, method: 'Network.getAllCookies' }));
        });
        ws.close();
        const meoo = (result?.cookies || []).filter((c) => /(^|\.)meoo\.com$/.test(c.domain));
        if (meoo.length) return meoo.map((c) => `${c.name}=${c.value}`).join('; ');
      }
    } catch {}
    await new Promise((r) => setTimeout(r, 500));
  }
  return '';
}

async function oauthFlow() {
  const { createHash, randomBytes } = await import('node:crypto');
  const { createServer } = await import('node:http');
  const b64 = (b) => b.toString('base64url');
  const state = b64(randomBytes(32));
  const verifier = b64(randomBytes(32));
  const challenge = b64(createHash('sha256').update(verifier).digest());
  const redirect = `http://127.0.0.1:${OAUTH_PORT}/oauth/callback`;
  const u = new URL('https://meoo.com/oauth/authorize');
  const q = { client_id: 'meoo_app_tdWlRbpvAxN1EjmZ8XICxdTh', redirect_uri: redirect,
    response_type: 'code', scope: 'cli.compat', state, code_challenge: challenge, code_challenge_method: 'S256' };
  for (const [k, v] of Object.entries(q)) u.searchParams.set(k, v);

  const server = createServer(async (req, res) => {
    const url = new URL(req.url, `http://127.0.0.1:${OAUTH_PORT}`);
    if (url.pathname !== '/oauth/callback') { res.writeHead(404).end(); return; }
    const code = url.searchParams.get('code');
    res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' })
       .end('<h2>登录完成</h2>可以关闭这个页面，回到终端查看结果。');
    if (!code) { console.log(JSON.stringify({ ok: false, error: 'no code' })); process.exit(1); }
    const body = new URLSearchParams({ grant_type: 'authorization_code',
      client_id: 'meoo_app_tdWlRbpvAxN1EjmZ8XICxdTh', code, redirect_uri: redirect, code_verifier: verifier });
    const r = await fetch('https://meoo.com/oauth/token', { method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' }, body });
    const t = await r.json().catch(() => ({}));
    if (!r.ok || !t.refresh_token) {
      console.log(JSON.stringify({ ok: false, error: t.error || ('HTTP ' + r.status) }));
      process.exit(1);
    }
    const cookie = await readMeooCookie();
    if (IMPORT_TOKEN && API) {
      const ir = await fetch(`${API}/admin/api/oauth/import`, { method: 'POST',
        headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + IMPORT_TOKEN },
        body: JSON.stringify({ refreshToken: t.refresh_token, cookie }) });
      const j = await ir.json().catch(() => ({}));
      console.log(JSON.stringify({ ok: ir.ok, imported: j, token: { scope: t.scope, expires_in: t.expires_in } }));
    } else {
      console.log(JSON.stringify({ ok: true, refreshToken: t.refresh_token, accessToken: t.access_token }));
    }
    process.exit(0);
  });
  server.listen(OAUTH_PORT, '127.0.0.1', () => {
    log('打开浏览器授权：' + u.toString().slice(0, 120) + '…');
    try {
      spawn(CHROME, ['--user-data-dir=' + PROFILE, `--remote-debugging-port=${CDP_PORT}`,
        '--no-first-run', u.toString()], { stdio: 'ignore', detached: true });
    } catch (e) { log('请手动打开上面的地址'); }
  });
  setTimeout(() => { console.log(JSON.stringify({ ok: false, error: 'timeout' })); process.exit(2); }, TIMEOUT_MS);
}

(async () => {
  if (USE_OAUTH) { await oauthFlow(); return; }
  log(`浏览器已启动（profile: ${PROFILE}）`);
  log('请在浏览器窗口里用手机号登录 meoo.com（验证码由阿里云发送）');
  await connect();
  log('已连接 DevTools，等待登录完成…');

  const deadline = Date.now() + TIMEOUT_MS;
  let lastNames = '';
  while (Date.now() < deadline) {
    const { cookies } = await send('Network.getAllCookies');
    const meoo = cookies.filter((c) => /(^|\.)meoo\.com$/.test(c.domain));
    const names = meoo.map((c) => c.name).sort().join(',');
    if (names !== lastNames) {
      lastNames = names;
      if (names) log(`  当前 meoo cookie: ${names}`);
    }
    if (meoo.length) {
      const line = cookieLine(cookies);
      const v = await checkCookie(line);
      if (v.ok) {
        const d = v.body?.data || {};
        log(`\n登录成功，Cookie 可用（${line.length} 字节）`);
        log(`  initialized=${d.initialized} dailyLoginGranted=${d.dailyLoginGranted} amount=${d.dailyLoginGrantAmount ?? '-'} skip=${d.dailyLoginSkipReason || '-'}`);
        if (STDOUT_ONLY) {
          console.log(JSON.stringify({ ok: true, cookie: line, granted: d.dailyLoginGranted, amount: d.dailyLoginGrantAmount }));
        } else {
          await handle(line, d);
        }
        try { chrome.kill('SIGTERM'); } catch {}
        process.exit(0);
      }
      if (v.status === 401) { await sleep(1500); continue; }
    }
    await sleep(1000);
  }
  console.log(JSON.stringify({ ok: false, error: '等待超时，可能需要在浏览器里完成登录' }));
  console.error('等待超时，未拿到可用 Cookie');
  try { chrome.kill('SIGTERM'); } catch {}
  process.exit(2);
})();

async function handle(cookie, data) {
  if (IMPORT_TOKEN) {
    const r = await fetch(`${API}/admin/api/sso/import`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + IMPORT_TOKEN },
      body: JSON.stringify({ cookie }),
    });
    const j = await r.json().catch(() => ({}));
    log(r.ok ? `已导入账号：${j.name || j.id}（${j.key || '无 Key'}）` : `导入失败：${j.error || r.status}`);
    return;
  }
  if (!API) {
    log('\n未指定 --api，Cookie 如下（自行粘贴到控制台）:');
    log(cookie);
    return;
  }
  if (!ADMIN_PW) { log('\n未指定 --password，跳过自动写入'); return; }
  const t = await fetch(`${API}/admin/api/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username: 'admin', password: ADMIN_PW }),
  }).then((r) => r.json());
  if (!t.token) { log('管理端登录失败：' + (t.error || '')); return; }
  const auth = { 'Content-Type': 'application/json', Authorization: 'Bearer ' + t.token };
  const c = await fetch(`${API}/admin/api/accounts`, {
    method: 'POST', headers: auth, body: JSON.stringify({ cookie }),
  }).then((r) => r.json());
  const run = await fetch(`${API}/admin/api/accounts/${c.id}/checkin`, { method: 'POST', headers: auth }).then((r) => r.json());
  log(`已写入控制台：${run.ok ? (run.granted ? '签到成功 +' + run.amount : 'Cookie 有效，今日已领') : '签到失败 ' + (run.error || '')}`);
}
