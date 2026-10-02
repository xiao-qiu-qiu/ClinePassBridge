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

function encrypted(login) {
  const bytes = new TextEncoder().encode(JSON.stringify(login));
  const mask = new TextEncoder().encode(`cli-proxy-api-webui::secure-storage|cpa.example|${userAgent}`);
  return 'enc::v1::' + Buffer.from(bytes.map((byte, i) => byte ^ mask[i % mask.length])).toString('base64');
}

function loadAuth({ login = savedLogin(), apiBase = '/v0/management/clinepassbridge', encoded = true, loggedIn = 'true', session = new Map(), blocked = false } = {}) {
  const values = new Map([['isLoggedIn', loggedIn], ['cli-proxy-auth', encoded ? encrypted(login) : JSON.stringify(login)]]);
  const storage = { getItem: name => values.get(name) ?? null };
  const context = vm.createContext({
    location: new URL(origin + '/v0/resource/plugins/clinepassbridge/console'),
    navigator: { userAgent },
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
  const context = loadAuth();
  context.PassBridgeSaveAuth('new-fixture-key', 'bearer');
  context.PassBridgeClearAuth({ Authorization: `Bearer ${key}` });
  assert.equal(context.PassBridgeAuthHeaders().Authorization, 'Bearer new-fixture-key');
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
  const session = new Map();
  const options = { session, login: savedLogin(origin, false), loggedIn: null };
  const context = pageAPI(options);
  context.state.key = key;
  let status = 500;
  context.fetch = async () => ({ ok: status === 200, status, text: async () => '{}' });
  await assert.rejects(context.api('/status'), /HTTP 500/);
  assert.equal(session.size, 0);
  status = 200;
  await context.api('/status');
  assert.equal(session.size, 1);
  const refreshed = pageAPI(options);
  refreshed.fetch = async (_, request) => {
    assert.equal(request.headers.Authorization, `Bearer ${key}`);
    return { ok: false, status: 401, text: async () => '{}' };
  };
  await assert.rejects(refreshed.api('/status'), /HTTP 401/);
  assert.equal(session.size, 0);
  assert.equal(refreshed.$('authDialog').open, true);
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
    const context = pageAPI({ loggedIn: null });
    if (fromSession) context.PassBridgeSaveAuth(key, 'bearer');
    else context.state.key = key;
    let reply, started;
    const requestStarted = new Promise(resolve => { started = resolve; });
    context.fetch = () => { started(); return new Promise(resolve => { reply = resolve; }); };
    const oldRequest = context.api('/logs');
    await requestStarted;
    context.state.key = 'new-fixture-key';
    context.fetch = async () => ({ ok: true, status: 200, text: async () => '{}' });
    await context.api('/status');
    context.$('authButton').textContent = '密钥已输入';
    reply({ ok: false, status: 401, text: async () => '{}' });
    await assert.rejects(oldRequest, /HTTP 401/);
    assert.equal(context.$('authDialog').open, false);
    assert.equal(context.$('authButton').textContent, '密钥已输入');
    assert.equal(context.state.key, 'new-fixture-key');
    assert.equal(context.PassBridgeAuthHeaders().Authorization, 'Bearer new-fixture-key');
  }
});
