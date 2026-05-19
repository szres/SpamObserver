# AGENTS.md

This file provides guidance for AI coding agents working on SpamObserver.

## Build & Verify Commands

| Action | Command |
|---|---|
| Build | `go build ./...` |
| Vet | `go vet ./...` |
| Tidy deps | `go mod tidy` |
| Run locally | `BOT_TOKEN=xxx go run .` |
| Docker build | `docker compose build` |
| Docker run | `docker compose up -d` |

There are **no tests** yet. When adding tests, use `go test ./...`.

Run `go vet ./... && go build ./...` after every change to catch compile errors early.

## Critical Constraint: No Outbound Telegram Messages

This is a **passive observer**. The `internal/bot` package must **never** call any telego send/reply/edit/delete API. The `Monitor` struct does not hold a `*telego.Bot` reference — it only receives `telego.Update` values and publishes log entries.

If you add a feature that would require sending a message to Telegram, **do not implement it**. Log the observation instead.

## Architecture

```
main.go
  ├── Fiber HTTP server (single port)
  ├── Telego webhook registration (UpdatesViaWebhook → Fiber route)
  ├── startBot closure (mutex-protected bot lifecycle, reusable for restarts)
  ├── BotHandler (receives updates from channel, dispatches to Monitor)
  └── Signal handling (graceful shutdown)

internal/
  ├── db/db.go          — SQLite store, schema migration, admin CRUD, group CRUD
  ├── bot/bot.go        — Update processor: extracts events, runs pattern detection
  ├── logstream/logstream.go — Ring buffer (200 entries) + SSE broadcast broker
  ├── auth/auth.go      — HMAC-SHA256 session tokens, cookie auth, Fiber middleware
  ├── handler/handler.go — HTTP route handlers (login, groups, SSE, static)
  └── webui/
      ├── embed.go      — //go:embed directive
      └── index.html    — Single-page Alpine.js + Tailwind dark UI
```

### startBot lifecycle

`startBot` in `main.go` is a mutex-protected (`botMu`) closure that handles the full bot lifecycle. It is called both at startup and when the admin sets a new bot token via the UI. Each invocation: stops existing bot handler → creates new `telego.Bot` → creates `BotHandler` → registers dispatch → starts handler goroutine → sets webhook (if `PUBLIC_URL` is set). It is safe to call multiple times; it stops the previous bot before starting a new one.

### Bot token precedence

On startup, tokens are resolved in this order:
1. DB token (`store.GetBotToken()`) — preferred
2. `BOT_TOKEN` env var — fallback
3. No token — logs a warning, bot does not start

Setting a token via the Settings UI (`POST /api/config/bot/token`) saves to DB and triggers `restartBot(token)`, so the token persists across restarts.

The `bot_enabled` flag is loaded from DB at startup into an `atomic.Bool`. If the DB read fails, it defaults to `true`. `Monitor.ProcessUpdate` checks this at the top and short-circuits if disabled.

### Data flow

```
Telegram → Webhook POST → Fiber route → telego decode → updatesChan
  → BotHandler → Monitor.ProcessUpdate() → Broker.Publish()
  → SSE subscribers (browser) + ring buffer (history)
```

### Group monitoring is hot-reloaded

`Monitor.isMonitored()` calls `store.GetMonitoredIDs()` on every update — it queries SQLite each time. Adding/removing a group via the API takes effect immediately without restart.

## API Endpoints

| Route | Method | Auth | Purpose |
|---|---|---|---|
| `/` | GET | — | Serve SPA (embedded HTML) |
| `/api/auth/login` | POST | — | Authenticate, set cookie |
| `/api/auth/logout` | POST | — | Clear session cookie |
| `/api/auth/me` | GET | — | Check current session |
| `/api/logs/stream` | GET | — | SSE event stream |
| `/api/logs/recent` | GET | — | JSON array of recent entries |
| `/api/bot/status` | GET | — | `{enabled: bool}` |
| `/api/bot/info` | GET | — | `{has_token: bool, enabled: bool}` |
| `/api/config/groups` | GET | Cookie | List monitored groups |
| `/api/config/groups` | POST | Cookie | Add group `{chat_id}` |
| `/api/config/groups/:chatId` | DELETE | Cookie | Remove group |
| `/api/config/bot/toggle` | POST | Cookie | Enable/disable monitoring |
| `/api/config/bot/token` | GET | Cookie | Masked bot token info |
| `/api/config/bot/token` | POST | Cookie | Set bot token + restart bot |
| `/api/config/auth/change-password` | POST | Cookie | Change admin password |
| `/api/config/auth/change-username` | POST | Cookie | Change admin username |

Auth: Cookie routes check `spamo_session` cookie or `Authorization: Bearer <token>` header via `AuthMiddleware`.

### Webhook path

The webhook endpoint is the static constant `/api/webhook/tg` (`main.go:25`). It is NOT a URL-path-based token — the secret is validated via the `Telego-Webhook-Secret-Token` HTTP header. Allowed update types: `message`, `chat_member`, `my_chat_member`, `callback_query`.

### SSE protocol

Two event types:
- `event: history\ndata: <json-array>\n\n` — sent on connect (ring buffer dump)
- `event: log\ndata: <json-entry>\n\n` — each new entry in real-time

Keepalive comments (`: keepalive\n\n`) every 30 seconds. Frontend reconnects every 3s on disconnect.

