const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { test } = require('node:test');

const source = fs.readFileSync(process.env.PASSBRIDGE_AUTH_SOURCE || path.join(__dirname, '../internal/bridge/ui/cpa-auth.js'), 'utf8');
const origin = 'https://cpa.example';
const userAgent = 'PassBridge auth regression fixture';
const key = 'fixture-management-key';

function savedLogin(apiBase = origin, rememberPassword = true) {
  return { state: { apiBase, managementKey: key, rememberPassword }, version: 0 };
}

function encrypted(login, version = 'v1') {
  const bytes = new TextEncoder().encode(JSON.stringify(login));
  const mask = new TextEncoder().encode(version === 'v2'
    ? 'cli-proxy-api-webui::secure-storage|v2|cpa.example'
    : `cli-proxy-api-webui::secure-storage|cpa.example|${userAgent}`);
  return `enc::${version}::` + Buffer.from(bytes.map((byte, i) => byte ^ mask[i % mask.length])).toString('base64');
}

function loadAuth({ login = savedLogin(), apiBase = '/v0/management/clinepassbridge', encoded = true, version = 'v1', userAgent: currentUserAgent = userAgent, loggedIn = 'true', session = new Map(), persistent = new Map(), blocked = false } = {}) {
  for (const [name, value] of [['isLoggedIn', loggedIn], ['cli-proxy-auth', encoded ? encrypted(login, version) : JSON.stringify(login)]]) {
    if (value !== null && !persistent.has(name)) persistent.set(name, value);
  }
  const storage = {
    getItem: name => persistent.get(name) ?? null,
    setItem: (name, value) => persistent.set(name, value),
    removeItem: name => persistent.delete(name),
  };
  const context = vm.createContext({
    location: new URL(origin + '/v0/resource/plugins/clinepassbridge/console'),
    navigator: { userAgent: currentUserAgent },
    document: { querySelector: () => ({ content: apiBase }) },
    localStorage: storage,
    sessionStorage: {
      getItem: name => { if (blocked) throw new Error('storage blocked'); return session.get(name) ?? null; },
      setItem: (name, value) => { if (blocked) throw new Error('storage blocked'); session.set(name, value); },
      removeItem: name => { if (blocked) throw new Error('storage blocked'); session.delete(name); },
    },
    TextEncoder, TextDecoder, Uint8Array, URL, atob,
  });
  context.window = context;
  vm.runInContext(source, context);
  return context;
}

async function headers(options) {
  return JSON.parse(JSON.stringify(await loadAuth(options).PassBridgeAuthHeaders()));
}

test('reuse existing remembered login, encrypted or plain', async () => {
  for (const encoded of [true, false]) assert.deepEqual(await headers({ encoded }), { Authorization: `Bearer ${key}` });
});

test('reuse CPAMP v2 login even after the user agent changes', async () => {
  const persistent = new Map();
  for (const agent of [userAgent, 'Changed browser user agent']) {
    assert.deepEqual(await headers({ persistent, version: 'v2', userAgent: agent }), { Authorization: `Bearer ${key}` });
  }
});

test('recognize root, v0 and v8 endpoint forms including reverse-proxy prefixes', async () => {
  for (const prefix of ['', '/proxy']) {
    for (const version of ['v0', 'v8']) {
      for (const suffix of ['', '/v0/management/', '/v8/management/']) {
        assert.deepEqual(await headers({ login: savedLogin(origin + prefix + suffix), apiBase: `${prefix}/${version}/management/clinepassbridge` }), { Authorization: `Bearer ${key}` }, `${prefix} ${version} ${suffix}`);
      }
    }
  }
});

test('reject saved keys for another origin or another CPA on the same origin', async () => {
  for (const base of ['https://other.example', origin + '/other', 'http://cpa.example', 'https://user:pass@cpa.example', origin + '?server=other', origin + '#other']) {
    assert.deepEqual(await headers({ login: savedLogin(base) }), {}, base);
  }
});

test('do not reuse logged-out or unremembered persistent credentials', async () => {
  assert.deepEqual(await headers({ loggedIn: 'false' }), {});
  assert.deepEqual(await headers({ login: savedLogin(origin, false) }), {});
});

test('manual bearer and X-Management-Key survive refresh and iframe recreation within one tab', () => {
  for (const type of ['bearer', 'x-management-key']) {
    const session = new Map();
    const options = { session, login: savedLogin(origin, false), loggedIn: null };
    const first = loadAuth(options);
    first.PassBridgeSaveAuth(key, type);
    const expected = type === 'bearer' ? { Authorization: `Bearer ${key}` } : { 'X-Management-Key': key };
    assert.deepEqual(JSON.parse(JSON.stringify(loadAuth(options).PassBridgeAuthHeaders())), expected);
    assert.deepEqual(JSON.parse(JSON.stringify(loadAuth({ ...options, session: new Map() }).PassBridgeAuthHeaders())), {});
    assert.deepEqual(JSON.parse(JSON.stringify(loadAuth({ ...options, apiBase: '/other/v8/management/clinepassbridge' }).PassBridgeAuthHeaders())), {});
    first.PassBridgeClearAuth();
    assert.equal(session.size, 0);
  }
});

