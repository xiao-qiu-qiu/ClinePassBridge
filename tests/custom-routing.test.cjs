const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { test } = require('node:test');

const html = fs.readFileSync(path.join(__dirname, '../internal/bridge/ui/index.html'), 'utf8');
const clone = (value) => JSON.parse(JSON.stringify(value));
function section(start, end) {
  const from = html.indexOf(start), to = html.indexOf(end, from);
  assert.ok(from >= 0 && to > from, `missing UI section: ${start}`);
  return html.slice(from, to);
}
function declaration(name) {
  const match = html.match(new RegExp(`^    const ${name} = .+;$`, 'm'));
  assert.ok(match, `missing declaration: ${name}`);
  return match[0];
}
const source = [
  ...['pinAutosave', 'pinRoutingEditor', 'modelKey', 'modelTestKey', 'esc'].map(declaration),
  section('    const parseProviders =', '    function applyDeepseekPinPreset('),
  section('    function renderPinModelRows()', '    async function probeModelProviders('),
  'globalThis.ui = { openPinRouting, savePinRoutingSettings, orderedPinModels, presetProviderOptions, parsePinProviderOptions, routingSettingsKey, providerProbeKey, modelTestKey, renderPinModelRows };',
].join('\n');
function page(intercept = async () => {}) {
  const models = [
    { id: 'other', upstream_id: 'cline-pass/glm-5.3', providers: [], has: [], reasoning_mapping: { enabled: true } },
    { id: 'ds-alias', upstream_id: 'cline-pass/deepseek-v4.1-flash-1017', providers: ['deepseek'], has: ['vision', 'implicit-caching'] },
  ];
  const state = { models: clone(models), config: { provider_policy: 'config' }, modelBusy: false };
  const server = { models: clone(models) };
  const calls = [], nodes = new Map();
  function node(id) {
    if (!nodes.has(id)) nodes.set(id, {
      value: '', textContent: '', innerHTML: '', hidden: false, disabled: false, open: false,
      setAttribute() {}, showModal() { this.open = true; }, close() { this.open = false; },
    });
    return nodes.get(id);
  }
  const context = vm.createContext({
    state, $: node, TextEncoder, reasoningMappingRevision: 0, reasoningMappingBusy: false,
    dropdowns: new Map(), flushPinModels: async () => true, flushProviderPolicy: async () => true,
    pinSaveState() {}, syncProviderProbeControls() {}, syncPinModelTests() {},
    providerProbeHTML: () => '', buttonIcon: () => '', shown: (value) => value,
    renderModels: (value) => { state.models = clone(value.models); },
    api: async (url, options = {}) => {
      const request = { url, method: options.method || 'GET', body: options.body && clone(options.body) };
      calls.push(request);
      await intercept(request, server);
      if (request.method === 'PUT') server.models = clone(request.body.models);
      // Go serializes object keys in sorted order. A successful save must not
      // depend on the order in which the operator entered JSON fields.
      return JSON.parse(JSON.stringify(server, (_key, value) => value && typeof value === 'object' && !Array.isArray(value)
        ? Object.fromEntries(Object.keys(value).sort().map((key) => [key, value[key]])) : value));
    },
  });
  vm.runInContext(source, context);
  return { ui: context.ui, state, server, calls, node, model: () => state.models.find((model) => model.id === 'ds-alias') };
}

test('DeepSeek upstreams are stably first while action indices retain their original model', () => {
  const view = page();
  view.state.models = [
    { id: 'deepseek-in-name-only', upstream_id: 'cline-pass/glm-5.3', providers: [] },
    { id: 'ds-a', upstream_id: 'cline-pass/deepseek-v4.1-flash-1017', providers: ['deepseek'] },
    { id: 'kimi', upstream_id: 'cline-pass/kimi-k3', providers: [] },
    { id: 'ds-b', upstream_id: 'deepseek/deepseek-chat', providers: [] },
    { id: 'ds-c', upstream_id: 'cline-pass/DeepSeek-reasoner', providers: [] },
  ];
  const before = clone(view.state.models);
  assert.deepEqual(clone(view.ui.orderedPinModels(view.state.models).map(({ index }) => index)), [1, 3, 4, 0, 2]);
  view.ui.renderPinModelRows();
  const rows = view.node('pinModelList').innerHTML.split(/<div class="model-row"[^>]*>/).slice(1);
  assert.match(rows[0], /ds-a/);
  assert.match(rows[0], /pin-routing-options[^>]*data-index="1"/);
  assert.match(rows[1], /pin-routing-options[^>]*data-index="3"/);
  assert.deepEqual(view.state.models, before);
});

