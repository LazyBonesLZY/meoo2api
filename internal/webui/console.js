'use strict';
const $ = s => document.querySelector(s), $$ = s => [...document.querySelectorAll(s)];
const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const num = v => Number.isFinite(Number(v)) ? Number(v) : 0;
const enc = encodeURIComponent;
let TOKEN = '', SNAP = null, generation = 0, deviceRun = 0, deviceAbort = null, refreshPromise = null, logRun = 0, deviceCancel = Promise.resolve();
let cookieAccountID = '', cookieRun = 0, cookieAbort = null;
try { TOKEN = sessionStorage.getItem('m2a_admin') || ''; } catch {}
function rememberToken(token) { TOKEN = token; try { token ? sessionStorage.setItem('m2a_admin', token) : sessionStorage.removeItem('m2a_admin'); } catch {} }
function clearPrivate() {
  SNAP = null;
  for (const id of ['accBody','akBody','logBody','recentBody','alerts']) $('#'+id).replaceChildren();
  for (const id of ['addCookie','lgPass','newPass']) $('#'+id).value = '';
  for (const id of ['cAcc','cKey','cCi','cReq']) $('#'+id).textContent = '0';
}
function showLogin(message = '') {
  generation++; rememberToken(''); refreshPromise=null; stopDevice(false); closeCookie(); clearPrivate();
  $('#login').classList.add('on'); $('#lgMsg').textContent = message;
  $('#addModal').classList.remove('on');
}
async function admin(path, opts = {}) {
  const token = TOKEN, epoch = generation;
  const controller = new AbortController(), timer = setTimeout(() => controller.abort(), 330000);
  const abort = () => controller.abort();
  if (opts.signal?.aborted) abort(); else opts.signal?.addEventListener('abort', abort, {once:true});
  try {
    const r = await fetch(location.origin+path, {...opts, signal:controller.signal, mode:'same-origin', credentials:'omit', cache:'no-store',
      headers:{'Content-Type':'application/json', Authorization:'Bearer '+token}});
    if (epoch !== generation || token !== TOKEN) throw new Error('会话已切换');
    if (r.status === 401) { showLogin('登录已过期，请重新登录'); throw new Error('需要登录'); }
    const d = await r.json().catch(() => { throw new Error('服务返回了无效响应'); });
    if (!r.ok) { const error=new Error(typeof d.error === 'string' ? d.error : ('HTTP '+r.status));error.status=r.status;throw error; }
    return d;
  } finally { clearTimeout(timer); opts.signal?.removeEventListener('abort', abort); }
}
async function busy(button, action) {
  if (button.disabled) return;
  button.disabled = true;
  try { await action(); } finally { if(button.id==='devStart')await deviceCancel; button.disabled = false; }
}
async function copyKey(key) {
  if (!navigator.clipboard?.writeText) { prompt('请手动复制 API Key：', key); return; }
  try { await navigator.clipboard.writeText(key); alert('已复制 API Key'); }
  catch { prompt('剪贴板访问失败，请手动复制：', key); }
}
function date(value, timeOnly = false) {
  const d = new Date(value);
  return Number.isFinite(d.getTime()) ? (timeOnly ? d.toLocaleTimeString('zh-CN',{hour12:false}) : d.toLocaleString('zh-CN',{hour12:false})) : '—';
}
$('#lgBtn').onclick = () => busy($('#lgBtn'), async () => {
  $('#lgMsg').textContent = '';
  try {
    const r = await fetch(location.origin+'/admin/api/login', {method:'POST', mode:'same-origin', credentials:'omit', cache:'no-store',
      signal:AbortSignal.timeout(30000), headers:{'Content-Type':'application/json'},
      body:JSON.stringify({username:$('#lgUser').value.trim(), password:$('#lgPass').value})});
    const d = await r.json();
    if (!r.ok || typeof d.token !== 'string') throw new Error(d.error || '登录失败');
    generation++; rememberToken(d.token); $('#lgPass').value = '';
    $('#login').classList.remove('on'); await refresh();
  } catch (e) { $('#lgMsg').textContent = e.message; }
});
$('#lgPass').addEventListener('keydown', e => { if (e.key === 'Enter') $('#lgBtn').click(); });
$('#logout').onclick = () => busy($('#logout'), async () => {
  try { await admin('/admin/api/logout', {method:'POST'}); }
  catch (e) { alert('退出未确认：'+e.message); return; }
  showLogin();
});
$$('.nav button[data-view]').forEach(b => b.onclick = () => {
  $$('.nav button[data-view]').forEach(x => x.classList.toggle('on', x === b));
  $$('.view').forEach(v => v.classList.toggle('on', v.id === 'v-'+b.dataset.view));
  if (b.dataset.view === 'logs') void loadLogs();
});
try { document.documentElement.dataset.theme = localStorage.getItem('m2a_theme') === 'light' ? 'light' : 'dark'; } catch {}
$('#theme').onclick = () => {
  const t = document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark';
  document.documentElement.dataset.theme = t; try { localStorage.setItem('m2a_theme', t); } catch {}
};

