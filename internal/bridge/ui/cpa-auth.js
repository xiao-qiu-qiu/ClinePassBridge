/* Compatibility with the official management center's cli-proxy-auth storage format.
 * Reuses the saved login for this exact CPA endpoint. Manually entered keys are
 * retained only in this tab's sessionStorage, after a successful API request.
 */
(() => {
  'use strict';
  function endpoint() {
    const own = new URL(document.querySelector('meta[name="cpa-api-base"]').content, location.origin);
    if (own.origin !== location.origin || own.username || own.password || own.search || own.hash || !/\/v(?:0|8)\/management\/clinepassbridge\/?$/.test(own.pathname)) return null;
    return { origin: own.origin, root: own.pathname.replace(/\/v(?:0|8)\/management\/clinepassbridge\/?$/, '') };
  }
  function sessionKey() {
    const own = endpoint();
    return own ? `clinepassbridge:management-auth:${own.origin}${own.root}` : null;
  }
  function authHeaders(login) {
    if (!login || typeof login.key !== 'string' || !login.key.trim()) return {};
    return login.type === 'x-management-key' ? { 'X-Management-Key': login.key.trim() } : { Authorization: `Bearer ${login.key.trim()}` };
  }
  function readSessionLogin() {
    try {
      const name = sessionKey();
      return name ? JSON.parse(sessionStorage.getItem(name)) : null;
    } catch { return null; }
  }
  function readSavedLogin() {
    try {
      if (localStorage.getItem('isLoggedIn') !== 'true') return null;
      let value = localStorage.getItem('cli-proxy-auth');
      if (!value) return null;
      if (value.startsWith('enc::v1::')) {
        const bytes = Uint8Array.from(atob(value.slice(9)), c => c.charCodeAt(0));
        const mask = new TextEncoder().encode(`cli-proxy-api-webui::secure-storage|${location.host}|${navigator.userAgent}`);
        value = new TextDecoder().decode(bytes.map((byte, i) => byte ^ mask[i % mask.length]));
      }
      const saved = JSON.parse(value)?.state;
      if (!saved || saved.rememberPassword !== true || typeof saved.managementKey !== 'string' || !saved.managementKey.trim()) return null;
      const server = new URL(saved.apiBase);
      const own = endpoint();
      const cleanPath = path => path.replace(/\/+$/, '').replace(/\/v(?:0|8)\/management$/, '');
      if (!own || server.username || server.password || server.search || server.hash || server.origin !== own.origin || cleanPath(server.pathname) !== own.root) return null;
      return { key: saved.managementKey.trim(), base: server.href };
    } catch { return null; }
  }
  window.PassBridgeAuthHeaders = () => {
    const sessionHeaders = authHeaders(readSessionLogin());
    return Object.keys(sessionHeaders).length ? sessionHeaders : authHeaders(readSavedLogin());
  };
  window.PassBridgeSaveAuth = (key, type) => {
    try {
      const name = sessionKey();
      if (name && typeof key === 'string' && key.trim()) sessionStorage.setItem(name, JSON.stringify({ key: key.trim(), type: type === 'x-management-key' ? type : 'bearer' }));
    } catch { /* Storage can be blocked; the current page still holds the key. */ }
  };
  window.PassBridgeClearAuth = (rejectedHeaders) => {
    try {
      const name = sessionKey();
      if (!name) return;
      const saved = authHeaders(readSessionLogin());
      if (!rejectedHeaders || Object.keys(saved).some(header => saved[header] === rejectedHeaders[header])) sessionStorage.removeItem(name);
    } catch { /* Clearing a blocked store must not break the page. */ }
  };
})();
