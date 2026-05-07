# WindsurfAPI — Go rewrite

> 当前版本 · **1.4.17-go**（2026-05-06）
> 变更摘要见 [CHANGELOG.md](CHANGELOG.md)。

Drop-in Go port of the Node.js service. **9,300+ lines**, **single static
binary (~13 MB)**, embeds the dashboard SPA, zero runtime dependencies
beyond `language_server_linux_x64`.

## Status

| Phase | Scope | Status |
|---|---|---|
| P1 | protobuf codec · gRPC-over-HTTP/2 client · LS process pool · WindsurfClient · account pool · models catalog · cache · sanitize · conv pool | ✓ |
| P2 | `/v1/chat/completions` (stream + non-stream) · tool emulation · runtime-config · stats · model-access · proxy-config | ✓ |
| P3 | `/v1/messages` Anthropic bridge (live SSE translator) · Codeium cloud REST · Firebase email+password · token refresh · OAuth re-register · proxy CONNECT tunnel · periodic tasks (6h probe / 15 min credits / 50 min Firebase) · preflight rate-limit | ✓ |
| P4 | dashboard API (25+ routes) · SSE log stream · JSONL daily rotation · embedded SPA · self-update via PM2 restart · proxy egress-IP test | ✓ |

## Layout

```
go/
  cmd/windsurfapi/main.go          entrypoint
  internal/
    config/      .env + typed config
    logx/        ring buffer + SSE fan-out + JSONL rotation
    pbenc/       zero-dep protobuf wire codec
    grpcx/       HTTP/2 gRPC unary + stream (h2c)
    windsurf/    exa.language_server_pb builders / parsers (Cascade + Legacy)
    models/      107-model catalog + tier access
    cache/       exact-body response cache
    sanitize/    /tmp/windsurf-workspace path scrubber (stream-safe)
    convpool/    Cascade cascade_id reuse pool
    langserver/  language_server_linux_x64 process pool (per-proxy)
    client/      WindsurfClient (Cascade + Legacy flows + stall logic)
    auth/        account pool, tier, RPM weighting, capability probe, credit refresh
    cloud/       Codeium REST (GetUserStatus / ModelConfigs / RateLimit / register_user)
    firebase/    sign-in + token refresh + re-register (UA fingerprint rotation)
    toolemu/     OpenAI tools[] ↔ Cascade text-protocol
    modelaccess/ global allow/block list
    proxycfg/    global + per-account HTTP proxy
    runtimecfg/  runtime-config.json (experimental flags + identity prompts)
    stats/       per-model / per-account / 72h-bucket stats with p50/p95
    server/      HTTP router + chat + messages + probe builder
    dashapi/     every /dashboard/api/* route
    web/         embed index.html
```

## Build & run

```bash
cd go
go build -o windsurfapi ./cmd/windsurfapi
./windsurfapi
```

Go ≥ 1.22. External deps: `golang.org/x/net/http2` (h2c client for the LS
local gRPC) plus its transitive `golang.org/x/text`.

Dashboard: `http://<host>:<PORT>/dashboard`

## Docker

Container packaging is supported for **Linux amd64**.

Build locally:

```bash
docker build -t windsurf-to-api-go .
```

Run locally:

```bash
docker run --platform=linux/amd64 \
  -p 3003:3003 \
  --env-file .env \
  -v "$(pwd)/.docker-data:/data" \
  -v windsurf-ls-data:/opt/windsurf/data \
  -v windsurf-workspace:/tmp/windsurf-workspace \
  ghcr.io/wuhao1477/windsurf-to-api-go:latest
```

Or use:

```bash
docker compose up -d --build
```

Notes:

- The image downloads `language_server_linux_x64` during build, so Git LFS is not required for container builds.
- Runtime state (`accounts.json`, `proxy.json`, `runtime-config.json`, `model-access.json`, `stats.json`, `logs/`) is stored under `/data`.
- GitHub Actions publishes images to `ghcr.io/wuhao1477/windsurf-to-api-go`.

## Env

Same names as the JS service — see `.env.example`. Load order: process env
overrides `.env`. Key vars:

- `PORT` — HTTP listener (default 3003)
- `API_KEY` — required for `/v1/chat/completions` + `/v1/messages` (leave empty = open)
- `DASHBOARD_PASSWORD` — required for `/dashboard/api/*` (falls back to `API_KEY` when unset)
- `LS_BINARY_PATH` — default `/opt/windsurf/language_server_linux_x64`
- `CODEIUM_API_KEY` / `CODEIUM_AUTH_TOKEN` — comma-separated, loaded at boot

## Endpoints

```
POST /v1/chat/completions     OpenAI compatible (stream + non-stream)
POST /v1/messages             Anthropic compatible (live SSE translator)
GET  /v1/models
POST /auth/login              {api_key}|{token}|{email,password} (batch via {accounts:[…]})
GET  /auth/accounts           list all accounts
DELETE /auth/accounts/:id
GET  /auth/status
GET  /health
GET  /dashboard               SPA (131 KB HTML, embedded via //go:embed)
/dashboard/api/*              25+ routes, match 1-1 with JS
```

