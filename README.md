<div align="center">
  <img src="static/logo.webp" alt="OllaMail logo" width="120" />
  <h1>OllaMail (AWS)</h1>
  <p><strong>Serverless Gmail email labeling — rules in plain English, classified by Amazon Bedrock.</strong></p>
  <img src="https://img.shields.io/badge/AWS-Lambda-FF9900?logo=awslambda&logoColor=white" alt="AWS Lambda" />
  <img src="https://img.shields.io/badge/Amazon-Bedrock-232F3E?logo=amazon&logoColor=white" alt="Bedrock" />
  <img src="https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white" alt="Go" />
</div>

---

OllaMail connects to your Gmail accounts via OAuth, scans recent emails on a schedule, and runs each email through rules you define in plain English. An LLM on **Amazon Bedrock** (Nova Micro by default) decides whether each rule applies and performs the matching action automatically — applying Gmail labels, archiving, trashing, marking as spam, or marking as read.

> **Migrated from local Ollama:** this app began as a self-hosted Ollama + SQLite + Docker project. It now runs entirely on AWS — Lambda for compute, DynamoDB for storage, and Bedrock for inference. The name is retained for continuity.

## Features

- **Plain-English rules** — write prompts like "newsletters from SaaS products" and map them to a label or action
- **Multiple Gmail actions** — apply labels, archive, trash, mark as spam, or mark as read
- **Stop-processing rules** — halt evaluation of subsequent rules for an email once a rule matches
- **Drag-and-drop rule ordering** — control the order in which rules are evaluated
- **Per-account or global rules** — scope a rule to a specific account or apply it across all accounts
- **AI prompt builder** — describe what you want to catch in plain English; the LLM writes the classifier instruction for you (streaming output)
- **Batch classification** — all rules for an email are evaluated in a single Bedrock call for efficiency
- **Multiple accounts** — add as many Gmail accounts as you like via OAuth
- **Web UI** — manage accounts, rules, retention, settings, and logs from a browser
- **Auto-label creation** — labels are created in Gmail automatically if they don't exist
- **Email retention management** — set per-label or global retention rules that auto-trash old emails; add label exemptions to protect important labels
- **Categorization history** — searchable and filterable log of every labeling decision, with recategorization
- **Log export** — download processing logs as CSV
- **Config import/export** — full backup and restore of accounts, rules, settings, and retention as JSON
- **Deduplication** — each email is evaluated once per account and never reprocessed

---

## Architecture

```
                Function URL (AWS_IAM)          EventBridge Scheduler
                        │                          rate(1 minute)
                        ▼                                 │
                ┌───────────────┐               ┌─────────▼─────────┐
                │  WebFunction  │               │   ScanFunction    │
                │  (MODE=web)   │               │   (MODE=scan)     │
                │  HTMX web UI  │               │  scanOnce() pass  │
                └───────┬───────┘               └─────────┬─────────┘
                        │                                 │
             ┌──────────┴───────────┬─────────────────────┤
             ▼                      ▼                     ▼
      ┌─────────────┐       ┌───────────────┐     ┌──────────────┐
      │  DynamoDB   │       │    Bedrock    │     │  Gmail API   │
      │  `ollamail` │       │  Nova Micro   │     │  (OAuth 2.0) │
      └─────────────┘       └───────────────┘     └──────────────┘
                                    ▲
                        SSM SecureString /ollamail/credentials
                          (Google OAuth client JSON)
```

- **Two image-based Lambdas**, both built from the same `Dockerfile` (`x86_64`):
  - **WebFunction** — serves the management UI, exposed via a **Lambda Function URL** locked to `AuthType: AWS_IAM` (nothing is public; browse it via a local SigV4 proxy).
  - **ScanFunction** — triggered by **EventBridge on a fixed `rate(1 minute)` schedule**. Runs one labeling pass (`scanOnce`) per invocation. Overlapping runs are safe — processed emails are deduped in DynamoDB.