test('custom mode seeds the working official preset and saves typed JSON without changing other models', async () => {
  const view = page();
  await view.ui.openPinRouting(view.model());
  const seed = JSON.parse(view.node('pinProviderOptions').value);
  assert.equal(seed.gateway.inferenceRegion.providers['openai-compatible-private'].geoRegion, 'us');
  view.node('pinRoutingMode').value = 'custom';
  const options = { openaiCompat: { max_tokens: -1, strictJsonSchema: 0, reasoningEffort: false }, gateway: { has: null, only: ['deepseek'] } };
  view.node('pinProviderOptions').value = JSON.stringify(options);
  view.server.models[0].reasoning_mapping = { enabled: false, rules: { high: 'max' } };
  assert.equal(await view.ui.savePinRoutingSettings(), true);
  assert.deepEqual(view.model().provider_options, options);
  assert.equal(view.model().routing_mode, 'custom');
  assert.deepEqual(view.server.models[0].reasoning_mapping, { enabled: false, rules: { high: 'max' } });
  assert.deepEqual(view.model().providers, ['deepseek']);
  assert.equal(view.node('pinRoutingDialog').open, false);
});

test('invalid JSON and incorrect wrappers send no configuration writes', async () => {
  for (const raw of ['{', 'null', '[]', 'false', '{"providerOptions":{"gateway":{}}}']) {
    const view = page();
    await view.ui.openPinRouting(view.model());
    view.node('pinRoutingMode').value = 'custom';
    view.node('pinProviderOptions').value = raw;
    assert.equal(await view.ui.savePinRoutingSettings(), false, raw);
    assert.equal(view.calls.length, 0, raw);
    assert.equal(view.node('pinRoutingDialog').open, true);
    assert.match(view.node('pinRoutingState').textContent, /JSON 无效/);
  }
});

test('a failed custom save retains the editor and succeeds on retry', async () => {
  let fail = true;
  const view = page(async (request) => { if (request.method === 'PUT' && fail) throw new Error('offline'); });
  await view.ui.openPinRouting(view.model());
  view.node('pinRoutingMode').value = 'custom';
  view.node('pinProviderOptions').value = '{"gateway":{"only":["deepseek"]}}';
  const draft = view.node('pinProviderOptions').value;
  assert.equal(await view.ui.savePinRoutingSettings(), false);
  assert.equal(view.node('pinProviderOptions').value, draft);
  assert.equal(view.node('pinRoutingDialog').open, true);
  assert.equal(view.node('savePinRouting').disabled, false);
  fail = false;
  assert.equal(await view.ui.savePinRoutingSettings(), true);
  assert.equal(view.calls.filter((call) => call.method === 'PUT').length, 2);
});

test('switching back to preset preserves the last saved JSON even after an invalid draft', async () => {
  const view = page();
  view.state.models[1].routing_mode = view.server.models[1].routing_mode = 'custom';
  const options = { gateway: { inferenceRegion: { scope: 'zone', geoRegion: 'eu' } } };
  view.state.models[1].provider_options = view.server.models[1].provider_options = options;
  await view.ui.openPinRouting(view.model());
  view.node('pinRoutingMode').value = 'preset';
  view.node('pinProviderOptions').value = '{';
  assert.equal(await view.ui.savePinRoutingSettings(), true);
  assert.equal(view.model().routing_mode, 'preset');
  assert.deepEqual(view.model().provider_options, options);
});

test('concurrent routing edits are rejected without overwriting the newer server configuration', async () => {
  const view = page();
  await view.ui.openPinRouting(view.model());
  view.node('pinRoutingMode').value = 'custom';
  view.node('pinProviderOptions').value = '{}';
  view.server.models[1].routing_mode = 'custom';
  view.server.models[1].provider_options = { gateway: { only: ['another-provider'] } };
  assert.equal(await view.ui.savePinRoutingSettings(), false);
  assert.equal(view.calls.filter((call) => call.method === 'PUT').length, 0);
  assert.match(view.node('pinRoutingState').textContent, /其他位置更新/);
});

test('empty custom parameters are valid and routing changes invalidate old model results', async () => {
  const view = page();
  const before = view.ui.modelTestKey(view.model(), 'credential', '');
  await view.ui.openPinRouting(view.model());
  view.node('pinRoutingMode').value = 'custom';
  view.node('pinProviderOptions').value = '{}';
  assert.equal(await view.ui.savePinRoutingSettings(), true);
  assert.deepEqual(view.model().provider_options, {});
  assert.notEqual(view.ui.modelTestKey(view.model(), 'credential', ''), before);
  const key = view.ui.providerProbeKey(view.model(), 'credential');
  view.model().provider_options = { gateway: { has: ['reasoning'] } };
  assert.notEqual(view.ui.providerProbeKey(view.model(), 'credential'), key);
  view.ui.renderPinModelRows();
  assert.match(view.node('pinModelList').innerHTML, /自定义 JSON/);
});
