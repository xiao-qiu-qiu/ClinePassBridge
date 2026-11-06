const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { test } = require('node:test');

const source = fs.readFileSync(path.join(__dirname, '../internal/bridge/ui/release-notice.js'), 'utf8');
const markup = fs.readFileSync(path.join(__dirname, '../internal/bridge/ui/index.html'), 'utf8');
const publishedRevision = markup.match(/<dialog id="releaseNoticeDialog" data-notice-revision="([^"]+)"/)[1];

function page({ store = new Map(), blockedStorage = false, pathname = '/v0/resource/plugins/clinepassbridge/console', authOpen = false, noticeRevision = publishedRevision } = {}) {
  const pending = [];
  const nodes = new Map();
  const documentEvents = new Map();
  const windowEvents = new Map();
  const tabs = [];
  class Node {
    constructor(id, open = false) {
      this.id = id;
      this.open = open;
      this.events = new Map();
      this.attributes = new Set();
      this.openCount = 0;
      this.dataset = id === 'releaseNoticeDialog' ? { noticeRevision } : {};
    }
    addEventListener(name, fn) {
      const handlers = this.events.get(name) || [];
      handlers.push(fn);
      this.events.set(name, handlers);
    }
    emit(name, event = {}) {
      for (const fn of this.events.get(name) || []) fn(event);
    }
    toggleAttribute(name, enabled) { enabled ? this.attributes.add(name) : this.attributes.delete(name); }
    showModal() { this.open = true; this.openCount++; }
    close() {
      if (!this.open) return;
      this.open = false;
      for (const fn of documentEvents.get('close') || []) fn({ target: this });
      this.emit('close');
    }
    escape() {
      const event = { defaultPrevented: false, preventDefault() { this.defaultPrevented = true; } };
      this.emit('cancel', event);
      if (!event.defaultPrevented) this.close();
    }
  }
  for (const id of ['releaseNoticeButton', 'releaseNoticeDialog', 'releaseNoticeVersion', 'releaseNoticeDismiss', 'releaseNoticeSettings', 'authDialog']) {
    nodes.set(id, new Node(id, id === 'authDialog' && authOpen));
  }
  const localStorage = {
    get length() { if (blockedStorage) throw Error('blocked'); return store.size; },
    key(index) { if (blockedStorage) throw Error('blocked'); return [...store.keys()][index] ?? null; },
    getItem(key) { if (blockedStorage) throw Error('blocked'); return store.get(key) ?? null; },
    setItem(key, value) { if (blockedStorage) throw Error('blocked'); store.set(key, value); },
  };
  const context = vm.createContext({
    document: {
      getElementById: id => nodes.get(id),
      querySelector: selector => selector === 'dialog[open]' && [...nodes.values()].find(node => node.id.endsWith('Dialog') && node.open),
      addEventListener(name, fn) {
        const handlers = documentEvents.get(name) || [];
        handlers.push(fn);
        documentEvents.set(name, handlers);
      },
    },
    window: { addEventListener: (name, fn) => windowEvents.set(name, fn) },
    location: { origin: 'https://cpa.example', pathname },
    localStorage,
    queueMicrotask: fn => pending.push(fn),
    setTab: name => tabs.push(name),
  });
  vm.runInContext(source, context);
  const flush = () => { while (pending.length) pending.shift()(); };
  const button = nodes.get('releaseNoticeButton');
  const dialog = nodes.get('releaseNoticeDialog');
  return {
    store, nodes, button, dialog, tabs, flush,
    update: version => context.updateReleaseNotice(version),
    click: id => { nodes.get(id).emit('click'); flush(); },
    unread: () => button.attributes.has('data-unread'),
    storage: event => windowEvents.get('storage')(event),
  };
}

test('first visit presents once; dismissal persists across refresh and plugin upgrades', () => {
  const first = page();
  first.update('0.18.0');
  assert.equal(first.dialog.openCount, 1);
  assert.equal(first.unread(), true);
  first.update('0.18.0');
  assert.equal(first.dialog.openCount, 1);
  first.click('releaseNoticeDismiss');
  assert.equal(first.unread(), false);
  first.update('0.18.0');
  assert.equal(first.dialog.openCount, 1);

  const refreshed = page({ store: first.store });
  refreshed.update('0.18.0');
  assert.equal(refreshed.dialog.open, false);
  refreshed.update('0.19.0');
  assert.equal(refreshed.dialog.open, false);
  assert.equal(refreshed.unread(), false);
  assert.equal(refreshed.nodes.get('releaseNoticeVersion').textContent, 'v0.19.0');
  const upgraded = page({ store: first.store });
  upgraded.update('0.20.0');
  assert.equal(upgraded.dialog.open, false);
  const otherService = page({ store: first.store, pathname: '/other/v0/resource/plugins/clinepassbridge/console' });
  otherService.update('0.18.0');
  assert.equal(otherService.dialog.open, true);
});

test('已读旧公告的用户收到本次不钉公告，确认后刷新不再弹出', () => {
  const old = page({ noticeRevision: '1' });
  old.update('0.4.6');
  old.click('releaseNoticeDismiss');
  const upgraded = page({ store: old.store });
  upgraded.update('0.4.7');
  assert.equal(upgraded.dialog.open, true);
  upgraded.click('releaseNoticeSettings');
  assert.deepEqual(upgraded.tabs, ['pin']);
  const refreshed = page({ store: old.store });
  refreshed.update('0.4.7');
  assert.equal(refreshed.dialog.open, false);
});

