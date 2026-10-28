# muse2api (Go)

A lightweight, self-contained Go service that exposes the multimodal capabilities of a signed-in [muse.ai](https://muse.ai/) web session as an OpenAI-compatible API: streaming chat, image generation/editing and asynchronous video generation.

The entire service is **one static Go binary with zero third-party Go modules**. The admin console, browser extension and version metadata are embedded. Chromium is the only external runtime dependency, and it is only used to drive muse.ai.
---

## Highlights

- **OpenAI-compatible APIs**
  - `POST /v1/chat/completions` (SSE streaming and non-streaming)
  - `POST /v1/responses` (streaming Responses API events)
  - `POST /v1/images/generations` and `POST /v1/images/edits`
  - `POST /v1/images/tasks` plus polling at `GET /v1/images/tasks/{id}`
  - `POST /v1/videos` plus polling at `GET /v1/videos/{id}`
  - `GET /v1/models`, `GET /v1/media/{name}`
- **Single binary**: admin SPA, Chrome extension ZIP and update metadata are compiled in.
- **Zero Go dependencies**: including a small RFC 6455 WebSocket client for the Chrome DevTools Protocol.
- **Multi-account pool**: isolated incognito browser contexts per task, exclusive account leases, affinity and automatic failover.
- **Session maintenance**: HTTP `/api/session` renewal, VM wake verification and a 15-minute keepalive daemon.
- **Reference image safety**: up to four ordered references, 10 MiB each / 24 MiB total; server-local paths and private URLs are denied by default, with DNS/redirect revalidation and pinned connections for public URLs.
- **Chat image input**: `image_url` parts in `/v1/chat/completions` and `input_image` items in `/v1/responses` upload into the muse.ai composer (data URLs or public URLs; same limits as reference images).
- **Async image tasks** with idempotency keys, a queue admission cap and restart recovery.
- **English-only** UI, prompts, docs and tools. A few bilingual DOM selector and quota-parser tokens remain because the live muse.ai interface and usage panel can be localized; they are not user-facing text.

---

## Requirements

- Go **1.26+** to build (no external modules are downloaded).
- Chromium, Google Chrome, Microsoft Edge or Brave on the host/container.
- About 2 GB of shared memory for Chromium when generating in parallel (`shm_size` in Docker Compose).

---

## Quick start

```bash
# Build
go build -trimpath -o muse2api ./cmd/muse2api

# Run (config is read from .env in the executable directory, or MUSE2API_HOME)
./muse2api
```

On first start, if `MUSE2API_KEY` is empty, a random key beginning with `m2a_` is generated, written to `.env`, and printed to the log. Open:

```
http://localhost:18610/admin?key=m2a_your_key
```

Copy `BASE URL` and the API key from the admin page into your client.

### Environment

Copy `.env.example` to `.env` and adjust:

| Variable | Default | Purpose |
|---|---|---|
| `MUSE2API_KEY` | auto-generated | Admin/API bearer key; a nonempty process environment value is managed by deployment configuration |
| `MUSE2API_ENV_FILE` | `.env` in home | Override the persistent settings file; Compose uses `/app/data/.env` |
| `MUSE2API_CORS_ORIGINS` | `https://muse.ai,https://www.muse.ai` | Comma-separated allowed browser origins; use `*` explicitly for unrestricted origins |
| `MUSE2API_HOST` | `0.0.0.0` | Listen address |
| `MUSE2API_PORT` | `18610` | Listen port |
| `MUSE2API_PUBLIC_BASE` | empty | Public URL used in media links; empty = relative URLs |
| `MUSE2API_CHROMIUM` | auto-detect | Browser executable |
| `MUSE2API_CDP_PORT` | `19210` | Local DevTools port |
| `MUSE2API_CONCURRENCY` | `2` | Maximum concurrent browser slots |
| `MUSE2API_IMAGE_TIMEOUT` | `240` | Image generation timeout (seconds) |
| `MUSE2API_VIDEO_TIMEOUT` | `600` | Video generation timeout (seconds) |
| `MUSE2API_CHAT_TIMEOUT` | `300` | Chat timeout (seconds) |
| `MUSE2API_REF_IMAGE_LOCAL_FILES` | `0` | Allow server-local reference file paths |
| `MUSE2API_REF_IMAGE_PRIVATE_FETCH` | `0` | Allow private/loopback reference URLs |
| `MUSE2API_TOOL_PROTOCOL` | `0` | Inject the pseudo tool-call protocol |
| `MUSE2API_KEEPALIVE_DISABLED_ACCOUNTS` | `0` | Keep disabled accounts alive without routing to them |
| `MUSE2API_REPO` | empty | Optional GitHub repo for update checks |
| `MUSE2API_REPO_BRANCH` | `main` | Branch used by update checks |
| `MUSE2API_REBUILD_CMD` | empty | Command to rebuild the binary after a source update |
| `GITHUB_TOKEN` | empty | Optional token for private repos/API limits |
| `MUSE2API_HOME_DIR` | user home | Working directory for Chromium |
| `MUSE2API_PROFILE_ROOT` | `~/muse2api-profiles` | Browser profile root |
| `MUSE2API_SITE` | `https://muse.ai/` | muse.ai site URL |
| `MUSE2API_LOGIN_WAIT` | `60` | Page readiness timeout (seconds) |

`MUSE2API_HOME` selects the base directory for `.env` and `data/` (default: the executable directory). `MUSE2API_ENV_FILE` overrides the settings-file path without relocating runtime data. Invalid or unreadable persisted account/task files stop startup instead of being treated as empty.

---

## Account import

muse.ai's session cookies are HttpOnly, so `document.cookie` cannot read them. Only a browser extension or the DevTools protocol can. Two options are included:

### 1. Browser extension (recommended)

1. In the admin console, open **Get Cookies** and download `muse2api-extension.zip`.
2. Open `chrome://extensions`, enable **Developer mode**, and load the unpacked ZIP.
3. Sign in to muse.ai in that browser until the chat UI is visible.
4. Open the extension, enter your service URL and API key, and click **Read and import**.

Only `hatch_sess` and `hatch_vml` are required. `hatch_gw` is an optional routing cache and is not needed.

### 2. Built-in cookie helper

The same binary can launch an isolated browser window, read the HttpOnly cookies and upload them:

```bash
muse2api cookie-helper --base http://your-server:18610 --key m2a_xxx --label muse-01
```

The helper uses a temporary profile, so your daily browser is untouched. Run `muse2api cookie-helper --help` for all options.

A Tampermonkey userscript is also included at `tools/muse2api_cookie_importer.user.js`.

### Session lifetime

- `hatch_vml` is the short-lived cookie (about two days) and determines the account's lifetime.
- `hatch_sess` is the main session cookie (about 30 days).
- Usage does not extend core cookie expiry. The service renews through `/api/session`, but a fresh import is still needed when the account expires.
- Import several accounts if you need continuous availability.

---

## API examples

All protected endpoints require:

```http
Authorization: Bearer m2a_your_key
```

### Chat

```bash
curl -X POST http://localhost:18610/v1/chat/completions \
  -H "Authorization: Bearer m2a_your_key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "muse-spark",
    "messages": [{"role": "user", "content": "Explain goroutine scheduling in two paragraphs."}],
    "stream": true
  }'
```

Streaming uses standard SSE chunks and terminates with `data: [DONE]`.

Multimodal messages are supported: `image_url` content parts (data URLs or public URLs, up to four images with the same 10 MiB/24 MiB limits as reference images) are uploaded into the muse.ai composer before the prompt is sent.

Files that muse creates in its VM and shares in the reply are captured automatically: they are downloaded and saved to the media library, and the assistant reply gains an `Attached files: [name](/v1/media/...)` line with working download links. Up to four files per reply, 100 MiB each; disable with `MUSE2API_CHAT_FILES=0`.

```bash
curl -X POST http://localhost:18610/v1/chat/completions \
  -H "Authorization: Bearer m2a_your_key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "muse-spark",
    "messages": [{"role": "user", "content": [
      {"type": "text", "text": "What does this screenshot show?"},
      {"type": "image_url", "image_url": {"url": "data:image/png;base64,..."}}
    ]}]
  }'
```

### Responses API

```bash
curl -X POST http://localhost:18610/v1/responses \
  -H "Authorization: Bearer m2a_your_key" \
  -H "Content-Type: application/json" \
  -d '{"model":"muse-spark","input":"Write a haiku about static binaries."}'
```

### Image generation

```bash
curl -X POST http://localhost:18610/v1/images/generations \
  -H "Authorization: Bearer m2a_your_key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "muse-image",
    "prompt": "A mechanical shiba inu in a zero-gravity suit, cinematic lighting, 8k",
    "size": "16:9",
    "response_format": "url"
  }'
```

With one or more ordered references:

```json
{
  "prompt": "Combine the subject from the first image with the palette of the second.",
  "images": [
    {"url": "https://example.com/subject.png"},
    {"url": "https://example.com/style.png"}
  ]
}
```

`/v1/images/edits` accepts JSON or `multipart/form-data` with repeated `image` fields.

### Async image tasks

Long generations can outlive reverse-proxy/client timeouts. Use tasks:

```bash
curl -X POST http://localhost:18610/v1/images/tasks \
  -H "Authorization: Bearer m2a_your_key" \
  -H "Idempotency-Key: my-stable-key" \
  -H "Content-Type: application/json" \
  -d '{"prompt":"A quiet orbital garden at dawn","size":"1:1"}'
```

Poll `GET /v1/images/tasks/{id}` every few seconds. Reusing the same idempotency key with the same request returns the original task; reusing it with a different request returns HTTP 409. At most eight queued/processing image and video tasks are accepted in total. Active tasks cannot be deleted and are preserved when clearing task history. Generation timeouts cover queued browser work and retries; releasing a browser context has a separate teardown allowance of up to two seconds.

### Video

```bash
curl -X POST http://localhost:18610/v1/videos \
  -H "Authorization: Bearer m2a_your_key" \
  -H "Content-Type: application/json" \
  -d '{"prompt":"Golden maple leaves drifting in sunlight","duration":5,"size":"16:9"}'

curl http://localhost:18610/v1/videos/task_xxx \
  -H "Authorization: Bearer m2a_your_key"
```

Video tasks are asynchronous. A completed task returns a media URL under `/v1/media/`.

### Models

`GET /v1/models` returns `muse-spark`, `muse-image` and `muse-video`. Common OpenAI/Anthropic model names are accepted as aliases and mapped to these three capabilities.

---

## Admin console

`GET /admin` provides:

- Service, account and browser health.
- Account pool import (single, cookie header or batch), test, quota, keepalive, rename, enable/disable, delete.
- Expiry warnings and quota details.
- Task history with progress and errors.
- Media library with multi-select deletion.
- API playground for image and video tasks.
- API key display, copy and rotation.
- Extension ZIP download and cookie-helper instructions.
- Integration docs and an optional update/repository panel.

---

## Using with OpenCode

[OpenCode](https://opencode.ai) works with muse2api as a custom OpenAI-compatible provider: chat, code Q&A and image understanding all work through it. Agentic tool calling does not (see below).

### Configuration

OpenCode reads config from `~/.config/opencode/opencode.jsonc` (global) or an `opencode.jsonc` in the project directory (project-level overrides global). Add:

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "muse2api": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Muse2API",
      "options": {
        "baseURL": "http://localhost:18610/v1",
        "apiKey": "m2a_your_key"
      },
      "models": {
        "muse-spark": {
          "name": "Muse Spark",
          "attachment": true,
          "tool_call": false,
          "modalities": {
            "input": ["text", "image"],
            "output": ["text"]
          }
        }
      }
    }
  }
}
```

Adapt for your deployment:

- **`baseURL`** — `http://your-server:18610/v1` for a remote or Docker deployment; `/v1` must be kept as the path suffix.
- **`apiKey`** — the service key shown at the top of the admin console (also in the server's `.env` as `MUSE2API_KEY`). Use `{env:MUSE2API_KEY}` instead of a literal to read it from the environment — note that OpenCode only sees environment variables of the terminal it launches from.
- **`modalities`** — required. OpenCode drops image content parts unless the model declares `image` input here; the error to look for is *"this model does not support image input"*.
- **`tool_call: false`** — honest default: muse.ai's assistant does not emit tool-call JSON, so agent/build modes cannot work against it. OpenCode will not send tools it knows the model cannot use.

Restart OpenCode after changing the config; it is only read at startup.

### Usage

- `/models` → **muse2api** → **Muse Spark**, then switch to the **plan** agent (Tab). Paste (Ctrl+V) or drag-drop images straight into the input; text files attach with `@`.
- One-off commands: `opencode run -m muse2api/muse-spark --agent plan "..."` (`-c` continues the last session, text-file `-f` attachments work).
- Known quirk in OpenCode 1.19.x: `opencode run --file image.png` mislabels binary images as `text/plain`, so CLI-attached images do not reach the model. Use TUI paste/drag-drop or the `opencode serve` HTTP API for images; this is an OpenCode issue, not a muse2api one.

### What works / what does not

| Capability | Status |
|---|---|
| Chat, code explanation, review, planning | Works |
| Image understanding (paste/drag into TUI) | Works |
| Tool calling / file edits / shell commands | Not possible — muse.ai refuses structured tool calls; use plan mode and apply changes yourself, or pair with a tool-capable model for the build step |

Image and video generation are outside OpenCode's chat flow; call `/v1/images/*` and `/v1/videos` directly (curl, the admin playground) or use a client that speaks those endpoints.

---

## Docker

```bash
cp .env.example .env
docker compose up -d --build
```

The image includes Chromium. Persistent data and auto-generated keys live in `./data`. An empty `MUSE2API_KEY` loads the persisted key on restart. A nonempty key supplied by the deployment environment takes precedence; change it in the deployment configuration rather than rotating it through the admin API. File-managed keys can be rotated in the admin console only after the new value is saved successfully.

## Bare metal (systemd)

```bash
go build -trimpath -o /opt/muse2api/muse2api ./cmd/muse2api
sudo cp deploy/muse2api.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now muse2api
```

If you use Nginx, copy `deploy/nginx.example.conf` and keep `proxy_buffering off` plus the long read timeouts.

---

## How it works

1. Chromium is launched headlessly with a dedicated profile and a local DevTools port.
2. Each task gets a fresh incognito `BrowserContext`, so cookies and sessions are fully isolated.
3. The service injects the account's cookies, opens `https://muse.ai/thread/new`, and waits for React hydration and the composer.
4. Prompts are typed into the real composer and sent. Results are read from the DOM's generated-media attachments.
5. Media bytes are fetched in-page (`fetch` → blob → base64), with a download-button fallback scoped to the selected attachment.
6. Video results require a real video source; a poster/cover image is never delivered as a video.
7. `/api/session` renews cookies and `/api/hatch/vm/wake` is followed by a verification read; only `RUNNING` counts as a successful wake.
8. Only explicit HTTP 401/login-screen evidence marks an account unhealthy. 403/429/5xx/network failures preserve the prior confirmed state.

The browser automation is intentionally selector-driven because muse.ai has no public generation API. If the muse.ai UI changes, the selectors in `internal/engine/js.go` may need updating. The admin update panel refuses tracked local modifications and stops on failed source installation. Repository pushes refuse unrelated staged files. The panel can pull source changes from a configured repository; set `MUSE2API_REBUILD_CMD` to rebuild automatically.

---

## Security

- Account cookies are stored locally in `data/accounts.json` with mode `0600`. Nothing is sent to third-party services except muse.ai and the optional GitHub update checks.
- `GET /v1/media/*` is intentionally public so generated URLs work in clients. Use a reverse proxy or a private deployment if media must be protected.
- Reference images deny server-local paths and private/loopback URLs by default. Public fetches resolve DNS once, reject any non-global address, revalidate every redirect and connect to the checked IP while preserving the original Host/SNI.
- Frontend labels, notes and errors are inserted with `textContent`; the admin page does not render backend strings as HTML.
- The update/push endpoints run git commands deliberately. Keep the API key private and only configure `MUSE2API_REPO` when you trust the target repository.

---

## Testing

```bash
go test ./...
go vet ./...
```

The test suite covers configuration, expiry estimation, quota parsing, prompt construction, reference-image policy, async task admission/idempotency, keepalive scheduling, media deletion, extension packaging and admin base URLs. Browser-dependent behavior uses a real Chromium page and is skipped automatically if no browser is available.

---

## Project layout

```
cmd/muse2api/          CLI: serve, cookie-helper, mailtm-otp, verify-selectors
internal/config/       Environment and .env configuration
internal/store/        Atomic JSON account/task persistence
internal/cdp/          Minimal Chrome DevTools Protocol WebSocket client
internal/engine/       Browser lifecycle, DOM automation, chat, quota, references
internal/api/          HTTP routes, OpenAI compatibility, admin API, keepalive
internal/web/          Embedded admin.html, extension, version.json
deploy/                systemd and Nginx examples
tools/                 Tampermonkey userscript
```

---

## Disclaimer

This software is intended for technical study, automation research and testing of your own accounts. Respect muse.ai's terms of service and applicable law.
