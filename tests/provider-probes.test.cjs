const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { test } = require('node:test');

const html = fs.readFileSync(path.join(__dirname, '../internal/bridge/ui/index.html'), 'utf8');
function section(start, end) {
  const from = html.indexOf(start), to = html.indexOf(end, from);
  assert.ok(from >= 0 && to > from, `missing UI section: ${start}`);
  return html.slice(from, to);
}
function declaration(name) {
  const line = html.match(new RegExp(`^    const ${name} = .+;$`, 'm'));
  assert.ok(line, `missing UI declaration: ${name}`);
  return line[0];
}
const code = [
  declaration('pinAutosave'), declaration('policyAutosave'), declaration('modelKey'),
  section('    function renderModels(', '    async function saveModels('),
  section('    const parseProviders =', "    $('probeAllModels').addEventListener('click'"),
  'globalThis.ui = { providerProbeKey, providerProbeHTML, syncProviderProbeResults, renderPinModelRows, probeModelProviders, queuePinSave, flushPinModels };',
].join('\n');
const clone = value => JSON.parse(JSON.stringify(value));
const settle = () => new Promise(resolve => setImmediate(resolve));
function deferred() {
  let resolve;
  const promise = new Promise(yes => { resolve = yes; });
  return { promise, resolve };
}

function page(intercept = async () => {}) {
  const nodes = new Map(), calls = [];
  const model = { id: 'alias', upstream_id: 'cline-pass/deepseek-v4.1-flash-1017', providers: ['deepseek'], has: ['vision'] };
  const server = { models: [clone(model)] };
  const state = {
    models: [clone(model)], providers: new Set(), config: { provider_policy: 'config' },
    providerProbes: new Map(), providerProbePending: new Map(), providerProbeBatch: false,
    modelBusy: false, fetchingModels: false, candidateBusy: false, editingModelIndex: -1,
  };
  function node(id) {
    if (!nodes.has(id)) nodes.set(id, {
      value: '', dataset: {}, innerHTML: '', textContent: '', hidden: true,
      querySelectorAll() { return []; },
    });
    return nodes.get(id);
  }
  node('pinProbeCredential').value = 'account-a';
  const context = vm.createContext({
    state, $: node, document: { activeElement: null }, reasoningMappingBusy: false,
    credentialView: { items: [{ id: 'account-a', label: 'A' }, { id: 'account-b', label: 'B' }] },
    esc: value => String(value ?? ''), shown: value => String(value ?? ''), buttonIcon: () => '',
    present: value => value !== undefined && value !== null && value !== '',
    renderProviderOptions() {}, renderModelRows() {}, renderReasoningModels() {},
    resetModelForm() {}, renderCandidateList() {}, syncPinModelTests() {}, setButtonLabel() {},
    setTimeout: () => 1, clearTimeout() {},
    api: async (url, options = {}) => {
      const request = { url, ...clone(options) };
      calls.push(request);
      const response = await intercept(request, calls.length);
      if (response !== undefined) return response;
      if (url === '/models' && options.method === 'PUT') {
        server.models = clone(options.body.models);
        return clone(server);
      }
      if (url === '/models/providers') return {
        ok: true, upstream_id: server.models[0].upstream_id, has: clone(server.models[0].has),
        providers: ['fixture-provider'], note: 'fixture candidates',
      };
      throw new Error(`unexpected API: ${url}`);
    },
  });
  vm.runInContext(code, context);
  const ui = context.ui;
  return {
    state, server, calls, ui, node,
    model: () => state.models[0],
    result: () => node('pinProbeResult-0').innerHTML,
    editHas(value) {
      const input = { value, dataset: { hasKey: JSON.stringify([state.models[0].id, state.models[0].upstream_id]) } };
      ui.queuePinSave(input);
    },
  };
}

test('候选按能力草稿、凭据和模型映射隔离，恢复原配置时可复用对应结果', async () => {
  const view = page();
  await view.ui.probeModelProviders(view.model());
  assert.match(view.result(), /data-provider="fixture-provider"/);
  view.editHas('implicit-caching');
  assert.doesNotMatch(view.result(), /data-provider=/);
  view.editHas('vision');
  assert.match(view.result(), /data-provider="fixture-provider"/);
  view.node('pinProbeCredential').value = 'account-b';
  view.ui.syncProviderProbeResults();
  assert.doesNotMatch(view.result(), /data-provider=/);
  view.node('pinProbeCredential').value = 'account-a';
  view.state.models = [{ ...view.model(), upstream_id: 'cline-pass/another-model' }];
  view.ui.syncProviderProbeResults();
  assert.doesNotMatch(view.result(), /data-provider=/);
});