- **DynamoDB** single-table `ollamail` (on-demand, TTL enabled) — accounts, rules, history, logs, retention, suggestions, OAuth tokens.
- **Amazon Bedrock** — classification and the prompt builder (`us.amazon.nova-micro-v1:0` by default; selectable in Settings).
- **SSM Parameter Store** — the Google OAuth **client** JSON, stored as a SecureString at `/ollamail/credentials`.

> **Scan cadence is fixed in the template** (`template.yaml`, `rate(1 minute)`). There is no user-configurable poll interval — to change cadence, edit the `ScheduleExpression` and redeploy. The **Scan Now** button on the dashboard runs an immediate pass in the web request.

---

## Prerequisites

- An AWS account with Bedrock model access enabled for the chosen model (see step 3).
- [AWS SAM CLI](https://docs.aws.amazon.com/serverless-application-model/latest/developerguide/install-sam-cli.html) and AWS credentials configured.
- Docker (SAM builds the Lambda container images locally).
- A Google Cloud project (free tier is fine).

---

### 1. Google Cloud Setup

1. Go to [console.cloud.google.com](https://console.cloud.google.com) and create a project.
2. Enable the **Gmail API** and **Google People API**.
3. Go to **APIs & Services → Credentials → Create Credentials → OAuth client ID**.
4. Choose **Web application**.
5. Under **Authorized redirect URIs**, add: `http://localhost`
6. Click **Create** and download the JSON file — this is your OAuth **client** credentials.

#### Publish the consent screen (stops the 7-day token revocation)

This app requests the **restricted** `gmail.modify` scope. While the OAuth consent screen is in **Testing** mode, Google **expires refresh tokens after 7 days**, so accounts silently stop working roughly weekly. To fix this:

1. **APIs & Services → OAuth consent screen**.
2. **Publishing status → Publish app** (move from *Testing* to *In production*).
3. Because the app is unverified with a restricted scope, consent will show an **"unverified app"** warning. For your own account, click **Advanced → Go to \<app name\> (unsafe)** and continue. Published apps do **not** apply the 7-day test-mode refresh-token expiry.
4. After publishing, **re-run the OAuth flow once** in the web UI (Accounts → add account) to mint a fresh, non-expiring refresh token.

> Full Google verification (CASA security assessment) is only required to remove the warning entirely or to serve more than 100 external users — **not needed for personal use**.

---

### 2. Store the OAuth credentials in SSM

```bash
aws ssm put-parameter \
  --name /ollamail/credentials \
  --type SecureString \
  --value "$(cat credentials.json)" \
  --region us-east-1
```

The parameter ARN is referenced by `samconfig.toml` (`CredentialsSsmArn`). The Lambda reads it at runtime via `CREDENTIALS_SSM_PARAM`.

---

### 3. Enable Bedrock model access

In the Bedrock console (**Model access**), request/enable access to the classification model — by default **Amazon Nova Micro** (`us.amazon.nova-micro-v1:0`) in `us-east-1`. The Lambda role grants `bedrock:InvokeModel*` and the list APIs used to populate the Settings model picker.

---

### 4. Deploy

Deploys happen automatically via GitHub Actions (`.github/workflows/deploy.yml`) on push to `main`, using OIDC role assumption (no stored keys). To deploy manually:

```bash
sam build
sam deploy   # uses samconfig.toml: stack `ollamail`, region us-east-1
```

Outputs include the **WebFunctionUrl** (the AWS_IAM-protected Function URL) and the DynamoDB table name.

---

### 5. Open the web interface

The Function URL is locked to `AWS_IAM`, so it can only be reached with SigV4-signed requests. Use the bundled signing proxy:

```bash
# Ensure your AWS CLI credentials are available, then:
go run ./tools/sigv4proxy
# open http://localhost:8080
```

Flags let you override `-target` (Function URL), `-region`, and `-listen`.

| Page | Description |
|---|---|
| **Dashboard** | Account/rule counts, scan cadence, recent activity, **Scan Now** |
| **Accounts** | Add Gmail accounts via OAuth; enable/disable/delete |
| **Prompts** | Define labeling rules in plain English; drag to reorder |
| **Builder** | AI prompt builder — describe what to catch, let Bedrock write the classifier |
| **History** | Searchable log of every labeling decision; recategorize |
| **Settings** | Choose Bedrock models; backup/restore config |
| **Logs** | Per-account processing history; CSV export |
| **Retention** | Per-label and global email retention rules; label exemptions |
| **Troubleshooting** | Test rules against sample emails and inspect raw LLM responses |

---

## Configuration Reference

Configuration is set via Lambda environment variables (see `template.yaml`).

| Variable | Default | Description |
|---|---|---|
| `BEDROCK_MODEL` | `us.amazon.nova-micro-v1:0` | Default Bedrock model (Settings can override per-function) |
| `DDB_TABLE` | `ollamail` | DynamoDB table name |
| `GMAIL_MAX_RESULTS` | `50` | Emails fetched per inbox scan (only unprocessed ones are classified) |
| `GMAIL_LOOKBACK_HOURS` | `24` | How far back to look for emails on each scan |
| `EMAIL_BODY_TRUNCATION` | `3000` | Max characters of email body sent to the LLM |
| `LOG_RETENTION_DAYS` | `30` | Days to keep processing log/history entries |
| `HISTORY_MAX_LIMIT` | `500` | Maximum rows returned in history/log queries |
| `CREDENTIALS_SSM_PARAM` | `/ollamail/credentials` | SSM SecureString holding the Google OAuth client JSON |
| `MODE` | `web` | `web` (UI server) or `scan` (EventBridge-triggered pass) |
| `DEBUG_LOGGING` | `0` | Set to `1` for verbose logging |

Scan cadence is **not** an environment variable — it is the `ScheduleExpression` on the `ScanFunction` in `template.yaml`.

---

## How It Works

1. **EventBridge** invokes the ScanFunction every minute (or you click **Scan Now** in the UI). Both call the same `scanOnce` pass.
2. For each active Gmail account, it fetches recent emails (bounded by `GMAIL_MAX_RESULTS` and `GMAIL_LOOKBACK_HOURS`).
3. Each email body is truncated to `EMAIL_BODY_TRUNCATION` characters and all active rules are sent to **Bedrock in a single call**, which returns a structured per-rule true/false decision.
4. For each matched rule, the configured action is applied via the Gmail API (label, archive, trash, spam, mark as read). Labels are created automatically if missing.
5. If a matched rule has **stop processing** enabled, no further rules are evaluated for that email.
6. Processed message IDs are stored in DynamoDB (TTL-bounded) so each email is evaluated only once per account.
7. OAuth access tokens are refreshed automatically; refreshed tokens are written back to DynamoDB.

---

## Development Setup

```bash
git clone https://github.com/sloccy/OllaMail.git
cd OllaMail

# Frontend vendor assets (Bootstrap, htmx) are committed and embedded at compile time.
go build ./...
go vet ./...
go test ./...
```

To run the web server locally against AWS (DynamoDB + Bedrock + SSM), export AWS credentials and:

```bash
export MODE=web
export DDB_TABLE=ollamail
export CREDENTIALS_SSM_PARAM=/ollamail/credentials
go run .        # serves on :5000 (or $AWS_LWA_PORT)
```

---

## Tech Stack

| Component | Technology |
|---|---|
| Compute | AWS Lambda (container images, `x86_64`) + Lambda Web Adapter |
| Scheduling | EventBridge Scheduler (`rate(1 minute)`) |
| Backend | Go 1.25, net/http (stdlib) |
| UI | Bootstrap 5.3 (dark mode) + HTMX 2.0 |
| Database | DynamoDB (single-table, on-demand, TTL) |
| LLM runtime | Amazon Bedrock (Nova Micro) |
| Gmail integration | Google OAuth 2.0 + Gmail REST API |
| Secrets | SSM Parameter Store (SecureString) |
| Deployment | AWS SAM + GitHub Actions (OIDC) |
</content>
