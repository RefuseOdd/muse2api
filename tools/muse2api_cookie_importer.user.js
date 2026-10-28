// ==UserScript==
// @name         Muse2API Cookie Import Assistant
// @namespace    https://github.com/muse2api/muse2api
// @version      1.2.0
// @description  Read the muse.ai session cookies on the page and push them to the muse2api account pool. Floating panel, session status badge and clipboard backup included.
// @author       MUSE2API Contributors
// @match        https://muse.ai/*
// @match        https://*.muse.ai/*
// @updateURL    https://raw.githubusercontent.com/muse2api/muse2api/main/tools/muse2api_cookie_importer.user.js
// @downloadURL  https://raw.githubusercontent.com/muse2api/muse2api/main/tools/muse2api_cookie_importer.user.js
// @grant        GM_cookie
// @grant        GM_setClipboard
// @grant        GM_xmlhttpRequest
// @grant        GM_getValue
// @grant        GM_setValue
// @grant        GM_registerMenuCommand
// @connect      *
// @run-at       document-end
// ==/UserScript==

/*
 * Muse2API Cookie Import Assistant
 *
 * Reads the muse.ai cookies of the current browser (GM_cookie first, because
 * the session cookies are httpOnly, then document.cookie as a fallback) and
 * posts them to <service URL>/admin/accounts.
 *
 * Only hatch_sess and hatch_vml are required for an account to work. Any other
 * cookie the browser holds for muse.ai is forwarded as-is when present.
 */