function refresh() {
  if (!TOKEN) return Promise.resolve();
  if (refreshPromise?.epoch === generation) return refreshPromise.promise;
  const epoch = generation;
  const current = {epoch};
  current.promise = (async () => {
    try {
      const snapshot = await admin('/admin/api/state');
      if (epoch !== generation) return;
      SNAP = snapshot; renderAccounts(); renderApiKeys(); fillSettings();
      const usable = num(SNAP.usableKeys);
      $('#pHealth').className = 'pill '+(usable ? 'ok' : 'bad');
      $('#pHealth').textContent = '服务正常'; $('#pKeys').textContent = '可用 Key '+usable;
      $('#fUp').textContent = '运行 '+Math.floor(num(SNAP.uptime)/3600)+' 小时';
      $('#fAcc').textContent = '账号 '+(SNAP.accounts||[]).length;
      $('#alerts').innerHTML = (SNAP.alerts||[]).map(a=>`<div class="alert ${a.level==='bad'?'bad':'warn'}">● ${esc(a.text)}</div>`).join('');
      await loadUsage(epoch);
    } catch (e) {
      if (epoch === generation) { $('#pHealth').className='pill bad'; $('#pHealth').textContent=e.message; }
    }
  })().finally(() => { if(refreshPromise===current)refreshPromise=null; });
  refreshPromise=current;
  return current.promise;
}
function renderApiKeys() {
  const list = SNAP?.apiKeys || [];
  $('#akBody').innerHTML = list.map(k=>`<tr data-id="${esc(k.id)}"><td class="mono">${esc(k.key)}</td>
    <td><span class="badge ${k.enabled?'ok':''}">${k.enabled?'启用':'停用'}</span></td><td class="mono">${num(k.stats?.req)}</td><td class="mono">${date(k.createdAt)}</td>
    <td><button class="btn sm" data-ak="copy">复制</button><button class="btn sm" data-ak="toggle">${k.enabled?'停用':'启用'}</button><button class="btn sm danger" data-ak="del">删除</button></td></tr>`).join('') || '<tr><td colspan="5" class="muted">还没有 API Key</td></tr>';
  $('#akBody').querySelectorAll('button[data-ak]').forEach(b=>b.onclick=()=>busy(b, async()=>{
    const id=b.closest('tr').dataset.id, act=b.dataset.ak, k=list.find(x=>x.id===id), path='/admin/api/api-keys/'+enc(id);
    try {
      if(act==='copy'){ await copyKey((await admin(path+'/reveal')).key); return; }
      if(act==='toggle') await admin(path,{method:'PATCH',body:JSON.stringify({enabled:!k.enabled})});
      if(act==='del'){ if(!confirm('删除这个 API Key？'))return; await admin(path,{method:'DELETE'}); }
      await refresh();
    } catch(e){ alert(e.message); }
  }));
  $('#akStat').textContent=`${list.length} 个 · 启用 ${list.filter(k=>k.enabled).length}`;
}
$('#newApiKey').onclick=()=>busy($('#newApiKey'),async()=>{
  try { const r=await admin('/admin/api/api-keys',{method:'POST',body:'{}'}); await copyKey(r.key); await refresh(); } catch(e){alert(e.message);}
});
function keyBadge(a) {
  if(!a.enabled) return '<span class="badge">账号停用</span>';
  if(!a.key) return '<span class="badge warn">无 Key</span>';
  if(a.key.cooldown>0) return `<span class="badge warn">冷却 ${num(a.key.cooldown)}s</span>`;
  if(a.key.health==='ok')return `<span class="badge ok">正常 ${num(a.key.latencyMs)}ms</span>`;
  if(a.key.health==='invalid')return '<span class="badge bad">鉴权失败</span>';
  return '<span class="badge">'+(a.key.health==='error'?'异常':'未探测')+'</span>';
}
function fillUnfocused(id,value,checked=false) {
  const el=$('#'+id); if(document.activeElement===el || el.dataset.dirty==='1')return;
  if(checked)el.checked=!!value; else el.value=value;
}
function renderAccounts() {
  if(!SNAP)return;
  const list=SNAP.accounts||[], today=SNAP.today;
  $('#accBody').innerHTML=list.map(a=>`<tr data-id="${esc(a.id)}">
    <td><div>${esc(a.name||'未识别')}</div><div class="muted mono">${esc(a.userId)}</div></td><td class="mono">${a.key?esc(a.key.key):'—'}</td>
    <td>${keyBadge(a)}<div class="muted">${!a.hasCookie?'设备授权（未绑定 Cookie）':a.expired?'Cookie 会话失效':a.lastPing?(a.pingOk?'Cookie 保活 '+date(a.lastPing,true):'保活失败'):'Cookie 未保活'}</div></td>
    <td class="mono">${num(a.points)}<div class="muted">${a.pointsAt?date(a.pointsAt,true):'—'}</div></td>
    <td><span class="badge ${a.lastGrantDate===today?'ok':'warn'}">${!a.hasCookie?'未绑定 Cookie':a.lastGrantDate===today?'今日已签':'待签'}</span><div class="muted">累计 ${num(a.totalGranted)}</div></td>
    <td><button class="btn sm" data-act="copy" ${a.key?'':'disabled'}>复制 Key</button><button class="btn sm" data-act="probe" ${a.key?'':'disabled'}>探活</button>
    <button class="btn sm" data-act="checkin" ${a.enabled&&a.hasCookie?'':'disabled'}>签到</button><button class="btn sm" data-act="ping" ${a.hasCookie?'':'disabled'}>保活</button>
    <button class="btn sm" data-act="reset-cooldown" ${a.key?'':'disabled'}>解除冷却</button><button class="btn sm" data-act="points">刷新积分</button>
    <button class="btn sm" data-act="cookie">${a.hasCookie?'更新 Cookie':'补充 Cookie'}</button>
    <button class="btn sm" data-act="onboard">重试接入</button><button class="btn sm" data-act="toggle">${a.enabled?'停用':'启用'}</button>
    <button class="btn sm danger" data-act="del">删除</button></td></tr>`).join('')+(SNAP.orphanKeys||[]).map(k=>`<tr data-orphan="${esc(k.id)}"><td class="muted">历史项目 Key（未绑定账号）</td><td class="mono">${esc(k.key)}</td><td>${keyBadge({enabled:k.enabled,key:k})}</td><td>—</td><td>—</td><td><button class="btn sm" data-orphan-act="probe">探活</button><button class="btn sm" data-orphan-act="toggle">${k.enabled?'停用':'启用'}</button><button class="btn sm danger" data-orphan-act="del">删除</button></td></tr>`).join('') || '<tr><td colspan="6" class="muted">还没有账号，点「+ 添加账号」</td></tr>';
  $('#accBody').querySelectorAll('button[data-orphan-act]').forEach(b=>b.onclick=()=>busy(b,async()=>{
    const id=b.closest('tr').dataset.orphan,act=b.dataset.orphanAct,k=SNAP.orphanKeys.find(x=>x.id===id),path='/admin/api/orphan-keys/'+enc(id);
    try {
      if(act==='del'){if(!confirm('删除这个未绑定的 Key？'))return;await admin(path,{method:'DELETE'});}
      if(act==='toggle')await admin(path,{method:'PATCH',body:JSON.stringify({enabled:!k.enabled})});
      if(act==='probe'){const r=await admin(path+'/probe',{method:'POST'});alert(r.state==='ok'?`正常 ${num(r.latencyMs)}ms`:(r.error||r.state));}
      await refresh();
    } catch(e){alert(e.message);}
  }));
  $('#accBody').querySelectorAll('button[data-act]').forEach(b=>b.onclick=()=>busy(b,async()=>{
    const id=b.closest('tr').dataset.id, act=b.dataset.act, a=list.find(x=>x.id===id), path='/admin/api/accounts/'+enc(id);
    // Disable the whole row during operations; a refresh can replace the row,
    // but the server's lifecycle gate remains the final concurrency boundary.
    const buttons=[...b.closest('tr').querySelectorAll('button')], disabled=buttons.map(x=>x.disabled);
    buttons.forEach(x=>x.disabled=true);
    try {
      if(act==='cookie'){openCookie(a);return;}
      if(act==='copy'){await copyKey((await admin(path+'/key')).key);return;}
      if(act==='del'){if(!confirm('删除账号及全部关联 API Key？'))return;await admin(path,{method:'DELETE'});}
      else if(act==='toggle')await admin(path,{method:'PATCH',body:JSON.stringify({enabled:!a.enabled})});
      else {
        const r=await admin(path+'/'+act,{method:'POST'});
        if(act==='probe')alert(r.state==='ok'?`正常 ${num(r.latencyMs)}ms`:'异常：'+(r.error||r.state));
        if(act==='checkin')alert(r.ok?(r.granted?`签到成功 +${num(r.amount)}`:'今日已领'):('签到失败：'+(r.error||r.skipReason||'')));
        if(act==='points')alert('积分：'+num(r.points));
        if(act==='ping')alert(r.ok?'Cookie 会话有效':(r.error||'保活失败'));
        if(act==='onboard')alert(r.error||'接入完成');
      }
      await refresh();
    }catch(e){alert(e.message);}
    finally{buttons.forEach((x,i)=>x.disabled=disabled[i]);}
  }));
  $('#accStat').textContent=`${list.length} 个账号 · ${list.filter(a=>a.enabled).length} 个启用 · ${list.filter(a=>a.key).length} 个 API Key`;
  fillUnfocused('autoCi',SNAP.config.autoCheckin,true);fillUnfocused('ciHour',SNAP.config.checkinHour??0);fillUnfocused('ciKeep',SNAP.config.keepAliveHours??6);
  $('#cAcc').textContent=list.length;$('#cKey').textContent=num(SNAP.usableKeys);
  $('#cCi').textContent=list.filter(a=>a.lastGrantDate===today).length;
}

