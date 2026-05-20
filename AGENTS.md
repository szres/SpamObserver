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

## Outbound Telegram Messages — Conditional

SpamObserver is primarily a **passive observer**, but it does send limited outbound messages under specific conditions:

- **Spam warnings** (`sendSpamWarning`): When a new user is assessed as spam (by pattern or AI), a warning message is sent to the group via `b.SendMessage()` and auto-deleted after 120 seconds via `b.DeleteMessage()`.
- **Flood warnings** (`sendFloodWarning`): When a join flood is detected (3+ joins in 1 minute), a flood alert is sent to the group (also auto-deleted after 120s).

These are the **only** permitted outbound API calls. The Monitor must **never** reply to user messages, send direct messages, answer callback queries, or perform any other mutation. If you add a new feature that would require sending a Telegram message, verify it fits one of the above categories before implementing.

The `Monitor` struct holds a `*telego.Bot` reference via `atomic.Pointer[telego.Bot]`, set by `SetBot()` during bot startup. This pointer is used for `GetChat()` (bio fetching), `GetChatMember()` (mutual group counting), `SendMessage()`, and `DeleteMessage()`.

## Architecture

```
main.go
  ├── Fiber HTTP server (single port)
  ├── Telego webhook registration (UpdatesViaWebhook → Fiber route)
  ├── startBot closure (mutex-protected bot lifecycle, reusable for restarts)
  ├── BotHandler (receives updates from channel, dispatches to Monitor)
  ├── Log purge goroutine (deletes event_logs older than 7 days, every 1 hour)
  └── Signal handling (graceful shutdown)

internal/
  ├── ai/ai.go              — OpenAI-compatible LLM client for spam assessment (AssessUser, TestConnection)
  ├── auth/auth.go           — HMAC-SHA256 session tokens, cookie auth, Fiber middleware
  ├── bot/bot.go             — Update processor: event extraction, pattern detection, flood detection, AI assessment, in-group warnings
  ├── db/db.go               — SQLite store, schema migration, admin CRUD, group CRUD, log persistence, AI config, verification bots
  ├── handler/handler.go     — HTTP route handlers (login, groups, SSE, static, AI config, verification bots, stats)
  ├── logstream/logstream.go — Ring buffer (200 entries) + SSE broadcast broker
  ├── tracker/tracker.go     — New-user tracking with 24-hour TTL, backed by SQLite `new_users` table
  └── webui/
      ├── embed.go           — //go:embed directive
      └── index.html         — Single-page Alpine.js + Tailwind dark UI
```

### startBot lifecycle

`startBot` in `main.go` is a mutex-protected (`botMu`) closure that handles the full bot lifecycle. It is called both at startup and when the admin sets a new bot token via the UI. Each invocation: stops existing bot handler → creates new `telego.Bot` → calls `monitor.SetBot()` → creates `BotHandler` → registers dispatch → starts handler goroutine → sets webhook (if `PUBLIC_URL` is set). It is safe to call multiple times; it stops the previous bot before starting a new one.

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
  → SSE subscribers (browser) + ring buffer (history) + SQLite event_logs
