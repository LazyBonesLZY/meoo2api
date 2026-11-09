const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');

function harness() {
  const elements = new Map(), storage = new Map(), intervals = [];
  class Element {
    constructor(){this.value='';this.checked=false;this.disabled=false;this.dataset={};this.style={};this.innerHTML='';this.textContent='';this.listeners={};this.classes=new Set();this.classList={add:x=>this.classes.add(x),remove:x=>this.classes.delete(x),toggle:(x,b)=>b?this.classes.add(x):this.classes.delete(x),contains:x=>this.classes.has(x)};}
    addEventListener(name,fn){this.listeners[name]=fn;}
    removeEventListener(name){delete this.listeners[name];}
    replaceChildren(){this.innerHTML='';this.textContent='';}
    removeAttribute(name){delete this[name];}
    querySelectorAll(){return [];}
    focus(){}
    click(){return this.onclick?.();}
  }
  const get=s=>{if(!elements.has(s))elements.set(s,new Element());return elements.get(s);};
  const context = vm.createContext({
    document:{querySelector:get,querySelectorAll:()=>[],documentElement:{dataset:{}},activeElement:null,hidden:false,createElement:()=>new Element()},
    sessionStorage:{getItem:k=>storage.get(k)||null,setItem:(k,v)=>storage.set(k,v),removeItem:k=>storage.delete(k)},
    localStorage:{getItem:()=>null,setItem:()=>{}},location:{origin:'http://console.test'},navigator:{},
    fetch:async()=>{throw new Error('unexpected network request');},
    window:{open:()=>{}},alert:()=>{},prompt:()=>{},confirm:()=>true,
    AbortController,AbortSignal,DOMException,URL,Blob,TextEncoder,console,
    setInterval:fn=>{intervals.push(fn);return intervals.length;},setTimeout,clearTimeout,
  });
  vm.runInContext(fs.readFileSync(path.join(__dirname,'console.js'),'utf8'),context);
  return {context,get,storage,run:code=>vm.runInContext(code,context)};
}
const result=(data,status=200)=>({status,ok:status>=200&&status<300,json:async()=>data});
const snapshot=(overrides={})=>({config:{defaultMaxTokens:800,timeoutMs:180000,sticky:false,cooldownSec:60,hardCooldownSec:900,autoCheckin:false,checkinHour:0,keepAliveHours:6},accounts:[],apiKeys:[],orphanKeys:[],alerts:[],usableKeys:0,today:'2026-01-01',...overrides});
function setToken(h,token='token'){h.context.testToken=token;h.run('generation++;rememberToken(testToken)');}
function installSnapshot(h,data){h.context.testSnapshot=data;h.run('SNAP=testSnapshot;renderAccounts();renderApiKeys();fillSettings()');}
const tick=()=>new Promise(resolve=>setImmediate(resolve));

test('HTML and attribute data are escaped, dynamic numeric fields coerced',()=>{
  const h=harness(),attack='\"\'><img src=x onerror=alert(1)>';
  assert.equal(h.run(`esc(${JSON.stringify(attack)})`),'&quot;&#39;&gt;&lt;img src=x onerror=alert(1)&gt;');
  installSnapshot(h,snapshot({accounts:[{id:attack,name:attack,userId:attack,enabled:true,hasCookie:true,points:attack,key:{key:attack,health:'ok',latencyMs:attack}}],apiKeys:[{id:attack,key:attack,enabled:true,stats:{req:attack}}],orphanKeys:[{id:attack,key:attack,health:attack}]}));
  for(const id of ['#accBody','#akBody']){
    assert.ok(!h.get(id).innerHTML.includes('<img'));
    assert.ok(!h.get(id).innerHTML.includes('data-id="\"'));
  }
  h.context.testRows=[{ts:0,method:attack,path:attack,model:attack,status:attack,ms:attack,keyLabel:attack,pt:attack}];
  assert.ok(!h.run('logRows(testRows)').includes('<img'));
});

test('device-only accounts disable Cookie actions and orphan keys stay manageable',()=>{
  const h=harness();
  installSnapshot(h,snapshot({usableKeys:2,accounts:[{id:'a',enabled:true,hasCookie:false,key:{enabled:true,key:'masked',health:'ok'}}],orphanKeys:[{id:'orphan',key:'masked',enabled:true,health:'ok'}]}));
  const html=h.get('#accBody').innerHTML;
  assert.match(html,/data-act="checkin" disabled/);
  assert.match(html,/data-act="ping" disabled/);
  assert.match(html,/未绑定 Cookie/);
  assert.match(html,/data-act="cookie">补充 Cookie/);
  for(const action of ['probe','toggle','del'])assert.ok(html.includes('data-orphan-act="'+action+'"'));
  assert.equal(h.get('#cKey').textContent,2);
});

