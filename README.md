# gg-search-proxy

Self-hosted web search API backed by **Google Antigravity** (free Gemini + Google Search grounding), exposing a **9router-compatible `/v1/search` endpoint** with proper API keys for downstream consumers.

```
downstream (any HTTP client) ──Bearer gsk-…──▶ gg-search-proxy ──OAuth──▶ Google Antigravity backend
```

- **POST /v1/search** — 9router-compatible request/response shape (drop-in replacement)
- Multi-account pool with LRU scheduling + failover + auto token refresh
- API key management (create/list/revoke, sha256-hashed, usage tracking)
- SQLite storage, single static Go binary, zero CGO — runs on Windows / macOS / Linux

## Run a prebuilt binary

No Go toolchain needed — grab the right binary from `dist/`:

| File | Platform |
|---|---|
| `ggsearch-windows-amd64.exe` | Windows x64 |
| `ggsearch-linux-amd64` | Linux x64 (servers, most VPS) |
| `ggsearch-darwin-arm64` | macOS Apple Silicon (M1–M4) |
| `ggsearch-darwin-amd64` | macOS Intel |

**Linux / macOS:** make it executable once, then run like any command:

```bash
chmod +x ggsearch-linux-amd64
./ggsearch-linux-amd64 serve
```

macOS Gatekeeper may block an unsigned binary on first run — allow it in
*System Settings → Privacy & Security* (“Open Anyway”), or clear the quarantine flag:

```bash
xattr -d com.apple.quarantine ggsearch-darwin-arm64
```

The binary reads `.env` and `data/` from its **working directory** — keep the
`.env` file and the `data/` folder next to where you launch it.

## Quick start

### Windows

```powershell
go build -o ggsearch.exe .\cmd\ggsearch

.\ggsearch.exe import-9router     # import your Antigravity account from 9router (path auto-detected)
.\ggsearch.exe login              # ...or connect a Google account via OAuth (opens browser)
.\ggsearch.exe keys create my-app # create an API key — printed once, save it
.\ggsearch.exe serve              # start the API on :20129
```

### macOS / Linux

```bash
go build -o ggsearch ./cmd/ggsearch

./ggsearch import-9router         # auto-detects ~/Library/Application Support/9router/db/data.sqlite (macOS)
                                   # or ~/.config/9router/db/data.sqlite (Linux)
./ggsearch login                  # ...or OAuth login with a browser
./ggsearch keys create my-app     # create an API key
./ggsearch serve                  # start the API on :20129
```

Optional: install the binary anywhere on `PATH`, or cross-compile without a target toolchain:

```bash
GOOS=linux   GOARCH=amd64 go build -o ggsearch-linux-amd64 ./cmd/ggsearch
GOOS=darwin  GOARCH=arm64 go build -o ggsearch-darwin-arm64 ./cmd/ggsearch   # Apple Silicon
GOOS=darwin  GOARCH=amd64 go build -o ggsearch-darwin-amd64 ./cmd/ggsearch   # Intel Mac
GOOS=windows GOARCH=amd64 go build -o ggsearch.exe ./cmd/ggsearch
```

## Configuration

Copy `.env.example` to `.env` and edit (real environment variables always win over `.env`):

```bash
cp .env.example .env
```

| Var | Default | Purpose |
|---|---|---|
| `GGSP_ADDR` | `:20129` | listen address |
| `GGSP_DB` | `data/ggsearch.db` | SQLite path |
| `GGSP_ADMIN_TOKEN` | — | enables admin endpoints (keys, accounts, usage) |
| `GGSP_DEFAULT_MODEL` | `gemini-2.5-flash` | grounding model |
| `GGSP_UPSTREAM_TIMEOUT` | `20s` | upstream HTTP timeout |

## API

### `POST /v1/search` (requires `Authorization: Bearer gsk-...`)

```bash
curl -X POST http://localhost:20129/v1/search \
  -H "Authorization: Bearer gsk-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"antigravity","query":"latest LLM benchmarks","max_results":5}'
```

Request fields (9router-compatible):

| Field | Required | Notes |
|---|---|---|
| `query` | yes | search query |
| `model` / `provider` | no | accepts `antigravity`/`ag` or a Gemini model id; defaults to `GGSP_DEFAULT_MODEL` |
| `max_results` | no | default 10 |

Response shape (same as 9router):

```json
{
  "provider": "antigravity",
  "query": "...",
  "results": [
    {
      "title": "...", "url": "https://...", "snippet": "...", "position": 1,
      "score": null, "published_at": null, "favicon_url": null,
      "content": "surrounding grounded context",
      "metadata": {},
      "citation": {"provider": "antigravity", "retrieved_at": "...", "rank": 1}
    }
  ],
  "answer": {"source": "antigravity", "text": "grounded answer text", "model": "gemini-2.5-flash"},
  "usage": {"queries_used": 1, "search_cost_usd": 0, "llm_tokens": 1234},
  "metrics": {"response_time_ms": 850, "upstream_latency_ms": 700, "total_results_available": 8},
  "errors": []
}
```

### Admin endpoints (require `Authorization: Bearer $GGSP_ADMIN_TOKEN`)

| Method/Path | Purpose |
|---|---|
| `GET /health` | liveness (no auth) |
| `GET /v1/keys` | list keys + usage_24h / usage_30d |
| `POST /v1/keys` `{"label":"..."}` | create key — full key returned once |
| `DELETE /v1/keys/{id}` | revoke |
| `GET /v1/accounts` | account pool status |
| `POST /v1/accounts/{id}/enable` / `disable` | toggle scheduling |
| `DELETE /v1/accounts/{id}` | remove |
| `GET /v1/usage?window=24h` | per-key request counts (window: `1h`, `24h`, `7d`...) |

## CLI reference

```
ggsearch serve  [-addr :20129] [-db data/ggsearch.db]   start the API server
ggsearch login                                          add a Google account via OAuth
ggsearch import-9router [path/to/9router.db]            import Antigravity accounts from 9router
ggsearch accounts                                       list connected accounts
ggsearch keys create <label>                            create an API key
ggsearch keys list                                      list API keys
```

## How it works (upstream)

Uses the public Antigravity IDE OAuth client (the same one 9router ships) → Google OAuth2 code flow → `loadCodeAssist` to resolve the `cloudaicompanionProject` → `POST https://daily-cloudcode-pa.googleapis.com/v1internal:generateContent` with `requestType: "search"` and the `googleSearch` grounding tool, sent as the IDE (`User-Agent: antigravity/ide/2.11.0 darwin/arm64`). Grounding chunks/supports are deduped by URL into citations with snippets and expanded context windows, mirroring 9router's normalization.

**Quota/cost:** searches are free through the Antigravity account tier; `usage.llm_tokens` reports the Gemini token count per search.

## Caveats

- **Unofficial API** — Google can change the Antigravity backend at any time.
- Grounding URLs returned by upstream are `vertexaisearch.cloud.google.com/grounding-api-redirect/...` redirects, not canonical source URLs (9router has the same behavior).
- `.env` and `data/` are git-ignored; the SQLite DB contains refresh tokens — treat it as a secret.
- macOS/Linux OAuth login opens the browser via `open` / `xdg-open`; the callback server binds `127.0.0.1:<random>`.
