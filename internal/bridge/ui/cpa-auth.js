/* Compatibility with the official management center's cli-proxy-auth storage format.
 * Reuses the saved login for this exact CPA endpoint. Manually entered keys are
 * retained after a successful API request, in this tab unless explicitly remembered.
 */
(() => {
  'use strict';
  // A reverse proxy may strip its public prefix before CPA renders the page.
  // Resolve it once from the console URL for both API requests and saved logins.
  const apiMeta = document.querySelector('meta[name="cpa-api-base"]');
  const consolePath = location.pathname.match(/^(.*)\/v(?:0|8)\/resource\/plugins\/clinepassbridge\/console\/?$/);
  const configured = new URL(apiMeta.content, location.origin);
  const apiPath = configured.pathname.match(/\/v(?:0|8)\/management\/clinepassbridge\/?$/);
  if (consolePath && apiPath && configured.origin === location.origin && !configured.username && !configured.password && !configured.search && !configured.hash) {
    apiMeta.content = consolePath[1] + apiPath[0];
  }
  function endpoint() {
    const own = new URL(apiMeta.content, location.origin);
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
  function readManualLogin(persistent = false) {
    try {
      const name = sessionKey();
      return name ? JSON.parse((persistent ? localStorage : sessionStorage).getItem(name)) : null;
    } catch { return null; }
  }
  function readSavedLogin() {
    try {
      if (localStorage.getItem('isLoggedIn') !== 'true') return null;
      let value = localStorage.getItem('cli-proxy-auth');
      if (!value) return null;
      const v2 = value.startsWith('enc::v2::');
      if (v2 || value.startsWith('enc::v1::')) {
        const bytes = Uint8Array.from(atob(value.slice(9)), c => c.charCodeAt(0));
        const mask = new TextEncoder().encode(v2
          ? `cli-proxy-api-webui::secure-storage|v2|${location.host}`
          : `cli-proxy-api-webui::secure-storage|${location.host}|${navigator.userAgent}`);
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
    for (const login of [readManualLogin(), readManualLogin(true), readSavedLogin()]) {
      const headers = authHeaders(login);
      if (Object.keys(headers).length) return headers;
    }
    return {};
  };
  window.PassBridgeRememberAuth = () => Object.keys(authHeaders(readManualLogin(true))).length > 0;
  window.PassBridgeSaveAuth = (key, type, remember = false) => {
    const name = sessionKey();
    if (!name || typeof key !== 'string' || !key.trim()) return;
    const value = JSON.stringify({ key: key.trim(), type: type === 'x-management-key' ? type : 'bearer' });
    try { sessionStorage.setItem(name, value); } catch { /* The current page still holds the key. */ }
    try {
      if (remember) localStorage.setItem(name, value);
      else localStorage.removeItem(name);
    } catch { /* One blocked store must not prevent using the other. */ }
  };
  window.PassBridgeClearAuth = (rejectedHeaders) => {
    const name = sessionKey();
    if (!name) return;
    for (const persistent of [false, true]) {
      try {
        const saved = authHeaders(readManualLogin(persistent));
        if (!rejectedHeaders || Object.keys(saved).some(header => saved[header] === rejectedHeaders[header])) {
          (persistent ? localStorage : sessionStorage).removeItem(name);
        }
      } catch { /* Clearing a blocked store must not break the page. */ }
    }
  };
})();
