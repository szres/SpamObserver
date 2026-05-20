# SpamObserver

Passive Telegram anti-spam monitoring bot with AI-powered assessment. Observes and logs group events in real time, with optional in-group spam/flood warnings.

SpamObserver watches joins, bans, mutes, bot commands, advertising links, and suspicious keywords across your configured groups. New users are tracked and assessed by pattern matching and optional AI analysis. All events stream to a browser dashboard via SSE (Server-Sent Events).

## Features

- **Webhook-based** — receives updates via Telegram Webhook (not polling), single-binary architecture
- **Real-time SSE dashboard** — dark-mode WebUI with auto-scrolling log terminal, group management, and filters
- **Instant effect** — adding/removing monitored groups takes effect immediately, no restart required
- **Lightweight** — SQLite storage, in-memory ring buffer (200 entries), zero-build frontend (Alpine.js + Tailwind CDN)
- **New-user tracking** — records display name, username, and bio for users who joined within the last 24 hours
- **AI spam assessment** — optional OpenAI-compatible LLM integration evaluates new user profiles for spam risk
- **Flood detection** — detects join floods (3+ joins in 1 minute) and triggers alerts
- **In-group warnings** — optional: sends and auto-deletes warning messages when spam or floods are detected
- **Verification bot awareness** — tracks actions by configured verification bots (e.g. CAPTCHA bots)
- **Log persistence** — events stored in SQLite with 7-day retention, auto-purged hourly

## Monitored Events

| Category | Level | Description |
|---|---|---|
| `JOIN` | INFO | New member joined the group |
| `BOT_JOIN` | WARN | A bot was added to the group |
| `LEAVE` | INFO | Member left or was removed |
| `RESTRICT` | WARN | Member muted or restricted |
| `BAN` | WARN | Member banned |
| `REMOVE` | INFO | Member removed from group |
| `PROMOTE` | INFO | User promoted to member |
| `ADMIN` | INFO | User promoted to admin/creator |
| `BOT_MSG` | INFO | Message sent by a bot |
| `BUTTON` | INFO | Inline button callback clicked |
| `AD_LINK` | WARN | Advertising link detected (`t.me/`, `joinchat`) |
| `AD_KEYWORD` | WARN | Suspicious keyword detected (crypto, invest, etc.) |
| `COMMAND` | INFO | Bot command observed (`/verify`, `/captcha`, `/ban`, etc.) |
| `URL_ENTITY` | INFO | URL entity in a message |
| `TEXT_LINK` | INFO | Hyperlink entity in a message |
| `MENTION` | INFO | `@mention` entity in a message |
| `HASHTAG` | INFO | `#hashtag` entity in a message |
| `BOT_COMMAND` | INFO | Bot command entity in a message |
| `NEW_USER` | INFO | New user joined (tracked for 24h) |
| `NEW_MSG` | WARN | Message from a user who joined < 24h ago |
| `QUOTE` | INFO | Reply/quote message |
| `AI_ASSESS` | INFO | AI spam assessment result |
| `SPAM_CONFIRMED` | ERROR | AI-confirmed spam account |
| `SPAM_HIGH_RISK` | WARN | AI high-risk assessment |
| `SPAM_WARNING` | INFO | In-group spam warning sent |
| `FLOOD_ALERT` | ERROR | Join flood detected |
| `FLOOD_END` | INFO | Flood mode deactivated |

## Architecture