function closeCookie() {
  cookieRun++; cookieAbort?.abort(); cookieAbort=null; cookieAccountID='';
  $('#cookieModal').classList.remove('on'); $('#cookieInput').value='';
  $('#cookieAccount').textContent=''; $('#cookieMsg').textContent='';
}
function openCookie(account) {
  closeCookie(); cookieAccountID=account.id;
  $('#cookieTitle').textContent=account.hasCookie?'更新 Cookie':'补充 Cookie';
  $('#cookieAccount').textContent=(account.name||'未识别账号')+(account.userId?' · '+account.userId:'');
  $('#cookieModal').classList.add('on'); $('#cookieInput').focus();
}
$('#cookieCancel').onclick=closeCookie;
$('#cookieModal').addEventListener('keydown',e=>{if(e.key==='Escape')closeCookie();});
$('#cookieSave').onclick=()=>busy($('#cookieSave'),async()=>{
  const id=cookieAccountID, cookie=$('#cookieInput').value.trim(), run=cookieRun;
  if(!id)return;
  if(!cookie||cookie.length>16384||/[\r\n\0]/.test(cookie)){
    $('#cookieMsg').textContent='请粘贴完整 Cookie（单行，最多 16384 字符）';return;
  }
  const controller=new AbortController();cookieAbort=controller;
  $('#cookieMsg').textContent='正在验证账号并保存…';
  try{
    await admin('/admin/api/accounts/'+enc(id),{method:'PATCH',body:JSON.stringify({cookie}),signal:controller.signal});
    if(run!==cookieRun)return;
    closeCookie();await refresh();
  }catch(e){if(run===cookieRun)$('#cookieMsg').textContent=e.name==='AbortError'?'已取消':e.message;}
  finally{if(run===cookieRun)cookieAbort=null;}
});

