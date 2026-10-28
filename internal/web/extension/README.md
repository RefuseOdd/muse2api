Muse2API Cookie Import (browser extension)
==========================================

A small Manifest V3 extension that copies the muse.ai login session from your
browser into a muse2api service. No Python, no terminal, no separate cookie
helper.

What it does:

* Reads the muse.ai cookies the service needs (`hatch_sess` and `hatch_vml`)
  through the browser cookie API, including the httpOnly cookies that page
  scripts cannot see.
* Sends `{ "label": ..., "cookies": {...}, "expires": {...} }` to
  `<your service URL>/admin/accounts` with your API key.
* Remembers the service URL, API key and account label in
  `chrome.storage.local` on this machine only.


1. Installation
---------------

All Chromium browsers install this folder the same way: through developer mode
and "load unpacked". Unpack the folder first if you received a ZIP (for example
`muse2api-extension.zip` downloaded from the muse2api admin console), and keep
it somewhere permanent - the browser loads the files from that location every
time it starts.

The folder must keep this layout:

    manifest.json
    popup.html
    popup.js
    _locales/en/messages.json
    README.md

### Google Chrome

1. Open `chrome://extensions` in the address bar.
2. Turn on "Developer mode" (top right corner).
3. Click "Load unpacked" (top left corner).
4. Select the folder that contains `manifest.json` (the folder itself, not a
   file inside it).
5. Optional: click the puzzle-piece icon in the toolbar and pin
   "Muse2API Cookie Import" so the icon stays visible.

### Microsoft Edge

1. Open `edge://extensions` in the address bar.
2. Turn on "Developer mode" (left side of the page).
3. Click "Load unpacked".
4. Select the folder that contains `manifest.json`.
5. Optional: pin the extension from the extensions menu in the toolbar.

### Other Chromium browsers

Brave (`brave://extensions`), Vivaldi (`vivaldi://extensions`), Opera
(`opera://extensions`), Chromium and other Chromium-based browsers follow the
same steps: enable developer mode, choose "Load unpacked", pick this folder.

### Updating

Replace the files in the same folder and press the reload button on the
extension card in the extensions page. Your saved service URL and API key are
kept.


2. Usage
--------

1. In this same browser, open <https://muse.ai/> and log in. Wait until you can
   see the chat interface - the session cookies only become valid at that point.
2. Open the muse2api admin page, go to the "Account Pool" tab and copy the BASE
   URL and the API key from the top of the page. The API key starts with `m2a_`.
3. Click the Muse2API Cookie Import icon in the toolbar.
4. Fill in:
   * Service URL (BASE URL) - for example `http://192.168.1.10:18610` or
     `https://muse.example.com`. A missing scheme becomes `https://`, and
     trailing slashes and a trailing `/v1` are removed automatically.
   * API Key - the `m2a_...` value from the admin page.
   * Account label (optional) - a display name for this account, for example
     `muse-main`. Leave it empty to let the service generate one.
5. Click "Read and import".

If the popup was opened while the admin page was the active tab, the service URL
and API key are pre-filled for you (the admin page passes the key as a `?key=`
query parameter). Settings are saved after a successful import, so the next
import is a single click.

A successful import shows the account label, the account ID, the number of
cookies that were sent and the expiry date. If the service reports a missing
cookie warning, the account was still created but may not be able to generate
until the browser session is fully logged in.


3. Troubleshooting / FAQ
------------------------

**"No muse.ai cookies found."**
The browser has not logged in to muse.ai yet. Open <https://muse.ai/>, log in,
wait for the chat interface, then try again.

**"Read N cookies, but the core session cookies are missing: hatch_sess,
hatch_vml."**
The login did not finish, or the page was closed before the chat UI loaded.
Log in again, open a chat, and retry. Nothing is uploaded in this case, so no
broken account is created in the pool.

**"Invalid API Key (the service returned 401)."**
Copy the API key again from the top of the "Account Pool" tab. Watch for stray
spaces and make sure the whole key, starting with `m2a_`, was pasted.

**"Could not reach ..." / network error.**
Check that the service is running and that the URL answers in a normal tab
(open `<service URL>/admin`). Common causes: wrong port, a firewall between the
browser and the server, a self-signed certificate that has not been accepted
yet, or a reverse proxy that only serves the API under a different path.

**The account is created but generation fails.**
The cookies expired. Log in to muse.ai again and import once more; the service
replaces the cookies of the account with the same login.

**Does the extension need any account or sign-in of its own?**
No. It uses the muse.ai session that already exists in your browser and the API
key you paste into the popup.

**Which cookies does it require?**
`hatch_sess` and `hatch_vml` only. Other muse.ai cookies (analytics, preferences,
routing helpers) are forwarded when present, but they are never required, and
the service re-issues the routing ones on its own.

**Where are my settings stored?**
In `chrome.storage.local` (extension storage) on this machine, under the key
`muse2api_ext_cfg`. Removing the extension removes them.


4. Privacy
----------

* The extension reads only cookies for the `muse.ai` domain, using the
  `chrome.cookies` permission.
* It sends exactly one request: a `POST` to `<the service URL you enter>/admin/accounts`
  with your label, the muse.ai cookies and their expiry times.
* No analytics, no telemetry, no third-party server, no remote code. All logic
  lives in `popup.js`, which you can read.
* Permissions used: `cookies` (read the muse.ai session), `storage` (remember
  your settings), `activeTab` (pre-fill the service URL and key when the popup
  is opened on the admin page), plus host permissions for `muse.ai` and for
  arbitrary `http`/`https` services so the request can reach your own server.


5. Uninstalling
---------------

Open `chrome://extensions` or `edge://extensions`, find "Muse2API Cookie Import"
and click "Remove". Nothing else on your system is modified.