## Session Tokens

The `JWTManager` in `internal/auth/auth.go` is **not** standard JWT — it's a custom HMAC-SHA256 token scheme.

Token format: `username|issuedAt|expiresAt.hexSignature`

- Cookie name: `spamo_session`, HTTP-only, SameSite=Lax
- Token expiry: 24 hours
- Refresh threshold: 12 hours
- Secret: `JWT_SECRET` env var, or auto-generated 32-byte random hex on startup

## Telego Type Gotchas

The telego v1.9.0 API has several non-obvious type decisions. Read carefully before editing `internal/bot/bot.go`:

### ChatMember is an interface, not a struct

```go
// CORRECT — use interface methods
status := update.NewChatMember.MemberStatus()
user := update.NewChatMember.MemberUser()

// WRONG — these don't exist
update.NewChatMember.Status    // ✗ no such field
update.NewChatMember.UserID()  // ✗ no such method
```

Concrete types: `*ChatMemberOwner`, `*ChatMemberAdministrator`, `*ChatMemberMember`, `*ChatMemberRestricted`, `*ChatMemberLeft`, `*ChatMemberBanned`. Type-switch or type-assert when you need struct fields:

```go
if r, ok := newMember.(*telego.ChatMemberRestricted); ok {
    // r.CanSendMessages is a bool, NOT *bool
}
```

### Message.Chat is a value, not a pointer

```go
// CORRECT
chatID := msg.Chat.ID

// WRONG — Chat is never nil
if msg.Chat == nil { ... }  // ✗ compile error
```

### Message.NewChatMembers is []User, not *[]User

```go
// CORRECT
for _, member := range msg.NewChatMembers { ... }

// WRONG
for _, member := range *msg.NewChatMembers { ... }  // ✗ compile error
```

### CallbackQuery.Message is MaybeInaccessibleMessage (interface)

```go
// CORRECT
chat := cq.Message.GetChat()
chatID := chat.ID

// WRONG
cq.Message.Chat  // ✗ MaybeInaccessibleMessage has no Chat field
```

### MessageEntity.URL is string, not *string

```go
// CORRECT
if entity.URL != "" { ... }

// WRONG
if entity.URL != nil { ... }  // ✗ compile error
```

### Message.Entities is []MessageEntity, not *[]MessageEntity

```go
// CORRECT
if len(msg.Entities) > 0 { ... }

// WRONG
if msg.Entities != nil { ... }  // works but prefer len() check
for _, e := range *msg.Entities { ... }  // ✗ compile error
```

## Fiber v3 API Notes

- `app.Add(methods []string, path, handler)` — methods is a string slice, not a single string
- `c.SendStreamWriter(func(w *bufio.Writer))` — for SSE streaming responses
- `c.Bind().JSON(&body)` — JSON body binding
- `c.Context()` returns `context.Context`, NOT `*fasthttp.RequestCtx`
- `c.Params("name")` — route parameter (always returns string)
- `c.Cookie(&fiber.Cookie{...})` — set response cookie

## SQLite Notes

- Uses `modernc.org/sqlite` (pure Go, no CGO)
- Single connection (`MaxOpenConns=1`) with `sync.RWMutex` wrapper
- WAL journal mode, busy_timeout=5000ms
- Schema auto-migrates on startup via `CREATE TABLE IF NOT EXISTS`
- Password hashing uses `golang.org/x/crypto/bcrypt`

## Frontend

- Zero-build: Alpine.js + Tailwind CDN loaded from `<script>` tags
- The HTML file is embedded at compile time via `//go:embed index.html`
- SSE reconnects automatically every 3 seconds on disconnect
- Logs are capped at 2000 in the browser (trimmed to 1000 when exceeded)
- Display slice is the last 200 entries
- Category filter toggle buttons for each event category
- Text search across messages/categories/usernames
- Auto-scroll toggle
- Bot status indicators: NO TOKEN / ACTIVE / PAUSED
- Settings modal: bot token management, password change, username change

## Adding a New Event Category

1. Add detection logic in `internal/bot/bot.go` (in `processMessage`, `analyzeContent`, or `analyzeEntities`)
2. Use an existing level (`INFO`, `WARN`, `ERROR`) and pick a new `CATEGORY` constant
3. Publish via `m.broker.Publish(logstream.Entry{...})`
4. Add the new category string to `availableCategories` array in `internal/webui/index.html` so the filter buttons appear

## Environment Variables

| Variable | Required | Default |
|---|---|---|
| `BOT_TOKEN` | No* | — |
| `PUBLIC_URL` | No* | — |
| `WEBHOOK_SECRET` | No | `spamo-whsec` |
| `PORT` | No | `8080` |
| `DB_PATH` | No | `./data/spam-observer.db` |
| `INITIAL_ADMIN_USERNAME` | No | `admin` |
| `INITIAL_ADMIN_PASSWORD` | No | `admin` |
| `JWT_SECRET` | No | (auto-generated) |

\* `BOT_TOKEN` can alternatively be set via the Settings UI and stored in SQLite. `PUBLIC_URL` is only needed for webhook registration — without it the bot starts but doesn't receive updates.

Admin credentials are only written to the database on first run. Changing env vars after first run has no effect on the admin password — use the `UpdateAdminPassword` method or reset the DB.