```

### Group monitoring is hot-reloaded

`Monitor.isMonitored()` calls `store.GetMonitoredIDs()` on every update — it queries SQLite each time. Adding/removing a group via the API takes effect immediately without restart.

### New-user tracker

The `tracker.Tracker` maintains an in-memory cache of recently joined users (24-hour TTL), backed by the SQLite `new_users` table. When a new user joins, their display name, username, and bio are recorded. The tracker auto-purges expired entries every 10 minutes. Used to flag messages from new accounts (`NEW_MSG` category).

### Flood detection

`Monitor` tracks join timestamps per group. When 3+ joins occur within 1 minute, flood mode activates for that group. During flood mode, each new join is flagged as `FLOOD_ALERT` and AI assessment is skipped (users are marked as direct spam). Flood mode auto-deactivates after 1 minute of no joins (`FLOOD_END`). All timestamps are guarded by `floodMu` mutex.

### AI spam assessment

When `ai_base_url`, `ai_api_key`, and `ai_model` are configured in `admin_settings`, the monitor calls an OpenAI-compatible LLM API to assess new users. The prompt evaluates the user's name, username, and bio for spam indicators. Returns risk levels: low/medium/high/confirmed spam. Results are published as `AI_ASSESS` entries; high-risk or confirmed spam triggers `SPAM_HIGH_RISK` or `SPAM_CONFIRMED`. The AI client retries on HTTP 429 (rate limit) with exponential backoff.

### Log purging

A goroutine in `main.go` runs every hour and deletes `event_logs` rows older than 7 days via `store.PurgeOldLogs()`.

## API Endpoints

| Route | Method | Auth | Purpose |
|---|---|---|---|
| `/` | GET | — | Serve SPA (embedded HTML) |
| `/api/auth/login` | POST | — | Authenticate, set cookie |
| `/api/auth/logout` | POST | — | Clear session cookie |
| `/api/auth/me` | GET | — | Check current session |
| `/api/logs/stream` | GET | — | SSE event stream (auth users get 24h history, unauth get last 200) |
| `/api/logs/recent` | GET | — | JSON array of recent entries (optional `?hours=N`, default 24) |
| `/api/bot/status` | GET | — | `{enabled: bool}` |
| `/api/bot/info` | GET | — | `{has_token: bool, enabled: bool}` |
| `/api/bot/new-users-count` | GET | — | `{count: int}` new users in last 24h |
| `/api/bot/banned-count` | GET | — | `{count: int}` bans in last 24h |
| `/api/groups` | GET | — | Public group list (no auth required) |
| `/api/config/groups` | GET | Cookie | List monitored groups |
| `/api/config/groups` | POST | Cookie | Add group `{chat_id}` |
| `/api/config/groups/:chatId` | DELETE | Cookie | Remove group |
| `/api/config/bot/toggle` | POST | Cookie | Enable/disable monitoring |
| `/api/config/bot/warn-in-group` | GET | Cookie | Get warn-in-group state |
| `/api/config/bot/warn-in-group` | POST | Cookie | Set warn-in-group `{warn_in_group: bool}` |
| `/api/config/bot/token` | GET | Cookie | Masked bot token info |
| `/api/config/bot/token` | POST | Cookie | Set bot token + restart bot |
| `/api/config/auth/change-password` | POST | Cookie | Change admin password |
| `/api/config/auth/change-username` | POST | Cookie | Change admin username |
| `/api/config/verify-bots` | GET | Cookie | List verification bots |
| `/api/config/verify-bots` | POST | Cookie | Add verification bot `{bot_id, label}` |
| `/api/config/verify-bots/:botId` | DELETE | Cookie | Remove verification bot |
| `/api/config/new-users` | GET | Cookie | List new users (24h) |
| `/api/config/ai` | GET | Cookie | Get AI config (API key masked) |
| `/api/config/ai` | POST | Cookie | Set AI config `{base_url, api_key, model}` |
| `/api/config/ai/test` | POST | Cookie | Test AI connection |

Auth: Cookie routes check `spamo_session` cookie or `Authorization: Bearer <token>` header via `AuthMiddleware`.

### Webhook path

The webhook endpoint is the static constant `/api/webhook/tg` (`main.go:27`). It is NOT a URL-path-based token — the secret is validated via the `Telego-Webhook-Secret-Token` HTTP header. Allowed update types: `message`, `business_message`, `guest_message`, `chat_member`, `my_chat_member`, `callback_query`.

### SSE protocol

Two event types:
- `event: history\ndata: <json-array>\n\n` — sent on connect (ring buffer dump for unauthenticated; 24h DB history for authenticated)
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
- Schema auto-migrates on startup via `CREATE TABLE IF NOT EXISTS` + `ALTER TABLE ADD COLUMN` (ignored if column exists)
- Password hashing uses `golang.org/x/crypto/bcrypt`

### Database Tables

| Table | Purpose |
|---|---|
| `admin_settings` | Single-row config: username, password_hash, bot_enabled, bot_token, ai_base_url, ai_api_key, ai_model, warn_in_group |
| `monitored_groups` | Chat IDs to monitor, with title and added_at timestamp |
| `new_users` | Recent joins (24h TTL), PK `(user_id, chat_id)`, stores display_name, username, bio |
| `verification_bots` | Bot IDs recognized as verification bots, with label |
| `event_logs` | Persisted log entries with timestamp, level, category, source, tags, chat_id, user_id, username, is_new, mutual_groups, message, raw. Indexed on timestamp. Purged after 7 days. |

## logstream.Entry Fields

```go
type Entry struct {
    Timestamp    time.Time
    Level        string    // INFO, WARN, ERROR
    Category     string    // JOIN, BOT_JOIN, LEAVE, RESTRICT, BAN, REMOVE, etc.
    Tags         []string  // e.g. ["BOT_OP"] — supplementary labels
    Source       string    // "message", "business", "guest", "chat_member"
    ChatID       int64
    ChatName     string
    UserID       int64
    Username     string
    IsNew        bool      // true if user joined < 24h ago
    MutualGroups int       // count of monitored groups the user is in
    Message      string
    Raw          string
}
```

## Frontend

- Zero-build: Alpine.js + Tailwind CDN loaded from `<script>` tags
- The HTML file is embedded at compile time via `//go:embed index.html`
- SSE reconnects automatically every 3 seconds on disconnect
- Logs are capped at 2000 in the browser (trimmed to 1000 when exceeded)
- Display slice is the last 200 entries
- Stats row: 5 cards (Groups Monitored, Events Logged, Warnings, Banned 24h, NEW Users 24h)
- Category filter toggle buttons for each event category
- Source filter: ALL / MSG / BIZ / GUEST toggle buttons
- NEW Users filter toggle button
- Group filter dropdown
- Text search across messages/categories/usernames
- Auto-scroll toggle
- Bot status indicators: NO TOKEN / ACTIVE / PAUSED
- Badges: NEW (purple, new user), MG (orange, mutual groups), BOT_OP (amber, verification bot actions), BIZ (business message), GUEST (guest message)
- Settings modal sections:
  - Bot token management
  - Password change, username change
  - Group Warning Messages toggle (warn-in-group)
  - Verification Bots management (add/remove bot IDs with labels)
  - AI Spam Assessment configuration (Base URL, API Key, Model, Test button)
- Admin-only categories: `SYSTEM`, `CONFIG`

## Adding a New Event Category

1. Add detection logic in `internal/bot/bot.go` (in `processMessage`, `analyzeContent`, `analyzeEntities`, or a new method)
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
