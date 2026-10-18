# AllPlatforms — Unified Multi-Platform AI Gateway

<!-- Name:Shaw -->
<!-- Email: 2536990321@qq.com -->

A single-binary Go gateway that exposes multiple AI platforms (WorkBuddy /
CodeBuddy, Trae SOLO) behind one OpenAI-compatible API, on one port, with a
built-in web management panel.

This project is a **merge** of two independent upstream codebases into one
process. Instead of running two separate executables on two ports, it runs
**one** binary on **one** port and routes by URL path prefix.

---

## Table of Contents

- [Design Goals](#design-goals)
- [Architecture](#architecture)
- [Source Provenance](#source-provenance)
- [Directory Layout](#directory-layout)
- [HTTP API](#http-api)
- [Configuration](#configuration)
- [Building and Running](#building-and-running)
- [Platform Abstraction Layer](#platform-abstraction-layer)
- [Notable Implementation Details](#notable-implementation-details)
- [Third-Party Dependencies](#third-party-dependencies)
- [Security Notes](#security-notes)
- [License](#license)

---

## Design Goals

The merge was driven by four explicit requirements:

1. **One backend, one executable.** Merge `wbapi2` (WorkBuddy) and `traeapi2`
   (Trae) into a single process instead of two.
2. **One port.** Downstream consumers (DSH) connect to port `7863` only.
3. **Path-based platform separation.** Each platform is reachable under its own
   prefix so downstream can distinguish them:
   - `/workbuddy/v1` — WorkBuddy / CodeBuddy
   - `/trae/v1` — Trae SOLO
4. **Extensibility.** Adding another platform should not require touching
   routing, panel, or dashboard code. This is achieved through a platform
   abstraction layer (see [Platform Abstraction Layer](#platform-abstraction-layer)).

Backwards compatibility is preserved: the root path `/v1` still aliases
WorkBuddy, so pre-existing downstream configuration keeps working unchanged.

---

## Architecture

```
                    ┌──────────────────────────────────────┐
   downstream ─────▶│  root mux (single :7863 listener)    │
   (DSH, OpenAI     │                                      │
    SDK, curl)       │  /workbuddy/*  ──▶ WorkBuddy platform│
                    │  /trae/*       ──▶ Trae platform     │
                    │  /authorize    ──▶ Trae OAuth callback
                    │  /v1/*         ──▶ WorkBuddy (alias) │
                    │  /panel/*      ──▶ Web panel         │
                    └──────────────────────────────────────┘
                                     │
                     ┌───────────────┴───────────────┐
                     ▼                               ▼
              WorkBuddy upstream              Trae upstream
         (copilot.tencent.com,              (trae-api-cn.mchost.guru,
          www.codebuddy.cn,                  api.trae.cn,
          www.workbuddy.cn)                  api.trae.com.cn)
```

Each platform owns its own:

- account pool (with cooldown / breaker / rotation)
- upstream HTTP client
- scheduler (check-in, keep-alive, balance refresh)
- credential store

Cross-platform concerns (panel, unified dashboard, request logging) are driven
by a **registry** rather than by hard-coded platform references.

---

## Source Provenance

This project merges two upstream codebases. The original sources are **not**
vendored or copied wholesale — the platform implementations were relocated
under `internal/platform/<kind>/` and adapted to the abstraction layer.

### Upstream repositories (original author's local checkouts)

| Upstream project | Local path | Contributed |
|---|---|---|
| `traeapi2` | `F:\project\workbuddyapi2\traeapi2` | Trae SOLO platform: auth, pool, scheduler, upstream client, OAuth login, vision probe, OpenAI-compatible surface |
| `workbuddy2api-panel-main` | `F:\project\workbuddyapi2\workbuddy2api-panel-main` | WorkBuddy/CodeBuddy platform: auth, pool, scheduler, upstream client, web panel, usage accounting, request log, redis store |

### Relocated in this repository

| Original location | New location | Notes |
|---|---|---|
| `traeapi2/internal/*` | `internal/platform/trae/*` | package path rewritten to `allplatforms/internal/platform/trae/...` |
| `traeapi2/internal/server/*` | `internal/platform/trae/server/*` | management endpoints made platform-relative (`/api/...` instead of `/trae/api/...`); prefixing is done by the router |
| `workbuddy2api-panel-main/internal/*` | `internal/platform/workbuddy/*` | package path rewritten to `allplatforms/internal/platform/workbuddy/...` |
| `workbuddy2api-panel-main/cmd/*` | `cmd/*` | split into `server/`, `login/` |
| `workbuddy2api-panel-main/internal/panel/*` | `internal/panel/*` | made registry-driven (no per-platform hard-coding) |

### Newly written for this merge

| Path | Purpose |
|---|---|
| `internal/platform/platform.go` | `Platform` interface, `Registry`, shared account/action types |
| `internal/platform/capabilities.go` | Optional capability interfaces and runtime probing |
| `internal/platform/probeimage/probeimage.go` | Deterministic probe-image generator (migrated from `scripts/probe_vision.py`) |
| `internal/platform/vision/visionprobe.go` | In-process vision-capability probe (migrated from `scripts/probe_vision.py`) |
| `internal/platform/trae/tcdecrypt/tcdecrypt.go` | Go re-implementation of Trae's `tc` credential decryption |
| `cmd/server/trae_assembly.go` | Trae platform assembly and path constants |
| `cmd/server/rootalias.go` | Root-path backwards-compatibility aliases |

### External reference implementations

| Project | Used for | Reference |
|---|---|---|
| `laojichao/trae-local-api` | Algorithm for decrypting Trae's `tc`-encrypted credential store (`src/trae-decrypt.js`). The Go port in `tcdecrypt` is a faithful re-implementation; the hard-coded salt arrays originate from Trae's own front-end JavaScript. | https://github.com/laojichao/trae-local-api |

### Behavioural references consulted

| Project | Consulted for |
|---|---|
| `connectedGraph/trae2api-web` | multi-account pooling layout (derivative lineage of some upstream files) |
| `240xu/trae2api-more` | check-in `9074` self-healing notes (device-id rotation + backoff) |
| `aimod-cc/agent2api` | general design of reusing desktop-client login state |

### Protocol references extracted from the vendor client

Trae's login protocol was reverse-engineered from the locally installed
**TRAE SOLO CN** desktop client. All facts below come from that client's own
files, not from guesswork:

| Fact | Source |
|---|---|
| Login URL parameter set (including `auth_from`, `hide_saas_login`, `plugin_version`) | `%APPDATA%\TRAE SOLO CN\resources\app\out\main.js` and the client's runtime log `logs/<timestamp>/main.log` |
| PKCE algorithm (`code_verifier` = 48 random bytes base64url; `code_challenge` = SHA-256 of verifier, base64url) | `main.js` (`IPe` function) |
| OAuth callback shape (`authCodeInfo` / `loginTraceID` / `userInfo`) | runtime log |
| Authorization-code exchange request (endpoint + body) | runtime log |

---

## Directory Layout

```
all_platforms/
├── cmd/
│   ├── server/                  # gateway entry point
│   │   ├── main.go              # config load, registry assembly, HTTP server
│   │   ├── config.go            # configuration model + defaults
│   │   ├── paths.go             # path constants and helpers
│   │   ├── rootalias.go         # root-path compatibility aliases
│   │   ├── trae_assembly.go     # Trae platform wiring
│   │   └── callback_path_test.go
│   └── login/                   # standalone interactive login helper
├── internal/
│   ├── panel/                   # web management panel (registry-driven)
│   │   ├── panel.go             # HTTP handlers
│   │   ├── platforms.go         # per-platform panel routes
│   │   ├── alldashboard.go      # cross-platform dashboard
│   │   ├── index.html           # single-page UI
│   │   └── app.js               # front-end logic
│   ├── platform/
│   │   ├── platform.go          # Platform interface + Registry
│   │   ├── capabilities.go      # optional capability interfaces
│   │   ├── probeimage/          # deterministic probe image generator
│   │   ├── vision/              # vision-capability probing
│   │   ├── workbuddy/           # WorkBuddy/CodeBuddy platform
│   │   │   ├── auth/  pool/  scheduler/  server/
│   │   │   ├── upstream/  session/  usage/  redisstore/
│   │   │   ├── httpauth/  logfmt/  prompt/  livecfg/
│   │   │   └── workbuddy.go     # adapter to the Platform interface
│   │   └── trae/                # Trae SOLO platform
│   │       ├── auth/  pool/  scheduler/  server/
│   │       ├── upstream/  config/  tcdecrypt/
│   │       └── trae.go          # adapter to the Platform interface
│   └── ...
├── scripts/                     # auxiliary Python probes (legacy, superseded by internal/platform/vision)
├── data/                        # runtime state (git-ignored)
├── auths/                       # WorkBuddy credentials (git-ignored)
├── traeauths/                   # Trae credentials (git-ignored)
├── config.example.json          # configuration template (safe to commit)
├── go.mod / go.sum
├── Dockerfile / docker-compose.yml
└── README.md
```

---

## HTTP API

All paths are served on the **same port** (`7863` by default).

### Platform-scoped endpoints

| Path | Platform | Purpose |
|---|---|---|
| `POST /workbuddy/v1/chat/completions` | WorkBuddy | OpenAI-compatible chat |
| `GET  /workbuddy/v1/models` | WorkBuddy | model list |
| `POST /trae/v1/chat/completions` | Trae | OpenAI-compatible chat |
| `GET  /trae/v1/models` | Trae | model list |
| `GET  /workbuddy/healthz`, `/trae/healthz` | both | liveness |

### Backwards-compatible root alias

| Path | Resolves to |
|---|---|
| `POST /v1/chat/completions` | WorkBuddy (deprecated alias, still functional) |
| `GET  /v1/models` | WorkBuddy |
| `GET  /status` | WorkBuddy status |

### OAuth callback

| Path | Purpose |
|---|---|
| `GET /authorize` | Trae OAuth callback landing page |

> **Important:** this callback **must** live at the root path. The upstream
> rejects any callback URL carrying a platform prefix. See
> [Notable Implementation Details](#notable-implementation-details).

### Management panel

| Path | Purpose |
|---|---|
| `GET /panel/` | web UI |
| `GET /panel/api/platforms` | registered platforms and their capabilities |
| `GET /panel/api/platforms/{kind}/accounts` | account list with status |
| `GET /panel/api/platforms/{kind}/overview` | per-platform overview |
| `POST /panel/api/platforms/{kind}/actions/{action}` | platform-wide action (`checkin` / `keepalive` / `balance`) |
| `POST /panel/api/platforms/{kind}/accounts/{uid}/{action}` | per-account action |
| `POST /panel/api/platforms/{kind}/login/start` | begin browser login |
| `POST /panel/api/platforms/{kind}/import` | import a pasted credential |
| `GET  /panel/api/platforms/{kind}/client/probe` | detect a locally installed vendor client |
| `POST /panel/api/platforms/{kind}/client/import` | import credentials from the local vendor client |

---

## Configuration

Configuration is a single JSON file (`config.json`). Start from
`config.example.json` — it contains **no** secrets.

Key fields:

| Field | Meaning |
|---|---|
| `listen` | bind address, e.g. `:7863` |
| `api_key` | bearer token required by the gateway and panel |
| `auth_dir` | WorkBuddy credential directory |
| `state_file` | WorkBuddy runtime state file |
| `schedule.*` | scheduler hours and toggles |
| `pool.*` | rotation, cooldown, breaker, and refresh-window tuning |
| `trae.*` | Trae platform block (own auth dir, state file, pool, schedule, OAuth) |

> `config.json` itself is **git-ignored** because it carries the real
> `api_key`. Commit only `config.example.json`.

---

## Building and Running

### Requirements

- Go **1.22.5** or newer

### Build

```bash
go build ./...
```

### Produce a release binary

```bash
go build -trimpath -ldflags "-s -w" -o dist/allplatforms.exe ./cmd/server
```

### Run

```bash
./allplatforms.exe -config config.json
```

The gateway prints the listening address on startup, e.g.:

```
workbuddy2api listening on :7863 (api_key=true), panel http://127.0.0.1:7863/panel/
```

### Test

```bash
go vet ./...
go test ./...
```

### Docker

```bash
docker compose up -d
```

---

## Platform Abstraction Layer

Every platform implements a small core interface:

```go
type Platform interface {
    Kind() Kind
    DisplayName() string
    ModelsPath() string
    Register(mux *http.ServeMux)
    Accounts() []Account
    Trigger(action Action, uid string) ActionResult
    Close() error
}
```

Optional capabilities are discovered by **type assertion** rather than being
declared on the core interface, so a platform only implements what it actually
supports:

| Capability interface | Enables |
|---|---|
| `StatusProvider` | per-platform status summary |
| `AccountActioner` | per-account actions |
| `LoginProvider` | browser-based login flow |
| `Importer` | import a pasted credential |
| `ClientImporter` | import from a locally installed vendor client |
| `RawModelLister` | expose raw upstream model catalogue |
| `ServeMuxProvider` | mount extra routes |

`ProbeCapabilities` reports the result at startup; the panel uses it to decide
which controls to render, so users never click a button that returns `501`.

Platforms are held in an ordered `Registry`, which drives:

- route mounting (`RegisterAll`)
- panel navigation (`Infos`)
- the cross-platform dashboard
- platform-wide actions (`TriggerAll`)

**Adding a platform** therefore requires: implement the interface, register it
in `cmd/server/main.go`. Routing, panel navigation, and dashboard pick it up
automatically.

---

## Notable Implementation Details

These are behaviours that were established empirically and are easy to break
during refactoring. Each is pinned by a regression test.

### Trae OAuth callback must use the root path

The upstream **rejects** a callback URL whose path carries a platform prefix.
Measured with a controlled experiment (same browser, same moment, only the path
changed):

| `auth_callback_url` | Result |
|---|---|
| `http://127.0.0.1:<port>/authorize` | login page opens normally |
| `http://127.0.0.1:<port>/trae/authorize` | **"login failed / network error"** |

Port and URL-encoding were both ruled out as variables. The failure is
particularly deceptive: the upstream still returns HTTP 200, and the page
issues **no** failing network request — the error page is rendered by upstream
front-end JavaScript.

The gateway therefore serves the callback at the root path and forwards it
internally to the platform-prefixed route.

### Trae login URL parameters

The parameter set was extracted from the vendor client's own runtime log:

```
auth_from=solo            (only "solo" / "trae" are valid — there is no "ide")
hide_saas_login=true      (mandatory when auth_from=solo)
plugin_version=2.3.91089  (current client build; the old hard-coded value was stale)
code_challenge=<PKCE>     (PKCE, RFC 7636)
code_challenge_method=S256
```

Any one of these being wrong produces the same deceptive failure described
above, which is why they are documented and test-pinned.

### Trae credential decryption (`tc` format)

Trae stores credentials encrypted in `storage.json` under
`iCubeAuthInfo://icube.cloudide`. The layout is:

```
[6-byte header][32-byte random][AES-128-CBC ciphertext]
```

and the decrypted payload is:

```
[64-byte SHA-512 digest][plaintext JSON]
```

Key derivation:

```
salt  = SALT_A XOR SALT_B        (64 hard-coded bytes from Trae's front-end)
h1    = SHA-512(random)
h2    = SHA-512(h1 || salt)
key   = h2[0:16]      iv = h2[16:32]
```

The trailing SHA-512 digest lets decryption **self-verify**: a wrong key is
detected rather than silently producing garbage.

> **Pitfall worth recording:** Node's `crypto.createDecipheriv` strips PKCS#7
> padding automatically, but Go's `cipher.CBCDecrypter.CryptBlocks` does
> **not**. Omitting the unpad step makes the digest check fail, which looks
> like a wrong key and sends you chasing the key derivation. `tcdecrypt`
> un-pads explicitly.

### Account refresh and failure classification (WorkBuddy)

Access tokens live for roughly 30 days. The pool must decide **when** to
refresh, and how to react when the upstream rejects a token:

- `RefreshSkew` is a **lead time**, not an interval: the token is refreshed
  once its remaining lifetime drops below the skew. A very small skew means
  refresh effectively never fires; a large one refreshes earlier and more often.
- A `401` has two very different causes: the access token may be invalidated
  while the refresh token is still valid (recoverable by refreshing), or both
  may be dead (`12153`, requiring re-login). The gateway therefore attempts a
  refresh on `401` before declaring the account dead, and only disables it if
  the refresh itself fails with `12153`.
- Any `401`-triggered retry must clear the per-request "already tried" marker,
  otherwise the retry cannot re-select the same account and the request fails
  with `no_healthy_account` even though the refresh succeeded.

### Credit/usage write-back (Trae)

Usage figures are written back on **every** check-in branch, not only on
success. An account that has already checked in today would otherwise have no
recorded credits, and the panel would show `0` until the periodic background
refresh ran. The first check-in after import is performed **synchronously** so
the import response already carries the credit balance.

### Cooldown residue (Trae)

A cooldown sets both an expiry (`until`) and a reason string. Once the expiry
passes the account is eligible again, but the reason string historically was
never cleared — and the loader restored it verbatim, so restarts did not help
either. The panel therefore kept showing a failure reason for a perfectly
healthy account. Successes now clear the reason, and the loader drops residue
whose expiry has already passed.

---

## Third-Party Dependencies

All runtime dependencies are Go modules (see `go.mod`):

| Module | Role |
|---|---|
| `github.com/redis/go-redis/v9` | optional Redis-backed state mirror |
| `github.com/cespare/xxhash/v2` | hashing (transitive) |
| `github.com/dgryski/go-rendezvous` | consistent hashing (transitive) |
| `go.uber.org/atomic` | atomics (transitive) |
| `golang.org/x/sys` | OS primitives (transitive) |

There are **no** CGO dependencies and **no** vendored third-party source. The
`tcdecrypt` package uses only the Go standard library (`crypto/sha512`,
`crypto/aes`, `encoding/base64`), so no Node.js or Python runtime is required.

---

## Security Notes

This repository is configured to keep credentials out of version control.

**Never commit:**

- credential files (`auths/`, `traeauths/`, `*.json` containing tokens)
- runtime state (`data/`) — it contains account identifiers and usage history
- `config.json` — it contains the live `api_key`
- release packages (`release*/`, `dist/`) — they bundle credentials
- logs and PID files

**Safe to commit:**

- `config.example.json` (template, no secrets)
- all Go source and tests
- documentation

The gateway's own `api_key` guards both the OpenAI-compatible surface and the
management panel. Treat it as a secret: rotate it if it is ever exposed, and
never place it in a URL, a screenshot, or a commit.

Credential files are stored as plaintext JSON on disk. Protect the host
accordingly and restrict filesystem permissions on `auths/` and `traeauths/`.

---

## License

See [LICENSE](LICENSE).