function stopDevice(cancelRemote=true) {
  deviceRun++;deviceAbort?.abort();deviceAbort=null;
  const button=$('#devStart');button.disabled=!!(cancelRemote&&TOKEN);
  if(cancelRemote&&TOKEN) deviceCancel=admin('/admin/api/device/cancel',{method:'POST'}).catch(()=>{}).finally(()=>{button.disabled=false;});
  $('#addOk').disabled=false;
}
function closeAdd(){stopDevice();$('#addModal').classList.remove('on');$('#addCookie').value='';$('#devUrl').textContent='';$('#devLink').removeAttribute('href');$('#devBox').style.display='none';}
$('#openAdd').onclick=()=>{$('#addModal').classList.add('on');$('#addMsg').textContent='';};
$('#addCancel').onclick=closeAdd;
function wait(ms,signal){return new Promise((resolve,reject)=>{
  const abort=()=>{clearTimeout(timer);reject(new DOMException('Aborted','AbortError'));};
  const timer=setTimeout(()=>{signal.removeEventListener('abort',abort);resolve();},ms);
  if(signal.aborted)abort();else signal.addEventListener('abort',abort,{once:true});
});}
$('#devStart').onclick=()=>busy($('#devStart'),async()=>{
  const run=++deviceRun;
  await deviceCancel;
  if(run!==deviceRun)return;
  const controller=new AbortController();deviceAbort=controller;
  $('#devMsg').textContent='创建授权会话…';$('#addOk').disabled=true;
  try{
    const r=await admin('/admin/api/device/start',{method:'POST',body:'{}',signal:controller.signal});
    const target=new URL(r.url);
    if(target.protocol!=='https:'||target.hostname!=='meoo.com'||target.port||target.pathname!=='/cli/login'||target.username||target.password)throw new Error('授权链接无效');
    if(run!==deviceRun)return;
    $('#devUrl').textContent=target.href;$('#devLink').href=target.href;$('#devBox').style.display='block';window.open(target.href,'_blank','noopener,noreferrer');$('#devMsg').textContent='等待授权…';
    for(let i=0;i<180;i++){
      await wait(3000,controller.signal);
      let st;
      try{st=await admin('/admin/api/device/poll?sid='+enc(r.sessionId),{signal:controller.signal});}
      catch(e){if(e.status===502){$('#devMsg').textContent=e.message+'（自动重试中）';continue;}throw e;}
      if(run!==deviceRun)return;
      if(st.status==='approved'){
        $('#devMsg').textContent='已添加：'+(st.name||st.userId||'账号')+(st.error?' · '+st.error:'');
        await refresh();return;
      }
      if(st.status!=='pending'){$('#devMsg').textContent='授权已结束：'+st.status;return;}
    }
    $('#devMsg').textContent='等待超时，请重新开始';
  }catch(e){if(run===deviceRun)$('#devMsg').textContent=e.name==='AbortError'?'已取消':e.message;}
  finally{if(run===deviceRun){deviceAbort=null;$('#addOk').disabled=false;}else if(!deviceAbort)$('#addOk').disabled=false;}
});
$('#devLoginFirst').onclick=()=>window.open('https://meoo.com/','_blank','noopener,noreferrer');
$('#addOk').onclick=()=>busy($('#addOk'),async()=>{
  const cookie=$('#addCookie').value.trim();if(!cookie){$('#addMsg').textContent='先粘贴 Cookie';return;}
  $('#devStart').disabled=true;$('#addMsg').textContent='接入中…';
  try{const r=await admin('/admin/api/accounts',{method:'POST',body:JSON.stringify({cookie})});$('#addMsg').textContent='已添加：'+(r.name||r.id);$('#addCookie').value='';await refresh();}
  catch(e){$('#addMsg').textContent=e.message;}finally{$('#devStart').disabled=false;}
});
$('#btnCheckinAll').onclick=()=>busy($('#btnCheckinAll'),async()=>{
  $('#opStat').textContent='签到中…';
  try{const r=await admin('/admin/api/accounts-checkin-all',{method:'POST'});$('#opStat').textContent=`完成：${r.results.filter(x=>x.granted).length} 个到账`;await refresh();}
  catch(e){$('#opStat').textContent=e.message;}
});
$('#btnProbeAll').onclick=()=>busy($('#btnProbeAll'),async()=>{
  $('#opStat').textContent='探活中…';
  try{const st=await admin('/admin/api/state'),list=(st.accounts||[]).filter(a=>a.enabled&&a.key);let ok=0;
    for(const a of list){if((await admin('/admin/api/accounts/'+enc(a.id)+'/probe',{method:'POST'})).state==='ok')ok++;}
    $('#opStat').textContent=`探活完成：${ok}/${list.length} 可用`;await refresh();
  }catch(e){$('#opStat').textContent=e.message;}
});
function logRows(rows,recent=false){return rows.map(r=>`<tr><td class="mono">${date(r.ts,recent)}</td>${recent?'':`<td>${esc(r.method)}</td><td class="mono">${esc(r.path)}</td>`}<td>${esc(r.model||'—')}</td>
  <td><span class="badge ${r.status<400?'ok':'bad'}">${num(r.status)}</span></td><td class="mono">${num(r.ms)}ms</td>${recent?'':`<td class="mono">${num(r.pt)}/${num(r.ct)}</td>`}<td class="muted">${esc(r.keyLabel||'—')}</td></tr>`).join('')||`<tr><td colspan="${recent?5:8}" class="muted">暂无记录</td></tr>`;}