test('旧探测晚到时不会把旧能力、旧凭据或旧映射的候选显示到当前行', async () => {
  for (const change of ['capability', 'credential', 'mapping']) {
    const gate = deferred();
    const view = page(request => request.url === '/models/providers' ? gate.promise : undefined);
    const oldModel = clone(view.model());
    const pending = view.ui.probeModelProviders(view.model());
    await settle();
    assert.equal(view.calls.filter(call => call.url === '/models/providers').length, 1);
    if (change === 'capability') {
      view.editHas('implicit-caching');
      assert.equal(await view.ui.flushPinModels(), true);
    } else if (change === 'credential') {
      view.node('pinProbeCredential').value = 'account-b';
      view.ui.syncProviderProbeResults();
    } else {
      view.state.models = [{ ...view.model(), upstream_id: 'cline-pass/replaced-model' }];
      view.ui.syncProviderProbeResults();
    }
    gate.resolve({ ok: true, upstream_id: oldModel.upstream_id, has: oldModel.has, providers: ['stale-provider'], note: 'old result' });
    await pending;
    assert.doesNotMatch(view.result(), /data-provider="stale-provider"/, change);
    assert.equal(view.state.providerProbePending.size, 0);
  }
});

test('探测等待能力草稿保存完成，保存失败时保留草稿并且不发探测', async () => {
  const gate = deferred();
  const view = page(request => request.url === '/models' ? gate.promise : undefined);
  view.editHas('vision, implicit-caching');
  const pending = view.ui.probeModelProviders(view.model());
  await settle();
  assert.deepEqual(view.calls.map(call => call.url), ['/models']);
  assert.deepEqual(view.calls[0].body.models[0].has, ['vision', 'implicit-caching']);
  gate.resolve();
  await pending;
  assert.deepEqual(view.calls.map(call => call.url), ['/models', '/models/providers']);
  assert.deepEqual(clone(view.model().has), ['vision', 'implicit-caching']);
  assert.match(view.result(), /data-provider="fixture-provider"/);

  for (const failure of ['write-error', 'dropped-has']) {
    const failed = page(async request => {
      if (request.url !== '/models') return;
      if (failure === 'write-error') throw new Error('save fixture failure');
      return { models: request.body.models.map(model => ({ ...model, has: ['vision'] })) };
    });
    failed.editHas('implicit-caching');
    await failed.ui.probeModelProviders(failed.model());
    assert.deepEqual(failed.calls.map(call => call.url), ['/models'], failure);
    assert.equal(failed.node('pinModelsState').dataset.phase, 'error', failure);
    assert.deepEqual(clone(failed.model().has), ['vision'], failure);
    assert.equal(await failed.ui.flushPinModels(), false);
    assert.deepEqual(failed.calls[1].body, failed.calls[0].body, '重试仍提交未保存的能力草稿');
  }
});

test('预设按钮只出现在上游 ID 精确为 DeepSeek 1017 的行，客户端别名不决定预设', () => {
  const view = page();
  view.state.models = [
    { id: 'any-alias', upstream_id: 'cline-pass/deepseek-v4.1-flash-1017', providers: [] },
    { id: 'deepseek-v4.1-flash-1017', upstream_id: 'cline-pass/deepseek-v4.1-flash', providers: [] },
    { id: 'other', upstream_id: 'cline-pass/deepseek-v4.1-flash-1017-extra', providers: [] },
  ];
  view.ui.renderPinModelRows();
  const rows = view.node('pinModelList').innerHTML.split(/<div class="model-row"[^>]*>/).slice(1);
  assert.equal(rows.length, 3);
  assert.match(rows[0], /pin-deepseek-preset/);
  assert.doesNotMatch(rows[1], /pin-deepseek-preset/);
  assert.doesNotMatch(rows[2], /pin-deepseek-preset/);
});