```
┌─────────────────────────────────────────────────┐
│                  Caddy (host)                    │
│           SSL termination + reverse proxy        │
└────────────────────┬────────────────────────────┘
                     │ :443 → :8080
┌────────────────────▼────────────────────────────┐
│              SpamObserver Container              │
│                                                  │
│  ┌──────────┐  ┌──────────┐  ┌───────────────┐ │
│  │  Fiber   │  │  Telego  │  │   SQLite DB   │ │
│  │  Server  │  │ Webhook  │  │  (WAL mode)   │ │
│  └────┬─────┘  └────┬─────┘  └───────────────┘ │
│       │              │                           │
│  ┌────▼──────────────▼─────┐                    │
│  │     Bot Monitor         │                    │
│  │  (event filters +       │                    │
│  │   pattern detection +   │                    │
│  │   AI assessment +       │                    │
│  │   flood detection)      │                    │
│  └────────────┬────────────┘                    │
│               │                                  │
│  ┌────────────▼────────────┐                    │
│  │   SSE Log Broker        │                    │
│  │   (ring buffer +        │                    │
│  │    broadcast channels)  │                    │
│  └─────────────────────────┘                    │
└──────────────────────────────────────────────────┘
```

## Endpoint Map

| Path | Method | Auth | Description |
|---|---|---|---|
| `/` | GET | — | WebUI (embedded HTML) |
| `/api/webhook/tg` | POST | Secret header | Telegram Webhook receiver |
| `/api/auth/login` | POST | — | Authenticate, sets session cookie |
| `/api/auth/logout` | POST | — | Clear session |
| `/api/auth/me` | GET | — | Check current session |
| `/api/logs/stream` | GET | — | SSE event stream |
| `/api/logs/recent` | GET | — | Recent log entries (JSON) |
| `/api/bot/status` | GET | — | Bot enabled state |
| `/api/bot/info` | GET | — | Bot token + enabled state |
| `/api/bot/new-users-count` | GET | — | New users in last 24h |
| `/api/bot/banned-count` | GET | — | Bans in last 24h |
| `/api/groups` | GET | — | Public group list |
| `/api/config/groups` | GET | Cookie | List monitored groups |
| `/api/config/groups` | POST | Cookie | Add a group |
| `/api/config/groups/:chatId` | DELETE | Cookie | Remove a group |
| `/api/config/bot/toggle` | POST | Cookie | Enable/disable monitoring |
| `/api/config/bot/warn-in-group` | GET | Cookie | Get warn-in-group state |
| `/api/config/bot/warn-in-group` | POST | Cookie | Set warn-in-group toggle |
| `/api/config/bot/token` | GET | Cookie | Masked bot token info |
| `/api/config/bot/token` | POST | Cookie | Set bot token + restart |
| `/api/config/auth/change-password` | POST | Cookie | Change admin password |
| `/api/config/auth/change-username` | POST | Cookie | Change admin username |
| `/api/config/verify-bots` | GET | Cookie | List verification bots |
| `/api/config/verify-bots` | POST | Cookie | Add verification bot |
| `/api/config/verify-bots/:botId` | DELETE | Cookie | Remove verification bot |
| `/api/config/new-users` | GET | Cookie | List new users (24h) |
| `/api/config/ai` | GET | Cookie | Get AI config (key masked) |
| `/api/config/ai` | POST | Cookie | Set AI config |
| `/api/config/ai/test` | POST | Cookie | Test AI connection |

## Quick Start

### 1. Prerequisites