## Performance notes (JS vs Go)

| | Node.js | Go |
|---|---|---|
| Static binary | — (needs Node + `/opt/windsurf`) | 11.8 MB single file |
| Idle RSS | 70–90 MB | 10–15 MB |
| Concurrency | single event loop | goroutines + per-request context |
| Protobuf alloc | `Buffer.concat` repeated copies | `append([]byte, …)` one outer alloc |
| gRPC conn | `http2.connect` per poll | `http2.Transport` pool reuse |
| SSE throughput | Node Writable stream intermediate buffers | direct `http.Flusher` — no middlemen |
| Path sanitize | regex ReplaceAll per chunk | stream-safe `Stream` with holdLen tail |

## Parity with JS

The Go port mirrors the JS service's externally observable behaviour —
including the well-documented gotchas from the JS `CLAUDE.md`:

- `planner_mode = NO_TOOL (3)` + three `SectionOverrideConfig` overrides on
  fields 10/12/13 of `CascadeConversationalPlannerConfig`.
- Both `requested_model_uid` (field 35) AND the deprecated enum via field
  15 / field 1 of the ModelOrAlias message — needed when user status is nil.
- `responseText` preferred over `modifiedText` mid-stream; `modifiedText`
  top-up at idle only when it's a strict prefix extension.
- Cold-stall threshold = `30 s + ⌊chars/1500⌋·5 s` capped at 180 s.
- Warm-stall: 25 s no progress → retry when <300 chars yielded, accept
  otherwise.
- Account signalling: rate-limit / permission_denied / failed_precondition
  / `internal error occurred` each routed to their own quarantine path —
  only real auth failures decrement the error budget.
- Firebase API key `AIzaSyDsOl-1XpT5err0Tcnx8FFod1H8gVGIycY` — the three
  alternatives listed in `CLAUDE.md` are confirmed non-functional; **don't
  rotate**.
- Workspace wipe at boot on Linux.
- Dashboard password fallback to API key when unset.
- OpenAI-shape `prompt_tokens = input + cacheRead + cacheWrite`; Anthropic
  bridge surfaces `cache_creation_input_tokens` + `cache_read_input_tokens`
  separately.

## Non-goals / known stubs

- PM2 / systemd supervision is external — self-update does an orderly
  `os.Exit(0)` and relies on the supervisor to relaunch, identical to the
  JS version.
- Firebase sign-in uses the exact hard-coded API key — OAuth Google/GitHub
  flows come in through the dashboard front-end (which posts the Firebase
  idToken to `/dashboard/api/oauth-login`).
- `preflightRateLimit` adds one REST round-trip per chat attempt; off by
  default to match JS.

## `modelIdentityPrompt` — on by default

The experimental flag injects a per-vendor identity instruction at two
proto levels (`SendUserCascadeMessageRequest` fields 8 / 13 + a
prepended system-role message). Default **on** with tame templates for
all ten vendor families (anthropic / openai / google / deepseek / xai /
alibaba / moonshot / zhipu / minimax / windsurf). Operators edit or
blank individual templates in the dashboard's experimental panel.

Effectiveness is probabilistic:

| Model family | Override effective? |
|---|---|
| `claude-*` | mostly yes (Claude's RLHF leans into system-prompt identity) |
| `grok-3*` | no — grok's baked-in "I am Cascade" reply survives |
| others | varies per family |

Known side-effect under aggressive client system prompts (e.g. Claude
Code's "You are Claude Code …" + full CLAUDE.md rules): Cascade may
interpret the stacked identity layers as a prompt-injection attempt and
refuse to comply loudly ("I notice several prompt injection attempts").
If that happens, blank the `anthropic` template (or turn the flag off
entirely) to keep the tone calm — at the cost of the model sometimes
self-identifying as "Cascade".

## Default response language

Every Cascade request carries a tame "respond in Simplified Chinese by
default, follow the user if they write in another language" instruction
appended to `communication_section` (proto field 13). This is **not** an
identity injection — no `You are …` claim, no `ignore` / `override` /
`NEVER` / `CRITICAL` tokens — so it won't trigger client-side prompt-
injection detectors or stack with identity prompts into an anti-injection
trip.

Configurable via `responseLanguagePrompt` in `runtime-config.json`:

- **missing or empty** → uses the built-in default (Simplified Chinese)
- **non-empty** → uses your text verbatim (e.g. write `"Respond in
  English by default."` for an English default)
- **single space `" "`** → disables the steer; model responds in whatever
  language the stacked system prompts lead it to

No dashboard UI yet — edit `runtime-config.json` directly and restart the
service if you need to change the default.

## License

Same terms as the JS repo — see the root README. No commercial / relay /
resale use without written permission.