async function loadUsage(epoch){
  try{const d=await admin('/admin/api/usage?hours=24');if(epoch===generation)$('#cReq').textContent=d.hours.reduce((a,h)=>a+num(h.req),0);}catch(e){if(epoch===generation)$('#opStat').textContent=e.message;}
  try{const d=await admin('/admin/api/logs?limit=8');if(epoch===generation)$('#recentBody').innerHTML=logRows(d.rows,true);}catch(e){if(epoch===generation)$('#opStat').textContent=e.message;}
}
async function loadLogs(){
  if(!TOKEN)return;const run=++logRun,epoch=generation;
  try{const d=await admin('/admin/api/logs?limit=200&status='+enc($('#logStatus').value)+'&q='+enc($('#logQ').value.trim()));
    if(run!==logRun||epoch!==generation)return;$('#logCount').textContent=d.total+' 条';$('#logBody').innerHTML=logRows(d.rows);
  }catch(e){if(run===logRun&&epoch===generation)$('#logCount').textContent=e.message;}
}
$('#reloadLogs').onclick=loadLogs;
const settings=['cfgMt','cfgTimeout','cfgSticky','cfgCool','cfgHard'];
const schedules=['autoCi','ciHour','ciKeep'];
[...settings,...schedules].forEach(id=>$('#'+id).addEventListener('input',()=>{$('#'+id).dataset.dirty='1';}));
function fillSettings(){if(!SNAP)return;const c=SNAP.config;
  fillUnfocused('cfgMt',c.defaultMaxTokens);fillUnfocused('cfgTimeout',c.timeoutMs);fillUnfocused('cfgSticky',c.sticky,true);fillUnfocused('cfgCool',c.cooldownSec);fillUnfocused('cfgHard',c.hardCooldownSec);
}
function integer(id,min,max){const text=$('#'+id).value.trim(),n=Number(text);if(!text||!Number.isInteger(n)||n<min||n>max)throw new Error(`配置值需为 ${min}–${max} 之间的整数`);return n;}
function clean(ids){ids.forEach(id=>delete $('#'+id).dataset.dirty);}
$('#saveCfg').onclick=()=>busy($('#saveCfg'),async()=>{
  try{await admin('/admin/api/config',{method:'PATCH',body:JSON.stringify({defaultMaxTokens:integer('cfgMt',0,262144),timeoutMs:integer('cfgTimeout',1000,900000),sticky:$('#cfgSticky').checked,cooldownSec:integer('cfgCool',0,86400),hardCooldownSec:integer('cfgHard',0,604800)})});clean(settings);$('#cfgStat').textContent='已保存';await refresh();}
  catch(e){$('#cfgStat').textContent=e.message;}
});
$('#savePass').onclick=()=>busy($('#savePass'),async()=>{
  const value=$('#newPass').value;if(new TextEncoder().encode(value).length<12){alert('管理密码至少 12 字节');return;}
  try{await admin('/admin/api/config',{method:'PATCH',body:JSON.stringify({adminPassword:value})});showLogin('密码已更新，请重新登录');}catch(e){alert(e.message);}
});
$('#exportState').onclick=()=>busy($('#exportState'),async()=>{
  try{const d=await admin('/admin/api/export'),blob=new Blob([JSON.stringify(d,null,2)],{type:'application/json'}),url=URL.createObjectURL(blob),a=document.createElement('a');
    a.href=url;a.download='meoo2api-summary.json';a.click();setTimeout(()=>URL.revokeObjectURL(url),1000);
  }catch(e){alert(e.message);}
});
let scheduleSave=Promise.resolve();
function saveCiCfg(){
  scheduleSave=scheduleSave.catch(()=>{}).then(async()=>{
    try{await admin('/admin/api/config',{method:'PATCH',body:JSON.stringify({autoCheckin:$('#autoCi').checked,checkinHour:integer('ciHour',0,23),keepAliveHours:integer('ciKeep',0,168)})});clean(schedules);await refresh();$('#accStat').textContent+=' · 设置已保存';}
    catch(e){$('#accStat').textContent=e.message;}
  });
  return scheduleSave;
}
schedules.forEach(id=>$('#'+id).onchange=saveCiCfg);
if(TOKEN)void refresh();else showLogin();
setInterval(()=>{if(TOKEN&&!document.hidden)void refresh();},30000);