- Docker and Docker Compose
- A Telegram Bot Token (from [@BotFather](https://t.me/BotFather))
- A public HTTPS URL pointing to your server (Caddy, Nginx, etc.)
- Your bot must be **added to target groups** as a member (not admin — admin is not required for passive reading)

### 2. Generate Secrets

```bash
./scripts/gen-secret.sh
```

Output:

```
-------------------------------------------
 SpamObserver — Generated Secrets
-------------------------------------------
WEBHOOK_SECRET       k8Xz9mP2vQ...alphanumeric...
JWT_SECRET           mP2vQ...base64...
-------------------------------------------

Paste these into your .env file.
```

You can also generate only one token:

```bash
./scripts/gen-secret.sh webhook    # WEBHOOK_SECRET only
./scripts/gen-secret.sh jwt        # JWT_SECRET only
./scripts/gen-secret.sh --length 48  # longer tokens
```

If `openssl` is not available, the script falls back to `/dev/urandom` automatically.

### 3. Configure

```bash
cp .env.example .env
```

Edit `.env`:

```dotenv
BOT_TOKEN=123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11
PUBLIC_URL=https://observer.example.com
WEBHOOK_SECRET=<output from gen-secret.sh>
INITIAL_ADMIN_PASSWORD=<pick a strong password>
JWT_SECRET=<output from gen-secret.sh>
```

### 4. Deploy

```bash
docker compose up -d --build
```

The container will:
1. Start the HTTP server on port 8080
2. Register the webhook URL with Telegram (`SetWebhook`)
3. Begin accepting updates at `/api/webhook/tg`

### 5. Caddy Reverse Proxy (Host)

Add to your Caddyfile:

```
observer.example.com {
    reverse_proxy localhost:8080
}
```

Caddy handles TLS automatically.

### 6. Access the Dashboard

Open `https://observer.example.com`, log in with your admin credentials, and add the Chat IDs of the groups you want to monitor.

**Finding a group's Chat ID**: Add [@userinfobot](https://t.me/userinfobot) or [@getmyid_bot](https://t.me/getmyid_bot) to the group, or forward a group message to [@JsonDumpBot](https://t.me/JsonDumpBot).

## Environment Variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `BOT_TOKEN` | No* | — | Telegram Bot API token |
| `PUBLIC_URL` | No* | — | Public HTTPS URL (e.g., `https://observer.example.com`) |
| `WEBHOOK_SECRET` | No | `spamo-whsec` | Secret token for webhook request validation |
| `PORT` | No | `8080` | HTTP listen port |
| `DB_PATH` | No | `./data/spam-observer.db` | SQLite database file path |
| `INITIAL_ADMIN_USERNAME` | No | `admin` | Admin login username (set on first run only) |
| `INITIAL_ADMIN_PASSWORD` | No | `admin` | Admin login password (set on first run only) |
| `JWT_SECRET` | Auto | — | HMAC signing key for session tokens (auto-generated if empty) |

\* `BOT_TOKEN` can alternatively be set via the Settings UI and stored in SQLite. `PUBLIC_URL` is only needed for webhook registration — without it the bot starts but doesn't receive updates.

Admin credentials are only written to the database on first run. Changing env vars after first run has no effect on the admin password — use the Settings UI or reset the DB.

## Telegram Bot Setup

1. Create a bot via [@BotFather](https://t.me/BotFather)
2. **Disable privacy mode** (optional but recommended for full message visibility):
   ```
   /setprivacy → Select your bot → Disable
   ```
   This allows the bot to see all group messages, not just commands and mentions.
3. Add the bot to your target groups as a regular member
4. Start SpamObserver and add the group Chat IDs via the dashboard

> **Important**: Privacy mode controls what the bot can *receive*. SpamObserver only *sends* in-group warnings for detected spam/flood events (if the warn-in-group option is enabled).

## Manual Build (Without Docker)

```bash
# Requires Go 1.26+
go build -o spam-observer .

BOT_TOKEN="your-token" \
PUBLIC_URL="https://observer.example.com" \
./spam-observer
```

## Project Structure

```
.
├── main.go                          # Entrypoint: Fiber + Telego wiring
├── internal/
│   ├── ai/ai.go                     # OpenAI-compatible LLM spam assessment
│   ├── auth/auth.go                 # HMAC-SHA256 session management
│   ├── bot/bot.go                   # Update processing, pattern detection, flood detection, AI assessment
│   ├── db/db.go                     # SQLite schema, queries, AI config, verification bots
│   ├── handler/handler.go           # HTTP route handlers
│   ├── logstream/logstream.go       # Ring buffer & SSE broadcast
│   ├── tracker/tracker.go           # New-user tracking (24h TTL)
│   └── webui/
│       ├── embed.go                 # go:embed directive
│       └── index.html               # Single-page dashboard
├── scripts/
│   └── gen-secret.sh                # Secret token generator
├── Dockerfile                       # Multi-stage build
├── docker-compose.yml               # Compose config
├── .env.example                     # Environment template
└── .gitignore
```

## License

MIT
