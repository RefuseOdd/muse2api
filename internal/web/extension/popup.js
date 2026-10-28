/*
 * Muse2API Cookie Import - popup script.
 *
 * Reads the muse.ai login cookies through the browser cookie API and posts them
 * to <service>/admin/accounts. The browser API is used on purpose: the session
 * cookies that matter are httpOnly, so document.cookie cannot see them.
 *
 * Only hatch_sess and hatch_vml are required for an account to work. Any other
 * cookie the browser holds for muse.ai is forwarded as-is when present.
 */

'use strict';

const $ = (id) => document.getElementById(id);

const STORE_KEY = 'muse2api_ext_cfg';
const ESSENTIAL = ['hatch_sess', 'hatch_vml'];

const BTN_IDLE = 'Read and import';
const BTN_BUSY = 'Reading cookies...';

const HINT_LOGIN =
  'Open https://muse.ai/ in this browser, log in, and wait until the chat interface ' +
  'is visible. Then open this popup and press "Read and import" again.';

/* ------------------------------------------------------------------------- */
/* Small helpers                                                             */
/* ------------------------------------------------------------------------- */

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

function log(text, cls) {
  const box = $('log');
  box.textContent = '';
  const span = document.createElement('span');
  if (cls) span.className = cls;
  span.textContent = text;
  box.appendChild(span);
  box.classList.add('show');
}

function setBusy(busy) {
  const btn = $('go');
  btn.disabled = Boolean(busy);
  btn.textContent = busy ? BTN_BUSY : BTN_IDLE;
}