test('remembered manual keys survive a new session, stay endpoint scoped, and preserve panel storage on clear', async () => {
  for (const type of ['bearer', 'x-management-key']) {
    const session = new Map(), persistent = new Map();
    const options = { session, persistent };
    const context = loadAuth(options);
    const panelStorage = new Map(persistent);
    const expected = type === 'bearer' ? { Authorization: 'Bearer remembered-fixture-key' } : { 'X-Management-Key': 'remembered-fixture-key' };
    context.PassBridgeSaveAuth('remembered-fixture-key', type, true);
    assert.equal(session.size, 1);
    assert.equal(persistent.size, panelStorage.size + 1);
    assert.deepEqual(await headers({ ...options, session: new Map() }), expected);
    assert.deepEqual(await headers({ ...options, session: new Map(), apiBase: '/other/v8/management/clinepassbridge' }), {});

    const sessionOnly = new Map();
    loadAuth({ session: sessionOnly }).PassBridgeSaveAuth('session-fixture-key', type);
    const sessionExpected = type === 'bearer' ? { Authorization: 'Bearer session-fixture-key' } : { 'X-Management-Key': 'session-fixture-key' };
    assert.deepEqual(await headers({ ...options, session: sessionOnly }), sessionExpected);

    context.PassBridgeClearAuth();
    assert.equal(session.size, 0);
    assert.deepEqual(persistent, panelStorage);
    assert.deepEqual(await headers({ ...options, session: new Map() }), { Authorization: `Bearer ${key}` });
  }
});

test('default and explicit session-only saves remove an old remembered key for this endpoint', async () => {
  for (const remember of [undefined, false]) {
    const session = new Map(), persistent = new Map();
    const options = { session, persistent, login: savedLogin(origin, false), loggedIn: null };
    const context = loadAuth(options);
    const otherOptions = { ...options, session: new Map(), apiBase: '/other/v8/management/clinepassbridge' };
    loadAuth(otherOptions).PassBridgeSaveAuth('other-fixture-key', 'bearer', true);
    const savedPersistent = new Map(persistent);
    context.PassBridgeSaveAuth('old-fixture-key', 'bearer', true);
    assert.equal(persistent.size, savedPersistent.size + 1);
    context.PassBridgeSaveAuth(key, 'bearer', remember);
    assert.deepEqual(persistent, savedPersistent);
    assert.deepEqual(JSON.parse(JSON.stringify(context.PassBridgeAuthHeaders())), { Authorization: `Bearer ${key}` });
    assert.deepEqual(await headers({ ...options, session: new Map() }), {});
    assert.deepEqual(await headers({ ...otherOptions, session: new Map() }), { Authorization: 'Bearer other-fixture-key' });
  }
});

test('invalid storage and blocked storage leave the page usable', () => {
  const context = loadAuth({ blocked: true });
  assert.doesNotThrow(() => context.PassBridgeSaveAuth(key, 'bearer'));
  assert.doesNotThrow(() => context.PassBridgeClearAuth());
  assert.equal(context.PassBridgeAuthHeaders().Authorization, `Bearer ${key}`);
  const session = new Map();
  const corrupt = loadAuth({ session });
  corrupt.PassBridgeSaveAuth(key, 'bearer');
  session.set([...session.keys()][0], '{broken');
  assert.equal(corrupt.PassBridgeAuthHeaders().Authorization, `Bearer ${key}`);
});

test('a delayed rejection does not clear a newer manual key', () => {
  const session = new Map(), persistent = new Map();
  const context = loadAuth({ session, persistent });
  const panelStorage = new Map(persistent);
  context.PassBridgeSaveAuth(key, 'bearer', true);
  context.PassBridgeSaveAuth('new-fixture-key', 'bearer', true);
  const savedSession = new Map(session), savedPersistent = new Map(persistent);
  context.PassBridgeClearAuth({ Authorization: `Bearer ${key}` });
  assert.deepEqual(session, savedSession);
  assert.deepEqual(persistent, savedPersistent);
  assert.equal(context.PassBridgeAuthHeaders().Authorization, 'Bearer new-fixture-key');
  assert.equal(loadAuth({ persistent }).PassBridgeAuthHeaders().Authorization, 'Bearer new-fixture-key');
  context.PassBridgeClearAuth({ Authorization: 'Bearer new-fixture-key' });
  assert.equal(session.size, 0);
  assert.deepEqual(persistent, panelStorage);
});

const html = fs.readFileSync(path.join(__dirname, '../internal/bridge/ui/index.html'), 'utf8');
function pageAPI(options) {
  const context = loadAuth(options);
  const elements = new Map();
  Object.assign(context, {
    state: { key: '', authType: 'bearer' },
    apiReady: () => true,
    apiUrl: value => value,
    $: name => {
      if (!elements.has(name)) elements.set(name, { open: false, showModal() { this.open = true; } });
      return elements.get(name);
    },
  });
  const start = html.indexOf('    async function api(');
  const end = html.indexOf('    function formatTime(', start);
  assert.ok(start >= 0 && end > start);
  vm.runInContext(html.slice(start, end), context);
  return context;
}