test('refresh returns and coalesces a promise, settings edits survive refresh',async()=>{
  const h=harness();setToken(h);let resolveState,calls=0;
  h.context.fetch=async url=>{
    calls++;
    if(url.endsWith('/state'))return new Promise(resolve=>resolveState=()=>resolve(result(snapshot({usableKeys:3}))));
    return result(url.includes('/usage')?{hours:[]}:{rows:[]});
  };
  const first=h.run('refresh()'),second=h.run('refresh()');
  assert.equal(typeof first.then,'function');assert.equal(first,second);assert.equal(calls,1);
  h.get('#cfgMt').value='1234';h.get('#cfgMt').dataset.dirty='1';
  resolveState();await first;
  assert.equal(h.get('#cfgMt').value,'1234');assert.equal(h.get('#cKey').textContent,3);
});

test('old refresh completion cannot clear a new-generation refresh or expose old data',async()=>{
  const h=harness();setToken(h,'old');const pending=[];
  h.context.fetch=async url=>{
    if(url.endsWith('/state'))return new Promise(resolve=>pending.push(resolve));
    return result(url.includes('/usage')?{hours:[]}:{rows:[]});
  };
  const old=h.run('refresh()');setToken(h,'new');const current=h.run('refresh()');
  pending[0](result(snapshot({accounts:[{id:'old-secret',name:'OLD PRIVATE',enabled:true}]})));await old;
  assert.equal(h.run('refresh()'),current);
  assert.ok(!h.get('#accBody').innerHTML.includes('OLD PRIVATE'));
  pending[1](result(snapshot()));await current;
  assert.equal(h.run('refreshPromise'),null);
});

test('401 resets session and clears displayed secrets, malformed responses surface errors',async()=>{
  const h=harness();setToken(h);h.get('#accBody').innerHTML='PRIVATE';h.get('#newPass').value='SECRET';
  h.context.fetch=async()=>result({error:'expired'},401);
  await assert.rejects(h.run("admin('/admin/api/state')"),/需要登录/);
  assert.equal(h.run('TOKEN'),'');assert.equal(h.storage.size,0);
  assert.equal(h.get('#accBody').innerHTML,'');assert.equal(h.get('#newPass').value,'');assert.ok(h.get('#login').classList.contains('on'));
  setToken(h);h.context.fetch=async()=>({status:502,ok:false,json:async()=>{throw new Error('bad json');}});
  await assert.rejects(h.run("admin('/admin/api/state')"),/无效响应/);
});

test('closing device modal aborts pending work and sends server cancellation',async()=>{
  const h=harness();setToken(h);let polls=0,cancels=0,started=false;
  h.context.fetch=async(url,opts)=>{
    if(url.endsWith('/device/start')){started=true;return new Promise((resolve,reject)=>opts.signal.addEventListener('abort',()=>reject(new DOMException('Aborted','AbortError')),{once:true}));}
    if(url.endsWith('/device/cancel')){cancels++;return result({ok:true});}
    if(url.includes('/device/poll')){polls++;return result({status:'pending'});}
    throw new Error('unexpected URL');
  };
  const run=h.get('#devStart').click();await tick();assert.ok(started);
  h.get('#addCancel').click();await run;await tick();
  assert.equal(polls,0);assert.equal(cancels,1);assert.equal(h.run('deviceAbort'),null);
  assert.equal(h.get('#addOk').disabled,false);assert.equal(h.get('#devStart').disabled,false);
});

test('integer settings validate blank, fractions and limits; keepAliveHours zero retained',()=>{
  const h=harness();
  for(const text of ['', '1.2','-1','169']){h.get('#ciKeep').value=text;assert.throws(()=>h.run("integer('ciKeep',0,168)"));}
  h.get('#ciKeep').value='0';assert.equal(h.run("integer('ciKeep',0,168)"),0);
  installSnapshot(h,snapshot({config:{...snapshot().config,keepAliveHours:0}}));assert.equal(h.get('#ciKeep').value,0);
});

test('zero default output budget is displayed and saved without injection',async()=>{
  const h=harness();setToken(h);let body;
  const state=snapshot({config:{...snapshot().config,defaultMaxTokens:0}});
  installSnapshot(h,state);assert.equal(h.get('#cfgMt').value,0);
  // Real input elements stringify assigned values; mirror that DOM behavior.
  for(const id of ['#cfgMt','#cfgTimeout','#cfgCool','#cfgHard'])h.get(id).value=String(h.get(id).value);
  h.context.fetch=async(url,opts)=>{
    if(opts.method==='PATCH'){body=JSON.parse(opts.body);return result({ok:true});}
    if(url.endsWith('/state'))return result(state);
    return result(url.includes('/usage')?{hours:[]}:{rows:[]});
  };
  await h.get('#saveCfg').click();assert.equal(body.defaultMaxTokens,0);
  assert.equal(h.get('#cfgStat').textContent,'已保存');
});

