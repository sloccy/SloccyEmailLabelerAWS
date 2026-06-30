<div align="center">
  <img src="app/static/logo.webp" alt="OllaMail logo" width="120" />
  <h1>OllaMail</h1>
  <p><strong>Local LLM email labeling for Gmail — fully self-hosted, no data leaves your machine.</strong></p>
  <img src="https://img.shields.io/badge/Docker-ready-2496ED?logo=docker&logoColor=white" alt="Docker" />
  <img src="https://img.shields.io/badge/Ollama-powered-black?logo=llama&logoColor=white" alt="Ollama" />
  <img src="https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white" alt="Go" />
</div>

---

OllaMail connects to your Gmail accounts via OAuth, fetches recent emails on a schedule, and runs each email through rules you define in plain English. A local LLM (via [Ollama](https://ollama.com)) decides whether each rule applies and performs the matching action automatically — applying Gmail labels, archiving, trashing, and more. No email content ever leaves your machine.

## Features

- **Plain-English rules** — write prompts like "newsletters from SaaS products" and map them to a label or action
- **Multiple Gmail actions** — apply labels, archive, trash, mark as spam, or mark as read
- **Stop-processing rules** — halt evaluation of subsequent rules for an email once a rule matches
- **Drag-and-drop rule ordering** — control the order in which rules are evaluated
- **Per-account or global rules** — scope a rule to a specific account or apply it across all accounts
- **AI prompt builder** — describe what you want to catch in plain English; the LLM writes the classifier instruction for you (streaming output)
- **Batch classification** — all rules for an email are evaluated in a single LLM call for efficiency
- **Multiple accounts** — add as many Gmail accounts as you like via OAuth
- **Fully local** — all LLM inference runs on-device via Ollama; no email content is sent to any API
- **Web UI** — manage accounts, rules, retention, settings, and logs from a browser
- **Auto-label creation** — labels are created in Gmail automatically if they don't exist
- **Email retention management** — set per-label or global retention rules that auto-trash old emails; add label exemptions to protect important labels
- **Categorization history** — searchable and filterable log of every labeling decision
- **Log export** — download processing logs as CSV
- **Config import/export** — full backup and restore of accounts, rules, settings, and retention as JSON
- **Deduplication** — each email is evaluated once per account and never reprocessed
- **Configurable polling** — set the interval in the UI; adjust lookback window and batch size via env vars
- **Raspberry Pi friendly** — works on Pi 4 (4 GB+); Docker handles auto-start on boot

---

## Quick Start (Docker)

### Prerequisites

- [Docker](https://docs.docker.com/get-docker/) and Docker Compose
- A Google Cloud project (free tier is fine)
- A machine with at least 4 GB RAM

---

### 1. Google Cloud Setup

1. Go to [console.cloud.google.com](https://console.cloud.google.com) and create a new project.
2. Enable the **Gmail API** and **Google People API** for the project.
3. Go to **APIs & Services → Credentials → Create Credentials → OAuth client ID**.
4. Choose **Web application** as the application type.
5. Under **Authorized redirect URIs**, add:
   ```
   http://localhost
   ```
6. Click **Create**, then download the JSON file.
7. Save it as `credentials/credentials.json` in your project directory.
8. Go to **APIs & Services → OAuth consent screen** and add your Gmail address(es) as **Test users**.

   > **Important:** Without adding your address as a test user, the OAuth flow will fail with an access denied error.

---

### 2. Create the directory structure

```
ollamail/
├── docker-compose.yml
├── credentials/
│   └── credentials.json    ← paste your downloaded OAuth JSON here
└── data/                   ← SQLite database (auto-created on first run)
```

---

### 3. Create `docker-compose.yml`

```yaml
services:
  ollama:
    image: ollama/ollama:latest
    container_name: ollamail-ollama
    volumes:
      - ollama_data:/root/.ollama
    environment:
      - OLLAMA_NUM_PARALLEL=2
      # Set to your poll interval (or just above). OLLAMA_KEEP_ALIVE counts
      # from when the last LLM query finishes, and the poller uses wall-clock
      # scheduling, so the model is always still warm when the next tick fires.
      - OLLAMA_KEEP_ALIVE=60s
    restart: unless-stopped
    networks:
      - internal
    # Uncomment to enable NVIDIA GPU support:
    # deploy:
    #   resources:
    #     reservations:
    #       devices:
    #         - driver: nvidia
    #           count: all
    #           capabilities: [gpu]

  app:
    image: ghcr.io/sloccy/ollamail:latest
    container_name: ollamail-app
    ports:
      - "5001:5000"
    volumes:
      - ./data:/data
      - ./credentials:/credentials
    environment:
      - OLLAMA_HOST=http://ollama:11434
      - OLLAMA_MODEL=qwen3.5:4b-q4_K_M
      - DATA_DIR=/data
      - CREDENTIALS_FILE=/credentials/credentials.json
      # Optional overrides — see Configuration Reference below
      # - GMAIL_LOOKBACK_HOURS=24
      # - GMAIL_MAX_RESULTS=50
      # - POLL_INTERVAL=300
    depends_on:
      - ollama
    restart: unless-stopped
    networks:
      - internal

volumes:
  ollama_data:

networks:
  internal:
```

---

### 4. Start the app

```bash
docker compose up -d
```

On first start the app will automatically pull the configured Ollama model. This can take a few minutes depending on your connection speed. Watch progress with:

```bash
docker compose logs -f app
```

---

### 5. Open the web interface

Navigate to **http://localhost:5001**.

| Page | Description |
|---|---|
| **Dashboard** | Poller status, processing stats, and recent activity |
| **Accounts** | Add Gmail accounts via OAuth |
| **Prompts** | Define labeling rules in plain English |
| **Builder** | AI prompt builder — describe what to catch and let the LLM write the classifier |
| **History** | Searchable log of every labeling decision |
| **Settings** | Set poll interval and other runtime options |
| **Logs** | View per-account processing history; CSV export |
| **Retention** | Configure per-label and global email retention rules |

---

## Development Setup

To run without Docker:

```bash
# Clone the repo
git clone https://github.com/sloccy/OllaMail.git
cd OllaMail

# Build frontend vendor assets (Bootstrap, htmx) and minify app.js / style.css
npm install && npm run build

# Build the binary (templates and static files are embedded at compile time)
go build -o ollamail .

# Set required environment variables
export OLLAMA_HOST=http://localhost:11434
export DATA_DIR=./data
export CREDENTIALS_FILE=./credentials/credentials.json

# Run the app
./ollamail
```

You'll also need [Ollama](https://ollama.com) running locally and the model pulled:

```bash
ollama pull qwen3.5:4b-q4_K_M
```

---

## Configuration Reference

All settings are controlled via environment variables.

| Variable | Default | Description |
|---|---|---|
| `OLLAMA_HOST` | `http://localhost:11434` | URL of the Ollama instance |
| `OLLAMA_MODEL` | `qwen3.5:4b-q4_K_M` | Model to use for classification |
| `OLLAMA_TIMEOUT` | `600` | Seconds to wait for Ollama to respond or pull a model |
| `OLLAMA_NUM_CTX` | `4096` | LLM context window size in tokens |
| `GMAIL_MAX_RESULTS` | `50` | Emails fetched per inbox scan (only unprocessed ones are classified) |
| `GMAIL_LOOKBACK_HOURS` | `24` | How far back to look for emails on each scan |
| `EMAIL_BODY_TRUNCATION` | `3000` | Max characters of email body sent to the LLM |
| `LOG_RETENTION_DAYS` | `30` | Days to keep processing log entries |
| `POLL_INTERVAL` | `300` | Default poll interval in seconds (also configurable in the UI) |
| `MIN_POLL_INTERVAL` | `30` | Minimum allowed poll interval in seconds |
| `HISTORY_MAX_LIMIT` | `500` | Maximum rows returned in history/log queries |
| `DEBUG_LOGGING` | `0` | Set to `1` to enable verbose debug logging |
| `DATA_DIR` | `/data` | Directory where the SQLite database is stored |
| `CREDENTIALS_FILE` | `/credentials/credentials.json` | Path to the Google OAuth client credentials JSON |

---

## How It Works

```
┌─────────────┐     OAuth      ┌─────────────┐
│   Gmail API │ ◄────────────► │  OllaMail   │
└─────────────┘                │     (Go)    │
                               └──────┬──────┘
                                      │ email body + all rules
                               ┌──────▼──────┐
                               │   Ollama    │
                               │  (local LLM)│
                               └──────┬──────┘
                                      │ per-rule YES/NO (single call)
                               ┌──────▼──────┐
                               │ Apply label │
                               │ / action    │
                               │ via Gmail   │
                               │ API         │
                               └─────────────┘
```

1. The poller wakes up on a wall-clock schedule (every N seconds, configurable in the UI). The timer resets at the start of each tick, not after the scan finishes, so the cadence stays stable regardless of scan duration.
2. For each active Gmail account, it fetches recent emails (limited by `GMAIL_MAX_RESULTS` and `GMAIL_LOOKBACK_HOURS`).
3. Each email body is truncated to `EMAIL_BODY_TRUNCATION` characters and all active rules are sent to the LLM **in a single call**, which returns a structured per-rule true/false decision.
4. For each matched rule, the configured action is applied via the Gmail API (label, archive, trash, spam, mark as read). Labels are created in Gmail automatically if they don't exist.
5. If a matched rule has **stop processing** enabled, no further rules are evaluated for that email.
6. Processed email IDs are stored in SQLite so each email is evaluated only once per account.

---

## Raspberry Pi / Low-Power Notes

- Tested on Raspberry Pi 4 (4 GB RAM) with 64-bit OS.
- Inference time is 5–20 seconds per email per prompt rule depending on email length and the model used.
- Smaller quantized models are significantly faster on CPU-only hardware.
- `restart: unless-stopped` in Docker Compose handles automatic startup after reboots.
- The `ollama_data` named volume persists pulled models across container restarts.

---

## Tech Stack

| Component | Technology |
|---|---|
| Backend | Go 1.26 |
| HTTP server | net/http (stdlib) |
| UI | Bootstrap 5.3 (dark mode) + HTMX 2.0 |
| Database | SQLite via modernc.org/sqlite + sqlc |
| LLM runtime | Ollama |
| Gmail integration | Google OAuth 2.0 + Gmail REST API |
| Deployment | Docker / Docker Compose |