(function () {
  'use strict';

  const STORE_KEY = 'muse2api_cfg';
  const ESSENTIAL = ['hatch_sess', 'hatch_vml'];

  const BALL_ID = 'muse2api-floating-ball';
  const PANEL_ID = 'muse2api-cookie-panel';
  const STATUS_ID = 'muse2api-session-status';
  const VERSION = '1.2.0';

  const PUSH_IDLE_LABEL = 'Push to account pool';

  /* ------------------------------------------------------------------ */
  /* Helpers                                                            */
  /* ------------------------------------------------------------------ */

  function hasOwn(obj, key) {
    return Object.prototype.hasOwnProperty.call(obj, key);
  }

  function countLabel(n) {
    return n === 1 ? '1 cookie' : n + ' cookies';
  }

  function errorText(err) {
    if (!err) return 'unknown error';
    if (typeof err === 'string') return err;
    if (typeof err.message === 'string' && err.message) return err.message;
    return String(err);
  }

  function pickError(data, rawText) {
    if (data && typeof data === 'object') {
      for (const field of ['detail', 'error', 'message']) {
        const value = data[field];
        if (typeof value === 'string' && value.trim()) return value.trim().slice(0, 200);
        if (value && typeof value === 'object' && typeof value.message === 'string') {
          return value.message.slice(0, 200);
        }
      }
    }
    const trimmed = String(rawText || '').trim();
    return trimmed ? trimmed.slice(0, 200) : '(empty response body)';
  }

  /* Normalize the service URL: add a scheme, drop trailing slashes and /v1. */
  function normBase(value) {
    let s = String(value || '').trim();
    if (!s) return '';
    if (!/^https?:\/\//i.test(s)) {
      const localOnly = /^(localhost|127\.|0\.0\.0\.0|10\.|192\.168\.|172\.(1[6-9]|2\d|3[01])\.|\[::1\])/i.test(s);
      s = (localOnly ? 'http://' : 'https://') + s;
    }
    s = s.replace(/\/+$/, '');
    s = s.replace(/\/v1$/i, '');
    return s.replace(/\/+$/, '');
  }

  /* ------------------------------------------------------------------ */
  /* Settings                                                           */
  /* ------------------------------------------------------------------ */

  function loadCfg() {
    let raw = null;
    try {
      if (typeof GM_getValue === 'function') raw = GM_getValue(STORE_KEY, null);
    } catch (_) {
      raw = null;
    }
    if (typeof raw === 'string') {
      try {
        raw = JSON.parse(raw);
      } catch (_) {
        raw = null;
      }
    }
    if (!raw || typeof raw !== 'object') {
      try {
        raw = JSON.parse(localStorage.getItem(STORE_KEY) || 'null');
      } catch (_) {
        raw = null;
      }
    }

    const cfg = { base: '', key: '', label: '' };
    if (raw && typeof raw === 'object') {
      if (typeof raw.base === 'string') cfg.base = raw.base;
      if (typeof raw.key === 'string') cfg.key = raw.key;
      if (typeof raw.label === 'string') cfg.label = raw.label;
    }
    return cfg;
  }

  function saveCfg(cfg) {
    const value = { base: cfg.base || '', key: cfg.key || '', label: cfg.label || '' };
    let stored = false;
    try {
      if (typeof GM_setValue === 'function') {
        GM_setValue(STORE_KEY, value);
        stored = true;
      }
    } catch (_) {
      stored = false;
    }
    if (!stored) {
      try {
        localStorage.setItem(STORE_KEY, JSON.stringify(value));
      } catch (_) {
        /* storage unavailable: settings simply are not persisted */
      }
    }
  }

  /* ------------------------------------------------------------------ */
  /* Toasts                                                             */
  /* ------------------------------------------------------------------ */

  function showToast(message, type) {
    if (!document.body) return;
    const el = document.createElement('div');
    el.textContent = message;
    const background = type === 'ok' ? '#059669' : type === 'err' ? '#dc2626' : '#2563eb';
    el.style.cssText = [
      'position:fixed',
      'bottom:24px',
      'left:50%',
      'transform:translateX(-50%)',
      'z-index:2147483700',
      'padding:10px 18px',
      'background:' + background,
      'color:#fff',
      'border-radius:10px',
      'font-size:13px',
      'font-weight:500',
      'font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Arial,sans-serif',
      'box-shadow:0 12px 30px rgba(0,0,0,0.5)',
      'transition:opacity 0.3s',
      'max-width:420px',
      'text-align:center',
      'line-height:1.5',
      'white-space:pre-line',
      'pointer-events:none',
    ].join(';') + ';';

    document.body.appendChild(el);
    setTimeout(function () {
      el.style.opacity = '0';
      setTimeout(function () { el.remove(); }, 300);
    }, 4200);
  }

  /* ------------------------------------------------------------------ */
  /* Cookies                                                            */
  /* ------------------------------------------------------------------ */

  function cookieQueries() {
    const queries = [
      { url: 'https://muse.ai/' },
      { domain: '.muse.ai' },
      { domain: 'muse.ai' },
    ];
    try {
      if (window.location && window.location.href) queries.unshift({ url: window.location.href });
    } catch (_) {
      /* ignore */
    }
    return queries;
  }

  function gmCookieList(query) {
    return new Promise(function (resolve) {
      let settled = false;
      const done = function (list) {
        if (settled) return;
        settled = true;
        resolve(Array.isArray(list) ? list : []);
      };
      try {
        const result = GM_cookie.list(query, function (cookies, error) {
          done(error ? [] : cookies);
        });
        if (result && typeof result.then === 'function') {
          result.then(done, function () { done([]); });
        }
      } catch (_) {
        done([]);
      }
      setTimeout(function () { done([]); }, 1500);
    });
  }

  async function getCookies() {
    const cookies = {};
    const expires = {};

    if (typeof GM_cookie !== 'undefined' && GM_cookie && typeof GM_cookie.list === 'function') {
      for (const query of cookieQueries()) {
        const list = await gmCookieList(query);
        for (const cookie of list) {
          if (!cookie || !cookie.name) continue;
          const domain = String(cookie.domain || '').replace(/^\./, '').toLowerCase();
          if (domain !== 'muse.ai' && !domain.endsWith('.muse.ai')) continue;
          cookies[cookie.name] = cookie.value == null ? '' : String(cookie.value);
          if (typeof cookie.expirationDate === 'number' && isFinite(cookie.expirationDate)) {
            expires[cookie.name] = Math.floor(cookie.expirationDate);
          }
        }
      }
    }

    try {
      for (const part of String(document.cookie || '').split(';')) {
        const at = part.indexOf('=');
        if (at > 0) {
          const name = part.slice(0, at).trim();
          const value = part.slice(at + 1).trim();
          if (name && !hasOwn(cookies, name)) cookies[name] = value;
        }
      }
    } catch (_) {
      /* ignore */
    }

    return { cookies: cookies, expires: expires };
  }

  function cookieString(cookies) {
    return Object.keys(cookies).map(function (name) {
      return name + '=' + cookies[name];
    }).join('; ');
  }

  async function checkSessionStatus() {
    const result = await getCookies();
    const names = Object.keys(result.cookies);
    const missing = ESSENTIAL.filter(function (name) { return !hasOwn(result.cookies, name); });
    return {
      hasCore: missing.length === 0,
      count: names.length,
      missing: missing,
      cookies: result.cookies,
      expires: result.expires,
    };
  }

  /* ------------------------------------------------------------------ */
  /* Clipboard                                                          */
  /* ------------------------------------------------------------------ */

  function copyText(text) {
    if (!text) return false;
    try {
      if (typeof GM_setClipboard === 'function') {
        GM_setClipboard(text);
        return true;
      }
    } catch (_) {
      /* fall through */
    }
    try {
      if (navigator.clipboard && typeof navigator.clipboard.writeText === 'function') {
        navigator.clipboard.writeText(text);
        return true;
      }
    } catch (_) {
      /* fall through */
    }
    try {
      const area = document.createElement('textarea');
      area.value = text;
      area.setAttribute('readonly', 'readonly');
      area.style.cssText = 'position:fixed;top:-1000px;left:-1000px;opacity:0;';
      document.body.appendChild(area);
      area.select();
      const ok = document.execCommand('copy');
      area.remove();
      return ok;
    } catch (_) {
      return false;
    }
  }

  /* ------------------------------------------------------------------ */
  /* HTTP                                                               */
  /* ------------------------------------------------------------------ */

  function httpPost(url, headers, body) {
    if (typeof GM_xmlhttpRequest === 'function') {
      return new Promise(function (resolve, reject) {
        GM_xmlhttpRequest({
          method: 'POST',
          url: url,
          headers: headers,
          data: body,
          timeout: 20000,
          onload: function (res) {
            resolve({
              ok: res.status >= 200 && res.status < 300,
              status: res.status,
              text: res.responseText || '',
            });
          },
          onerror: function () {
            reject(new Error('network error while contacting the service'));
          },
          ontimeout: function () {
            reject(new Error('the request to the service timed out'));
          },
        });
      });
    }
    return fetch(url, { method: 'POST', headers: headers, body: body }).then(function (res) {
      return res.text().then(function (text) {
        return { ok: res.ok, status: res.status, text: text };
      });
    });
  }

  /* ------------------------------------------------------------------ */
  /* Push flow                                                          */
  /* ------------------------------------------------------------------ */

  async function executePush(buttonEl) {
    const button = buttonEl || null;
    const cfg = loadCfg();
    const base = normBase(cfg.base);
    const key = String(cfg.key || '').trim();
    const label = String(cfg.label || '').trim();

    if (button) {
      button.disabled = true;
      button.textContent = 'Reading cookies...';
    }

    let backup = false;
    let cookieHeader = '';
    try {
      const result = await getCookies();
      const cookies = result.cookies;
      const expires = result.expires;
      const names = Object.keys(cookies);

      if (!names.length) {
        showToast('No muse.ai cookies found. Log in to muse.ai and open the chat interface first.', 'err');
        return;
      }

      const missing = ESSENTIAL.filter(function (name) { return !hasOwn(cookies, name); });
      if (missing.length) {
        showToast('Read ' + countLabel(names.length) + ', but the core session cookies are missing (' +
          missing.join(', ') + ').\nLog in to muse.ai, open the chat interface and try again.', 'err');
        return;
      }

      cookieHeader = cookieString(cookies);
      backup = copyText(cookieHeader);

      if (!base) {
        showToast(backup
          ? 'Cookies copied to the clipboard. Set the service URL in the panel to push them directly.'
          : 'Cookies are ready, but no service URL is configured. Set the service URL in the panel first.',
          backup ? 'ok' : 'err');
        return;
      }

      if (button) button.textContent = 'Pushing to service...';

      const headers = { 'Content-Type': 'application/json' };
      if (key) headers['Authorization'] = 'Bearer ' + key;

      const response = await httpPost(
        base + '/admin/accounts',
        headers,
        JSON.stringify({ label: label, cookies: cookies, expires: expires })
      );

      let data = null;
      try {
        data = JSON.parse(response.text);
      } catch (_) {
        data = null;
      }

      if (response.status === 401) {
        showToast('Push failed: the service rejected the API key (401).\n' +
          'Copy the key again from the top of the Account Pool tab.', 'err');
        return;
      }

      if (!response.ok) {
        showToast('Push failed (HTTP ' + response.status + '): ' + pickError(data, response.text) +
          (backup ? '\nThe cookies are on your clipboard as a backup.' : ''), 'err');
        return;
      }

      const added = (data && Array.isArray(data.added) && data.added[0]) || {};
      const finalLabel = added.label || label || '(auto)';
      const count = typeof added.cookie_count === 'number' ? added.cookie_count : names.length;
      const expiresAt = typeof added.expires_at === 'number' && added.expires_at > 0
        ? new Date(added.expires_at * 1000).toLocaleString()
        : 'unknown';

      let message = 'Account imported into muse2api.\n' +
        'Label: ' + finalLabel + '\n' +
        'ID: ' + (added.id || '?') + '\n' +
        'Cookies: ' + count + '\n' +
        'Valid until: ' + expiresAt;
      if (data && typeof data.warning === 'string' && data.warning) {
        message += '\nNote: ' + data.warning;
      }

      showToast(message, 'ok');
      updateStatusBadge();
    } catch (err) {
      showToast('Push failed: ' + errorText(err) +
        (backup ? '\nThe cookies are on your clipboard as a backup.' : ''), 'err');
    } finally {
      if (button) {
        button.disabled = false;
        button.textContent = PUSH_IDLE_LABEL;
      }
    }
  }

  async function copyCookies() {
    try {
      const result = await getCookies();
      const header = cookieString(result.cookies);
      if (!header) {
        showToast('No muse.ai cookies found. Log in to muse.ai first.', 'err');
        return;
      }
      if (copyText(header)) {
        showToast('Cookie header copied to the clipboard (' +
          countLabel(Object.keys(result.cookies).length) + ').', 'ok');
      } else {
        showToast('Could not write to the clipboard. Check the browser clipboard permissions.', 'err');
      }
    } catch (err) {
      showToast('Could not read muse.ai cookies: ' + errorText(err), 'err');
    }
  }

  /* ------------------------------------------------------------------ */
  /* Status badge                                                       */
  /* ------------------------------------------------------------------ */

  async function updateStatusBadge() {
    const badge = document.getElementById(STATUS_ID);
    if (!badge) return;
    try {
      const status = await checkSessionStatus();
      if (status.hasCore) {
        badge.style.color = '#22c55e';
        badge.textContent = 'Ready (' + status.count + ' cookies)';
      } else {
        badge.style.color = '#f59e0b';
        badge.textContent = 'Not logged in yet';
      }
    } catch (err) {
      badge.style.color = '#f59e0b';
      badge.textContent = 'Session check failed';
    }
  }

  /* ------------------------------------------------------------------ */
  /* Panel                                                              */
  /* ------------------------------------------------------------------ */

  function togglePanel() {
    const existing = document.getElementById(PANEL_ID);
    if (existing) {
      existing.remove();
      return;
    }
    showPanel();
  }

  function showPanel() {
    if (!document.body) return;
    const existing = document.getElementById(PANEL_ID);
    if (existing) existing.remove();

    const cfg = loadCfg();

    const panel = document.createElement('div');
    panel.id = PANEL_ID;
    panel.setAttribute('role', 'dialog');
    panel.setAttribute('aria-label', 'muse2api cookie importer');
    panel.innerHTML = `
      <div style="position:fixed;top:20px;right:20px;z-index:2147483550;
                  width:360px;max-width:calc(100vw - 40px);box-sizing:border-box;
                  padding:18px 20px;border-radius:16px;color:#f8fafc;
                  background:linear-gradient(180deg,rgba(15,23,42,0.97) 0%,rgba(10,15,30,0.98) 100%);
                  box-shadow:0 20px 50px rgba(0,0,0,0.6),0 0 0 1px rgba(96,165,250,0.25);
                  font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Arial,sans-serif;
                  font-size:13px;line-height:1.4;">
        <div style="display:flex;align-items:center;justify-content:space-between;
                    margin-bottom:14px;padding-bottom:10px;border-bottom:1px solid #1e293b;">
          <div style="display:flex;align-items:center;gap:8px;">
            <span style="display:inline-block;width:10px;height:10px;border-radius:50%;
                         background:#38bdf8;box-shadow:0 0 10px #38bdf8;"></span>
            <b style="font-size:15px;color:#f1f5f9;letter-spacing:0.3px;">muse2api cookie importer</b>
            <span style="font-size:11px;background:#1e293b;color:#94a3b8;padding:1px 6px;border-radius:6px;">v1.2.0</span>
          </div>
          <button id="muse2api-close" type="button" title="Close"
                  style="background:transparent;border:0;color:#64748b;font-size:18px;
                         cursor:pointer;padding:0 4px;line-height:1;">&#10005;</button>
        </div>

        <div style="display:flex;justify-content:space-between;align-items:center;gap:10px;
                    background:#0f172a;border:1px solid #1e293b;border-radius:10px;
                    padding:10px 12px;margin-bottom:14px;">
          <span style="font-size:12px;color:#94a3b8;">Browser session</span>
          <span id="muse2api-session-status" style="font-size:12px;font-weight:600;color:#94a3b8;">Checking...</span>
        </div>

        <div style="margin-bottom:12px;">
          <div style="font-size:11px;color:#94a3b8;margin-bottom:5px;font-weight:500;">Service URL (BASE URL)</div>
          <input id="muse2api-cfg-base" type="text" spellcheck="false" autocomplete="off"
                 placeholder="e.g. https://muse.example.com or http://localhost:18610"
                 style="width:100%;box-sizing:border-box;padding:8px 11px;background:#0b1120;
                        border:1px solid #334155;border-radius:8px;color:#f1f5f9;font-size:12px;
                        font-family:monospace;">
        </div>

        <div style="margin-bottom:12px;">
          <div style="font-size:11px;color:#94a3b8;margin-bottom:5px;font-weight:500;">API Key (leave empty only if the service has no auth)</div>
          <input id="muse2api-cfg-key" type="password" spellcheck="false" autocomplete="off"
                 placeholder="m2a_..."
                 style="width:100%;box-sizing:border-box;padding:8px 11px;background:#0b1120;
                        border:1px solid #334155;border-radius:8px;color:#f1f5f9;font-size:12px;
                        font-family:monospace;">
        </div>

        <div style="margin-bottom:16px;">
          <div style="font-size:11px;color:#94a3b8;margin-bottom:5px;font-weight:500;">Account label (optional)</div>
          <input id="muse2api-cfg-label" type="text" spellcheck="false" autocomplete="off"
                 placeholder="e.g. acc-01 (generated when empty)"
                 style="width:100%;box-sizing:border-box;padding:8px 11px;background:#0b1120;
                        border:1px solid #334155;border-radius:8px;color:#f1f5f9;font-size:12px;">
        </div>

        <button id="muse2api-action-push" type="button"
                style="width:100%;padding:10px 0;border:0;border-radius:10px;
                       background:linear-gradient(135deg,#0284c7,#2563eb);color:#fff;
                       font-weight:600;font-size:13px;cursor:pointer;
                       box-shadow:0 4px 14px rgba(37,99,235,0.4);">
          Push to account pool
        </button>

        <div style="display:flex;gap:8px;margin-top:8px;">
          <button id="muse2api-action-copy" type="button"
                  style="flex:1;padding:7px 0;border:1px solid #334155;border-radius:8px;
                         background:#0f172a;color:#cbd5e1;font-size:12px;cursor:pointer;">
            Copy cookies
          </button>
          <button id="muse2api-action-save" type="button"
                  style="flex:1;padding:7px 0;border:1px solid #334155;border-radius:8px;
                         background:#0f172a;color:#cbd5e1;font-size:12px;cursor:pointer;">
            Save settings
          </button>
        </div>

        <div style="margin-top:14px;padding-top:10px;border-top:1px solid #1e293b;
                    display:flex;justify-content:space-between;align-items:center;gap:8px;
                    font-size:11px;color:#64748b;">
          <span>Settings stay in this browser</span>
          <span>Press Esc to close</span>
        </div>
      </div>
    `;

    document.body.appendChild(panel);

    const baseInput = document.getElementById('muse2api-cfg-base');
    const keyInput = document.getElementById('muse2api-cfg-key');
    const labelInput = document.getElementById('muse2api-cfg-label');
    const pushButton = document.getElementById('muse2api-action-push');
    const copyButton = document.getElementById('muse2api-action-copy');
    const saveButton = document.getElementById('muse2api-action-save');
    const closeButton = document.getElementById('muse2api-close');

    /* Values are assigned as properties, never interpolated into markup. */
    baseInput.value = cfg.base || '';
    keyInput.value = cfg.key || '';
    labelInput.value = cfg.label || '';

    function readForm() {
      return {
        base: normBase(baseInput.value),
        key: keyInput.value.trim(),
        label: labelInput.value.trim(),
      };
    }

    function persist() {
      const next = readForm();
      saveCfg(next);
      return next;
    }

    [baseInput, keyInput, labelInput].forEach(function (input) {
      input.addEventListener('change', function () { persist(); });
    });

    closeButton.addEventListener('click', function () { panel.remove(); });

    saveButton.addEventListener('click', function () {
      persist();
      showToast('Settings saved in this browser.', 'ok');
    });

    copyButton.addEventListener('click', function () {
      copyCookies();
    });

    pushButton.addEventListener('click', function () {
      persist();
      executePush(pushButton);
    });

    updateStatusBadge();
  }

  /* ------------------------------------------------------------------ */
  /* Floating trigger                                                   */
  /* ------------------------------------------------------------------ */

  function mountFloatingTrigger() {
    if (!document.body) return;
    if (document.getElementById(BALL_ID)) return;

    const ball = document.createElement('button');
    ball.id = BALL_ID;
    ball.type = 'button';
    ball.title = 'Open the muse2api cookie importer';
    ball.textContent = 'M2A';
    ball.style.cssText = [
      'position:fixed',
      'bottom:24px',
      'right:24px',
      'z-index:2147483500',
      'width:46px',
      'height:46px',
      'border:0',
      'border-radius:50%',
      'background:linear-gradient(135deg,#0284c7,#2563eb)',
      'color:#fff',
      'display:flex',
      'align-items:center',
      'justify-content:center',
      'cursor:pointer',
      'box-shadow:0 8px 24px rgba(37,99,235,0.45)',
      'font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Arial,sans-serif',
      'font-size:12px',
      'font-weight:700',
      'letter-spacing:0.5px',
      'user-select:none',
      'transition:transform 0.2s,box-shadow 0.2s',
    ].join(';') + ';';

    ball.addEventListener('click', togglePanel);
    ball.addEventListener('mouseenter', function () {
      ball.style.transform = 'scale(1.08)';
    });
    ball.addEventListener('mouseleave', function () {
      ball.style.transform = 'scale(1)';
    });

    document.body.appendChild(ball);
  }

  function ensureMounted() {
    if (document.body && !document.getElementById(BALL_ID)) mountFloatingTrigger();
  }

  /* ------------------------------------------------------------------ */
  /* Bootstrap                                                          */
  /* ------------------------------------------------------------------ */

  document.addEventListener('keydown', function (event) {
    if (event.key !== 'Escape') return;
    const panel = document.getElementById(PANEL_ID);
    if (panel) panel.remove();
  });

  if (typeof GM_registerMenuCommand === 'function') {
    try {
      GM_registerMenuCommand('Open the muse2api cookie importer', togglePanel);
      GM_registerMenuCommand('Push the current account now', function () { executePush(null); });
    } catch (_) {
      /* menu commands are optional */
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', ensureMounted);
  } else {
    ensureMounted();
  }

  window.addEventListener('load', ensureMounted);
  setInterval(ensureMounted, 3000);

  console.info('[muse2api] cookie importer v' + VERSION + ' loaded');
})();
