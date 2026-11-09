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
const API = (arg('api', '') || '').replace(/\/+$/, '');
const ADMIN_PW = arg('password', process.env.MEOO2API_ADMIN_PASSWORD || '');
const USE_SSO = !process.argv.includes('--no-sso');
const TIMEOUT_MS = Number(arg('timeout', 300000));

const CHROME = ['google-chrome', 'google-chrome-stable', 'chromium', 'chromium-browser']
  .find((b) => {
    try { return Boolean(spawn('which', [b]).stdout); } catch { return false; }
  }) || 'google-chrome';

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

(async () => {
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
        await handle(line, d);
        if (!flag('keep')) { try { chrome.kill('SIGTERM'); } catch {} }
        process.exit(0);
      }
      if (v.status === 401) { await sleep(1500); continue; }
    }
    await sleep(1000);
  }
  console.error('等待超时，未拿到可用 Cookie');
  try { chrome.kill('SIGTERM'); } catch {}
  process.exit(2);
})();

async function handle(cookie, data) {
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
  const list = await fetch(`${API}/admin/api/checkins`, { headers: auth }).then((r) => r.json());
  const exist = (list.checkins || []).find((x) => x.label === LABEL);
  const c = exist
    ? await fetch(`${API}/admin/api/checkins/${exist.id}`, { method: 'PATCH', headers: auth, body: JSON.stringify({ cookie }) })
        .then((r) => r.json()).then(() => ({ id: exist.id }))
    : await fetch(`${API}/admin/api/checkins`, {
        method: 'POST', headers: auth, body: JSON.stringify({ label: LABEL, cookie }),
      }).then((r) => r.json());
  if (!c.id) { log('写入签到账号失败：' + (c.error || '')); return; }
  const run = await fetch(`${API}/admin/api/checkins/${c.id}/run`, { method: 'POST', headers: auth }).then((r) => r.json());
  log(`已写入控制台：${run.ok ? (run.granted ? '签到成功 +' + run.amount : 'Cookie 有效，今日已领') : '签到失败 ' + (run.error || '')}`);
}