test('page API persists only verified manual keys and removes rejected session credentials', async () => {
  const session = new Map(), persistent = new Map();
  const options = { session, persistent, login: savedLogin(origin, false), loggedIn: null };
  const context = pageAPI(options);
  const panelStorage = new Map(persistent);
  const savedCalls = [], saveAuth = context.PassBridgeSaveAuth;
  context.PassBridgeSaveAuth = (...args) => { savedCalls.push(args); saveAuth(...args); };
  context.state.key = key;
  let status = 500;
  context.fetch = async () => ({ ok: status === 200, status, text: async () => '{}' });
  await assert.rejects(context.api('/status'), /HTTP 500/);
  assert.equal(session.size, 0);
  assert.equal(savedCalls.length, 0);
  assert.deepEqual(persistent, panelStorage);
  status = 200;
  await context.api('/status');
  assert.equal(session.size, 1);
  assert.deepEqual(savedCalls, [[key, 'bearer', false]]);
  assert.deepEqual(persistent, panelStorage);
  const refreshed = pageAPI(options);
  refreshed.fetch = async (_, request) => {
    assert.equal(request.headers.Authorization, `Bearer ${key}`);
    return { ok: false, status: 401, text: async () => '{}' };
  };
  await assert.rejects(refreshed.api('/status'), /HTTP 401/);
  assert.equal(session.size, 0);
  assert.equal(refreshed.$('authDialog').open, true);
});

test('page API remembers verified keys only when requested and clears rejected persistent credentials', async () => {
  for (const rejectedStatus of [401, 403]) {
    const session = new Map(), persistent = new Map();
    const options = { session, persistent, login: savedLogin(origin, false), loggedIn: null };
    const context = pageAPI(options);
    const panelStorage = new Map(persistent);
    context.state.key = key;
    context.state.rememberAuth = true;
    let status = 500;
    context.fetch = async () => ({ ok: status === 200, status, text: async () => '{}' });
    await assert.rejects(context.api('/status'), /HTTP 500/);
    assert.equal(session.size, 0);
    assert.deepEqual(persistent, panelStorage);
    status = 200;
    await context.api('/status');
    assert.equal(session.size, 1);
    assert.equal(persistent.size, panelStorage.size + 1);
    assert.deepEqual(await headers({ ...options, session: new Map() }), { Authorization: `Bearer ${key}` });
    const refreshed = pageAPI(options);
    refreshed.fetch = async (_, request) => {
      assert.equal(request.headers.Authorization, `Bearer ${key}`);
      return { ok: false, status: rejectedStatus, text: async () => '{}' };
    };
    await assert.rejects(refreshed.api('/status'), new RegExp(`HTTP ${rejectedStatus}`));
    assert.equal(session.size, 0);
    assert.deepEqual(persistent, panelStorage);
    assert.deepEqual(await headers({ ...options, session: new Map() }), {});
    assert.equal(refreshed.$('authDialog').open, true);
  }
});

test('page API rejects wrong manual keys without remembering them', async () => {
  for (const status of [401, 403]) {
    const session = new Map();
    const context = pageAPI({ session });
    context.state.key = 'wrong-fixture-key';
    context.state.authType = 'x-management-key';
    context.fetch = async () => ({ ok: false, status, text: async () => '{}' });
    await assert.rejects(context.api('/status'), new RegExp(`HTTP ${status}`));
    assert.equal(session.size, 0);
    assert.equal(context.state.key, '');
  }
});

test('late unauthorized responses do not reopen the dialog after a new login succeeds', async () => {
  for (const fromSession of [false, true]) {
    const session = new Map(), persistent = new Map();
    const options = { session, persistent, loggedIn: null };
    const context = pageAPI(options);
    context.state.rememberAuth = true;
    if (fromSession) context.PassBridgeSaveAuth(key, 'bearer', true);
    else context.state.key = key;
    let reply, started;
    const requestStarted = new Promise(resolve => { started = resolve; });
    context.fetch = () => { started(); return new Promise(resolve => { reply = resolve; }); };
    const oldRequest = context.api('/logs');
    await requestStarted;
    context.state.key = 'new-fixture-key';
    context.fetch = async () => ({ ok: true, status: 200, text: async () => '{}' });
    await context.api('/status');
    const savedSession = new Map(session), savedPersistent = new Map(persistent);
    assert.equal(loadAuth({ ...options, session: new Map() }).PassBridgeAuthHeaders().Authorization, 'Bearer new-fixture-key');
    context.$('authButton').textContent = '密钥已输入';
    reply({ ok: false, status: 401, text: async () => '{}' });
    await assert.rejects(oldRequest, /HTTP 401/);
    assert.equal(context.$('authDialog').open, false);
    assert.equal(context.$('authButton').textContent, '密钥已输入');
    assert.equal(context.state.key, 'new-fixture-key');
    assert.equal(context.PassBridgeAuthHeaders().Authorization, 'Bearer new-fixture-key');
    assert.deepEqual(session, savedSession);
    assert.deepEqual(persistent, savedPersistent);
  }
});