/* Normalize the service URL: add a scheme, drop trailing slashes and /v1. */
function normBase(value) {
  let s = String(value || '').trim();
  if (!s) return '';
  if (!/^https?:\/\//i.test(s)) s = 'https://' + s;
  s = s.replace(/\/+$/, '');
  s = s.replace(/\/v1$/i, '');
  return s.replace(/\/+$/, '');
}

function isValidBase(value) {
  if (!/^https?:\/\//i.test(value)) return false;
  try {
    const url = new URL(value);
    return (url.protocol === 'http:' || url.protocol === 'https:') && Boolean(url.hostname);
  } catch (err) {
    return false;
  }
}

/* ------------------------------------------------------------------------- */
/* Chrome API wrappers (work with both promise and callback flavors)         */
/* ------------------------------------------------------------------------- */

function chromeCookiesGetAll(filter) {
  return new Promise((resolve, reject) => {
    let settled = false;
    const done = (err, list) => {
      if (settled) return;
      settled = true;
      if (err) reject(err instanceof Error ? err : new Error(errorText(err)));
      else resolve(Array.isArray(list) ? list : []);
    };
    try {
      const maybe = chrome.cookies.getAll(filter, (list) => {
        const lastError = chrome.runtime && chrome.runtime.lastError;
        done(lastError ? lastError.message : null, list);
      });
      if (maybe && typeof maybe.then === 'function') {
        maybe.then((list) => done(null, list), (err) => done(err));
      }
    } catch (err) {
      done(err);
    }
  });
}

function chromeStorageCall(method, arg) {
  return new Promise((resolve, reject) => {
    let settled = false;
    const done = (err, value) => {
      if (settled) return;
      settled = true;
      if (err) reject(err instanceof Error ? err : new Error(errorText(err)));
      else resolve(value);
    };
    try {
      const maybe = chrome.storage.local[method](arg, (value) => {
        const lastError = chrome.runtime && chrome.runtime.lastError;
        done(lastError ? lastError.message : null, value);
      });
      if (maybe && typeof maybe.then === 'function') {
        maybe.then((value) => done(null, value), (err) => done(err));
      }
    } catch (err) {
      done(err);
    }
  });
}

/* ------------------------------------------------------------------------- */
/* Cookies                                                                   */
/* ------------------------------------------------------------------------- */

async function grabCookies() {
  const all = await chromeCookiesGetAll({ domain: 'muse.ai' });
  const cookies = {};
  const expires = {};

  for (const cookie of all) {
    if (!cookie || !cookie.name) continue;
    const domain = String(cookie.domain || '').replace(/^\./, '').toLowerCase();
    if (domain !== 'muse.ai' && !domain.endsWith('.muse.ai')) continue;
    cookies[cookie.name] = cookie.value == null ? '' : String(cookie.value);
    if (typeof cookie.expirationDate === 'number' && isFinite(cookie.expirationDate)) {
      expires[cookie.name] = Math.floor(cookie.expirationDate);
    }
  }

  return { cookies, expires };
}

/* ------------------------------------------------------------------------- */
/* Stored settings                                                           */
/* ------------------------------------------------------------------------- */

async function saveCfg(base, key, label) {
  try {
    await chromeStorageCall('set', { [STORE_KEY]: { base, key, label } });
  } catch (err) {
    console.warn('muse2api: could not save settings', err);
  }
}

async function loadCfg() {
  let cfg = {};
  try {
    const stored = await chromeStorageCall('get', STORE_KEY);
    const value = stored && stored[STORE_KEY];
    if (value && typeof value === 'object') cfg = value;
  } catch (err) {
    console.warn('muse2api: could not load settings', err);
  }

  if (typeof cfg.base === 'string' && cfg.base) $('base').value = cfg.base;
  if (typeof cfg.key === 'string' && cfg.key) $('key').value = cfg.key;
  if (typeof cfg.label === 'string' && cfg.label) $('label').value = cfg.label;

  if (!cfg.base) await prefillFromActiveTab();
}

/* First run: if the popup was opened on the admin page, reuse URL and key. */
async function prefillFromActiveTab() {
  let tab = null;
  try {
    const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
    tab = tabs && tabs[0];
  } catch (err) {
    return;
  }
  if (!tab || typeof tab.url !== 'string') return;

  let url;
  try {
    url = new URL(tab.url);
  } catch (err) {
    return;
  }
  if (!url.pathname.includes('/admin')) return;

  $('base').value = url.origin;

  const fromSearch = new URLSearchParams(url.search).get('key');
  const fromHash = new URLSearchParams(String(url.hash || '').replace(/^#/, '')).get('key');
  const key = fromSearch || fromHash;
  if (key) $('key').value = key;
}

/* ------------------------------------------------------------------------- */
/* Import flow                                                               */
/* ------------------------------------------------------------------------- */

function pickError(data, rawText) {
  if (data && typeof data === 'object') {
    for (const field of ['detail', 'error', 'message']) {
      const value = data[field];
      if (typeof value === 'string' && value.trim()) return value.trim();
      if (value && typeof value === 'object' && typeof value.message === 'string') {
        return value.message;
      }
    }
  }
  const trimmed = String(rawText || '').trim();
  return trimmed ? trimmed.slice(0, 300) : '(empty response body)';
}

function networkHint(base) {
  return (
    'Common causes:\n' +
    '- The service URL is wrong, or the service is not running: ' + base + '\n' +
    '- The service is not reachable from this browser (wrong port, firewall, VPN).\n' +
    '- The certificate is not trusted: open ' + base + '/admin in a tab and accept it first.\n' +
    '- The browser blocked the request: reload the extension so its host permissions apply.'
  );
}

async function run() {
  const base = normBase($('base').value);
  const key = $('key').value.trim();
  const label = $('label').value.trim();

  if (!base) {
    log('Please enter the Service URL (BASE URL) first.', 'bad');
    $('base').focus();
    return;
  }
  if (!isValidBase(base)) {
    log('This Service URL does not look valid: ' + base +
      '\nUse a full address such as http://192.168.1.10:18610 or https://muse.example.com', 'bad');
    $('base').focus();
    return;
  }
  if (!key) {
    log('Please enter the API Key first.\n' +
      'You can copy it from the top of the "Account Pool" tab on the muse2api admin page. ' +
      'It starts with m2a_.', 'bad');
    $('key').focus();
    return;
  }

  setBusy(true);
  log('Reading muse.ai cookies...');

  try {
    const { cookies, expires } = await grabCookies();
    const names = Object.keys(cookies);

    if (!names.length) {
      log('No muse.ai cookies found.\n' + HINT_LOGIN, 'bad');
      return;
    }

    const missing = ESSENTIAL.filter((name) => !hasOwn(cookies, name));
    if (missing.length) {
      log('Read ' + countLabel(names.length) + ', but the core session cookies are missing: ' +
        missing.join(', ') + '.\n' +
        'This browser is not logged in to muse.ai yet. Log in until the chat interface ' +
        'is visible, then try again. Nothing was uploaded.', 'warn');
      return;
    }

    log('Read ' + countLabel(names.length) + '. Uploading to ' + base + ' ...');

    let response;
    try {
      response = await fetch(base + '/admin/accounts', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'Authorization': 'Bearer ' + key,
        },
        body: JSON.stringify({ label, cookies, expires }),
      });
    } catch (err) {
      log('Could not reach ' + base + '.\n' + errorText(err) + '\n\n' + networkHint(base), 'bad');
      return;
    }

    let rawText = '';
    try {
      rawText = await response.text();
    } catch (err) {
      rawText = '';
    }

    let data = null;
    try {
      data = JSON.parse(rawText);
    } catch (err) {
      data = null;
    }

    if (response.status === 401) {
      log('Invalid API Key (the service returned 401).\n' +
        'Copy the correct API Key from the top of the "Account Pool" tab on the ' +
        'muse2api admin page.', 'bad');
      return;
    }

    if (!response.ok) {
      log('Import failed: HTTP ' + response.status + '\n' + pickError(data, rawText), 'bad');
      return;
    }

    const added = (data && Array.isArray(data.added) && data.added[0]) || {};
    const finalLabel = added.label || label || '(auto)';
    const finalId = added.id || '?';
    const count = typeof added.cookie_count === 'number' ? added.cookie_count : names.length;
    const expiresAt = typeof added.expires_at === 'number' && added.expires_at > 0
      ? new Date(added.expires_at * 1000).toLocaleString()
      : 'unknown';

    let message = 'Imported successfully.\n' +
      'Account label: ' + finalLabel + '\n' +
      'Account ID: ' + finalId + '\n' +
      'Cookie count: ' + count + '\n' +
      'Valid until: ' + expiresAt;
    if (data && typeof data.warning === 'string' && data.warning) {
      message += '\n\nNote: ' + data.warning;
    }

    log(message, 'ok');
    await saveCfg(base, key, label);
  } catch (err) {
    log('Something went wrong: ' + errorText(err) + '\n\n' +
      'Common causes:\n' +
      '- The extension could not read cookies for muse.ai (reload the extension and try again).\n' +
      '- The browser blocked the cross-origin request.\n' +
      '- The service returned a response that could not be read.', 'bad');
  } finally {
    setBusy(false);
  }
}

/* ------------------------------------------------------------------------- */
/* Wiring                                                                    */
/* ------------------------------------------------------------------------- */

$('go').addEventListener('click', () => {
  void run();
});

for (const id of ['base', 'key', 'label']) {
  $(id).addEventListener('keydown', (event) => {
    if (event.key === 'Enter') {
      event.preventDefault();
      void run();
    }
  });
}

loadCfg().catch((err) => console.warn('muse2api: initialization failed', err));
