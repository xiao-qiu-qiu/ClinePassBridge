/* Notice dismissal is scoped to this console and the publisher's notice revision. */
const releaseNoticeModule = (() => {
  'use strict';
  const button = document.getElementById('releaseNoticeButton');
  const dialog = document.getElementById('releaseNoticeDialog');
  const versionLabel = document.getElementById('releaseNoticeVersion');
  const service = location.origin + location.pathname.replace(/\/+$/, '');
  const noticeRevision = dialog.dataset.noticeRevision?.trim() || '1';
  const storageKey = `clinepassbridge:notice:${service}:${encodeURIComponent(noticeRevision)}`;
  const legacyPrefix = `clinepassbridge:release-notice:${service}:`;
  let currentVersion = '';
  let dismissed = false;
  let presented = false;
  let active = false;
  let retryQueued = false;

  function acknowledge() {
    dismissed = true;
    try { localStorage.setItem(storageKey, '1'); } catch { /* Keep page-local acknowledgement. */ }
  }
  function restoreDismissal() {
    try {
      dismissed = localStorage.getItem(storageKey) === '1';
      // Only the initial revision inherits old version-based acknowledgements.
      // Explicitly publishing a new revision must prompt even existing readers.
      if (!dismissed && noticeRevision === '1') {
        for (let index = 0; index < localStorage.length; index++) {
          const key = localStorage.key(index);
          if (key?.startsWith(legacyPrefix) && localStorage.getItem(key) === '1') {
            acknowledge();
            break;
          }
        }
      }
    } catch { /* Private browsing may block storage; page state still works. */ }
  }
  function render() {
    versionLabel.textContent = currentVersion ? `v${currentVersion}` : '公告';
    button.toggleAttribute('data-unread', !!currentVersion && !dismissed);
  }
  function open(manual = false) {
    if (active || dialog.open) return;
    if (!manual) {
      if (!currentVersion || presented || dismissed) return;
      if (document.querySelector('dialog[open]')) return;
    }
    dialog.showModal();
    active = true;
    presented = true;
    render();
  }
  function retry() {
    if (retryQueued) return;
    retryQueued = true;
    queueMicrotask(() => {
      retryQueued = false;
      open();
    });
  }
  button.addEventListener('click', () => open(true));
  document.getElementById('releaseNoticeDismiss').addEventListener('click', () => dialog.close());
  document.getElementById('releaseNoticeSettings').addEventListener('click', () => {
    setTab('pin');
    dialog.close();
  });
  dialog.addEventListener('close', () => {
    if (!active) return;
    active = false;
    acknowledge();
    render();
    retry();
  });
  // close does not bubble. Capture it so authentication and other dialogs can
  // finish their own close handlers before the pending announcement is shown.
  document.addEventListener('close', retry, true);
  window.addEventListener('storage', event => {
    const legacyDismissal = noticeRevision === '1' && event.key?.startsWith(legacyPrefix);
    if ((event.key !== storageKey && !legacyDismissal) || event.newValue !== '1') return;
    acknowledge();
    render();
  });
  restoreDismissal();
  render();
  return {
    update(version) {
      if (typeof version !== 'string' || !version.trim()) return;
      currentVersion = version.trim();
      render();
      open();
    },
  };
})();

function updateReleaseNotice(version) {
  releaseNoticeModule.update(version);
}