test('only an explicitly published new notice revision prompts existing readers again', () => {
  const first = page({ noticeRevision: '1' });
  first.update('1.0');
  first.click('releaseNoticeDismiss');
  const next = page({ store: first.store, noticeRevision: '2' });
  next.update('1.0');
  assert.equal(next.dialog.open, true, 'a notice revision can change without a plugin upgrade');
  assert.equal(next.unread(), true);
  next.click('releaseNoticeDismiss');
  const upgraded = page({ store: first.store, noticeRevision: '2' });
  upgraded.update('2.0');
  assert.equal(upgraded.dialog.open, false);
  assert.equal(upgraded.unread(), false);
});

test('initial revision inherits this service legacy dismissals; later revisions do not', () => {
  const legacyKey = 'clinepassbridge:release-notice:https://cpa.example/v0/resource/plugins/clinepassbridge/console:0.18.0';
  const store = new Map([[legacyKey, '1']]);
  const migrated = page({ store, noticeRevision: '1' });
  migrated.update('1.0');
  assert.equal(migrated.dialog.open, false);
  assert.equal(migrated.unread(), false);
  const [newKey] = [...store.keys()].filter(key => key !== legacyKey);
  assert.ok(newKey.startsWith('clinepassbridge:notice:'));
  store.delete(legacyKey);
  const refreshed = page({ store, noticeRevision: '1' });
  refreshed.update('2.0');
  assert.equal(refreshed.dialog.open, false, 'migration persists independently of the legacy record');
  const next = page({ store: new Map([[legacyKey, '1']]), noticeRevision: '2' });
  next.update('2.0');
  assert.equal(next.dialog.open, true);
  const otherService = page({ store: new Map([[legacyKey, '1']]), pathname: '/other/console', noticeRevision: '1' });
  otherService.update('1.0');
  assert.equal(otherService.dialog.open, true);
  const unreadLegacy = page({ store: new Map([[legacyKey, '0']]), noticeRevision: '1' });
  unreadLegacy.update('1.0');
  assert.equal(unreadLegacy.dialog.open, true);
});

test('manual bell reopens read notices; settings navigates and acknowledges; Escape also acknowledges', () => {
  const view = page();
  view.update('1.0');
  view.dialog.escape();
  view.flush();
  assert.equal(view.unread(), false);
  assert.equal(view.store.size, 1);
  view.click('releaseNoticeButton');
  assert.equal(view.dialog.open, true);
  view.click('releaseNoticeSettings');
  assert.deepEqual(view.tabs, ['pin']);
  assert.equal(view.dialog.open, false);
  const refreshed = page({ store: view.store });
  refreshed.update('1.0');
  assert.equal(refreshed.dialog.open, false);
});

test('automatic notice waits for authentication and any other open dialog to close', () => {
  const view = page({ authOpen: true });
  view.update('1.0');
  assert.equal(view.dialog.open, false);
  view.nodes.get('authDialog').close();
  assert.equal(view.dialog.open, false, 'notice waits for all close handlers');
  view.flush();
  assert.equal(view.dialog.open, true);
  assert.equal(view.dialog.openCount, 1);
});

test('a manual notice can be acknowledged before authentication returns the plugin version', () => {
  for (const versionArrivesWhileOpen of [false, true]) {
    const view = page();
    view.click('releaseNoticeButton');
    assert.equal(view.dialog.open, true);
    assert.equal(view.nodes.get('releaseNoticeVersion').textContent, '公告');
    if (versionArrivesWhileOpen) view.update('1.0');
    view.click('releaseNoticeDismiss');
    view.update('1.0');
    assert.equal(view.store.size, 1);
    assert.equal(view.unread(), false);
    assert.equal(view.dialog.openCount, 1);
  }
});

test('blocked storage still acknowledges within this page and cross-tab acknowledgement cancels a pending notice', () => {
  const blocked = page({ blockedStorage: true });
  blocked.update('1.0');
  blocked.click('releaseNoticeDismiss');
  blocked.update('2.0');
  assert.equal(blocked.dialog.openCount, 1);
  assert.equal(blocked.unread(), false);

  const first = page();
  first.update('1.0');
  first.click('releaseNoticeDismiss');
  const [key, value] = [...first.store][0];
  const pending = page({ authOpen: true });
  pending.update('1.0');
  pending.storage({ key, newValue: value });
  pending.nodes.get('authDialog').close();
  pending.flush();
  assert.equal(pending.dialog.open, false);
  assert.equal(pending.unread(), false);
});

test('cross-tab legacy dismissal migrates only the initial revision and ignores other services', () => {
  const legacyKey = 'clinepassbridge:release-notice:https://cpa.example/v0/resource/plugins/clinepassbridge/console:0.18.0';
  for (const noticeRevision of ['1', '2']) {
    const pending = page({ authOpen: true, noticeRevision });
    pending.update('1.0');
    pending.storage({ key: legacyKey, newValue: '1' });
    pending.nodes.get('authDialog').close();
    pending.flush();
    assert.equal(pending.dialog.open, noticeRevision === '2');
    assert.equal(pending.unread(), noticeRevision === '2');
  }
  const otherService = page({ authOpen: true, noticeRevision: '1', pathname: '/other/console' });
  otherService.update('1.0');
  otherService.storage({ key: legacyKey, newValue: '1' });
  otherService.nodes.get('authDialog').close();
  otherService.flush();
  assert.equal(otherService.dialog.open, true);
});