test('Cookie editor patches only the selected account and clears secret after saving',async()=>{
  const h=harness();setToken(h);const calls=[];
  installSnapshot(h,snapshot({accounts:[{id:'a/1',name:'Alice',userId:'user-1',hasCookie:false,enabled:true}]}));
  h.run('openCookie(SNAP.accounts[0])');
  assert.equal(h.get('#cookieTitle').textContent,'补充 Cookie');
  assert.equal(h.get('#cookieAccount').textContent,'Alice · user-1');
  assert.ok(h.get('#cookieModal').classList.contains('on'));
  h.get('#cookieInput').value='  session=example-cookie  ';
  h.context.fetch=async(url,opts)=>{
    calls.push([url,opts]);
    if(opts.method==='PATCH')return result({ok:true});
    if(url.endsWith('/state'))return result(snapshot({accounts:[{id:'a/1',hasCookie:true,enabled:true}]}));
    return result(url.includes('/usage')?{hours:[]}:{rows:[]});
  };
  await h.get('#cookieSave').click();
  assert.equal(calls[0][0],'http://console.test/admin/api/accounts/a%2F1');
  assert.equal(calls[0][1].method,'PATCH');
  assert.deepEqual(JSON.parse(calls[0][1].body),{cookie:'session=example-cookie'});
  assert.equal(calls.filter(x=>x[1].method==='POST').length,0);
  assert.equal(h.get('#cookieInput').value,'');
  assert.ok(!h.get('#cookieModal').classList.contains('on'));
  assert.match(h.get('#accBody').innerHTML,/data-act="cookie">更新 Cookie/);
});

test('Cookie validation errors stay in editor without replacing displayed account',async()=>{
  const h=harness();setToken(h);
  installSnapshot(h,snapshot({accounts:[{id:'a',name:'Alice',hasCookie:true,enabled:true}]}));
  h.run('openCookie(SNAP.accounts[0])');assert.equal(h.get('#cookieTitle').textContent,'更新 Cookie');
  let calls=0;
  h.context.fetch=async()=>{calls++;return result({error:'Cookie 属于其他账号'},400);};
  h.get('#cookieInput').value='';await h.get('#cookieSave').click();assert.equal(calls,0);
  h.get('#cookieInput').value='session=bad\nvalue=x';await h.get('#cookieSave').click();assert.equal(calls,0);
  h.get('#cookieInput').value='session=wrong-user';await h.get('#cookieSave').click();
  assert.equal(calls,1);assert.equal(h.get('#cookieMsg').textContent,'Cookie 属于其他账号');
  assert.ok(h.get('#cookieModal').classList.contains('on'));
  h.get('#cookieCancel').click();assert.equal(h.get('#cookieInput').value,'');
  assert.ok(!h.get('#cookieModal').classList.contains('on'));
});

test('closing Cookie editor cancels pending request and late response cannot reopen it',async()=>{
  const h=harness();setToken(h);let resolveRequest,signal;
  h.run("openCookie({id:'a',name:'Alice'})");h.get('#cookieInput').value='session=example';
  h.context.fetch=async(url,opts)=>{signal=opts.signal;return new Promise(resolve=>resolveRequest=resolve);};
  const pending=h.get('#cookieSave').click();
  h.get('#cookieCancel').click();assert.equal(signal.aborted,true);assert.equal(h.get('#cookieInput').value,'');
  h.run("openCookie({id:'b',name:'Bob'})");h.get('#cookieInput').value='session=bob';
  resolveRequest(result({ok:true}));await pending;
  assert.equal(h.get('#cookieInput').value,'session=bob');assert.equal(h.run('cookieAccountID'),'b');
  h.run('showLogin()');assert.equal(h.get('#cookieInput').value,'');
  assert.ok(!h.get('#cookieModal').classList.contains('on'));
});

test('clipboard denial uses manual copy and logout revokes server before clearing',async()=>{
  const h=harness();let copied='',requests=[];
  h.context.prompt=(label,key)=>copied=key;
  await h.run("copyKey('test-key')");assert.equal(copied,'test-key');
  setToken(h);h.context.fetch=async(url,opts)=>{requests.push([url,opts.method]);return result({ok:true});};
  await h.get('#logout').click();assert.equal(requests.length,1);assert.match(requests[0][0],/\/logout$/);assert.equal(requests[0][1],'POST');assert.equal(h.run('TOKEN'),'');
});
