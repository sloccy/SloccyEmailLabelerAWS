<div align="center">
  <img src="static/logo.webp" alt="OllaMail logo" width="120" />
  <h1>OllaMail (AWS)</h1>
  <p><strong>Serverless Gmail email labeling — rules in plain English, classified by Amazon Bedrock.</strong></p>
  <img src="https://img.shields.io/badge/AWS-Lambda-FF9900?logo=awslambda&logoColor=white" alt="AWS Lambda" />
  <img src="https://img.shields.io/badge/Amazon-Bedrock-232F3E?logo=amazon&logoColor=white" alt="Bedrock" />
  <img src="https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white" alt="Go" />
</div>

---

OllaMail connects to your Gmail accounts via OAuth, processes new mail the moment it arrives (Gmail push via Pub/Sub, with a daily catch-up scan as a safety net), and runs each email through rules you define in plain English. An LLM on **Amazon Bedrock** (model selectable in Settings) decides whether each rule applies and performs the matching action automatically — applying Gmail labels, archiving, trashing, marking as spam, or marking as read.

> **Migrated from local Ollama:** this app began as a self-hosted Ollama + SQLite + Docker project. It now runs entirely on AWS — Lambda for compute, DynamoDB for storage, and Bedrock for inference. The name is retained for continuity.

## Features

- **Plain-English rules** — write prompts like "newsletters from SaaS products" and map them to a label or action
- **Multiple Gmail actions** — apply labels, archive, trash, mark as spam, or mark as read
- **Stop-processing rules** — halt evaluation of subsequent rules for an email once a rule matches
- **Drag-and-drop rule ordering** — control the order in which rules are evaluated
- **Per-account or global rules** — scope a rule to a specific account or apply it across all accounts
- **AI prompt builder** — describe what you want to catch in plain English; the LLM writes the classifier instruction for you (streaming output)
- **AI rule improvement, grounded in history** — every recategorization permanently records a labeled example (which rule, which verdict, sender/subject/a short excerpt) against that rule. Improvement calls draw on up to ~30 of the newest examples per rule instead of just the one email that triggered the current round, so rewrites are shaped by what the rule already gets right, not just its latest miss
- **Bulk recategorization** — select multiple emails in History, choose "apply to all" / "remove from all" per rule, and correct them in one action; produces one AI suggestion per rule (not per email) from the combined examples
- **Suggestion validation** — before you review an AI-suggested rewrite, it's optionally replayed against the rule's example corpus on the *classification* model (never the improver) and shown as a pass rate, e.g. "27/30 correct" — toggle in Settings
- **Real-time labeling** — optional Gmail push (Pub/Sub → Lambda webhook) processes mail seconds after arrival
- **Batch classification** — all rules for an email are evaluated in a single Bedrock call for efficiency
- **Standard/Flex Bedrock tiers** — run classification and the prompt improver on Bedrock's discounted flex tier, selectable per model in Settings
- **Reasoning suppression** — chain-of-thought models (Qwen3, Nemotron, …) are automatically told to skip reasoning output, with a manual override for unrecognized model families
- **Multiple accounts** — add as many Gmail accounts as you like via OAuth
- **Web UI** — manage accounts, rules, retention, settings, and logs from a browser
- **Auto-label creation** — labels are created in Gmail automatically if they don't exist
- **Email retention management** — set per-label or global retention rules that auto-trash old emails; add label exemptions to protect important labels
- **Categorization history** — searchable and filterable log of every labeling decision, with single or bulk recategorization
- **Log export** — download processing logs as CSV
- **Config import/export** — full backup and restore of accounts, rules, settings, and retention as JSON
- **Deduplication** — each email is evaluated once per account and never reprocessed

---

## Architecture

```
   Function URL                EventBridge Scheduler      Gmail push → Pub/Sub
   (AWS_IAM, or CloudFront      cron(0 2 * * ? *)         (OIDC-verified POST)
    + Cloudflare Access)        America/New_York                  │
           │                           │                          │
           ▼                           ▼                          ▼
   ┌───────────────┐          ┌─────────────────┐        ┌─────────────────┐
   │  WebFunction  │          │  ScanFunction   │        │  PushFunction   │
   │  (MODE=web)   │          │  (MODE=scan)    │        │  (MODE=push)    │
   │  HTMX web UI  │          │ daily catch-up  │        │ per-message,    │
   │               │          │ + watch renewal │        │ real-time       │
   └───────┬───────┘          └────────┬────────┘        └────────┬────────┘
           │                           │                          │
           └──────────┬────────────────┴──────────┬───────────────┘
                      ▼                           ▼
        ┌─────────────┬───────────────┬──────────────┐
        │  DynamoDB   │    Bedrock    │  Gmail API   │
        │  `ollamail` │  Converse API │  (OAuth 2.0) │
        └─────────────┴───────────────┴──────────────┘
                              ▲
                  SSM SecureStrings: /ollamail/credentials (client JSON)
                     + /ollamail/accounts/<id>/token (per-account)
```

- **Three zip-packaged Lambdas** (`provided.al2023` custom runtime on **arm64/Graviton** — ~20% cheaper per GB-second than x86), all built from the same Go binary (`Makefile` cross-compiles `GOARCH=arm64`; the `MODE` env var selects behavior at runtime):
  - **WebFunction** — serves the management UI via a **Lambda Function URL**. Two auth modes (see [Open the web interface](#5-open-the-web-interface)): `AWS_IAM` (default; browse via a local SigV4 proxy) or, when the `CfAccessAud` stack parameter is set, a **CloudFront distribution behind Cloudflare Access** with the app verifying the Access JWT on every request.
  - **ScanFunction** — triggered by **EventBridge daily at 2 AM Eastern**. Runs one catch-up labeling pass (`scanOnce`) per invocation and renews Gmail `watch()` registrations. Overlapping runs are safe — processed emails are deduped in DynamoDB.
  - **PushFunction** — public Function URL that receives Gmail push notifications from Pub/Sub (OIDC-verified) and processes just the affected account immediately. This is the primary labeling path when push is configured (step 3b).
- **DynamoDB** single-table `ollamail` (provisioned 2 RCU / 2 WCU — inside the always-free tier; TTL enabled) — accounts, rules, history, logs, retention, suggestions, and a permanent per-rule example corpus that AI rule improvement draws on (no TTL — written only on manual recategorization, so it stays tiny relative to the free tier). Per-account Gmail OAuth tokens are **not** in the table: they live as SSM SecureStrings under `/ollamail/accounts/<id>/token`, so table read access alone can't exfiltrate mailbox credentials.
- **Amazon Bedrock** — classification, the prompt builder, and rule improvement (models and Standard/Flex service tier selectable in Settings; falls back to `us.amazon.nova-micro-v1:0` until one is configured). Suggestion validation (Settings toggle, on by default) adds a batch of classify calls per suggestion — always on the *classification* model, never the improver, so the resulting pass rate reflects the model that actually labels your mail.
- **SSM Parameter Store** — the Google OAuth **client** JSON, stored as a SecureString at `/ollamail/credentials`.

> **Scan cadence is fixed in the template** (`template.yaml`, `cron(0 2 * * ? *)` at `America/New_York`, i.e. 2 AM Eastern — off-peak for Bedrock flex-tier traffic). There is no user-configurable poll interval — to change cadence, edit the `ScanSchedule` and redeploy. The **Scan Now** button on the dashboard runs an immediate pass in the web request.

---

## Prerequisites

- An AWS account with Bedrock model access enabled for the chosen model (see step 3).
- [AWS SAM CLI](https://docs.aws.amazon.com/serverless-application-model/latest/developerguide/install-sam-cli.html) and AWS credentials configured.
- Go 1.25+ (`sam build` cross-compiles the Lambda binary via the `Makefile` — no Docker needed).
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
  --region us-east-2
```

The parameter ARN is referenced by `samconfig.toml` (`CredentialsSsmArn`). The Lambda reads it at runtime via `CREDENTIALS_SSM_PARAM`.

---

### 3. Enable Bedrock model access

In the Bedrock console (**Model access**), request/enable access to whichever classification model you plan to use (falls back to **Amazon Nova Micro**, `us.amazon.nova-micro-v1:0`, until one is picked in Settings) in `us-east-2`. The Lambda role grants `bedrock:InvokeModel*` and the list APIs used to populate the Settings model picker.

---

### 3b. (Optional) Enable Gmail push — real-time labeling

By default the app labels mail on a scheduled catch-up scan. To process mail **the moment it arrives** (faster, and far less wasted work), wire up Gmail push via Cloud Pub/Sub. In the **same GCP project** as your OAuth client:

1. **Create a topic** (e.g. `gmail-push`):
   ```bash
   gcloud pubsub topics create gmail-push
   ```
2. **Let Gmail publish to it** — grant the Gmail system service account Publisher:
   ```bash
   gcloud pubsub topics add-iam-policy-binding gmail-push \
     --member=serviceAccount:gmail-api-push@system.gserviceaccount.com \
     --role=roles/pubsub.publisher
   ```
3. **Deploy first** (see step 4) to get the **PushFunctionUrl** stack output — that public URL is the push endpoint.
4. **Create a push subscription with OIDC auth** so the endpoint can verify the caller. Use (or create) a dedicated service account `SA_EMAIL`:
   ```bash
   gcloud pubsub subscriptions create gmail-push-sub \
     --topic=gmail-push \
     --push-endpoint=<PushFunctionUrl> \
     --push-auth-service-account=<SA_EMAIL> \
     --push-auth-token-audience=<PushFunctionUrl>
   ```
5. **Set the stack parameters** so the app registers watches and authenticates pushes. Add to `samconfig.toml` `parameter_overrides` (or pass on the CLI) and redeploy:
   ```
   PubSubTopic=projects/<PROJECT_ID>/topics/gmail-push
   PushOidcAudience=<PushFunctionUrl>
   PushOidcServiceAccount=<SA_EMAIL>
   ```

Once set, each account registers a Gmail `watch()` on OAuth connect (and the daily scan renews it, since Gmail expires watches after ~7 days). Leave the three parameters empty to stay polling-only. **Publishing the OAuth consent screen (step 1 above) is required** — in Testing mode tokens die weekly and push stops.

---

### 4. Deploy

Deploys happen automatically via GitHub Actions (`.github/workflows/deploy.yml`) on push to `main`, using OIDC role assumption (no stored keys); the workflow can also be run manually from the Actions tab (`workflow_dispatch`). To deploy manually:

```bash
sam build
sam deploy   # uses samconfig.toml: stack `ollamail`, region us-east-2
```

Outputs include the **WebFunctionUrl** (Function URL), **WebDistributionDomain** (CloudFront, only when Cloudflare Access is enabled), **PushFunctionUrl**, and the DynamoDB table name.

#### Dependency updates (security-only)

Routine version bumps are disabled. Dependencies move only for security reasons: a weekly `govulncheck` workflow (shared from `sloccy/shared-ci`) bumps modules with *called* vulnerable code to the minimum fixed version and opens an auto-merging PR, and Dependabot handles advisory-driven security updates. Both flows need the `WORKFLOW_TOKEN` repo secret (a fine-grained PAT with Contents + Pull requests read/write) — bot-created events can't trigger CI or the deploy workflow, so without it the PRs stall until a human intervenes.

#### Backups

There is no PITR on the DynamoDB table (deliberate — it bills per GB-month). The hard-to-recreate data (prompt rules, settings, retention rules, account list) is covered by **Settings → backup/restore** (`GET /api/config/export` / `POST /api/config/import`), which excludes credentials. Export a config snapshot before deploying storage-layer changes. Everything else in the table is expendable: logs/history are TTL'd telemetry, processed-markers rebuild, and Gmail tokens can be re-obtained by re-running OAuth for an account.

---

### 5. Open the web interface

Two access modes, chosen by the `CfAccessAud` stack parameter:

**Default (`AWS_IAM`)** — the Function URL only accepts SigV4-signed requests. Use the bundled signing proxy:

```bash
# Ensure your AWS CLI credentials are available, then:
go run ./tools/sigv4proxy
# open http://localhost:8080
```

Flags let you override `-target` (Function URL), `-region`, and `-listen`.

**Cloudflare Access (Zero Trust)** — set the `CfAccessTeamDomain` and `CfAccessAud` stack parameters and redeploy. The template then creates a CloudFront distribution (OAC-signed to the Function URL) and the app verifies the Cloudflare Access JWT on every request (fails closed: with the URL public but the CF vars missing, the server refuses to start). Point a Cloudflare-proxied CNAME at the **WebDistributionDomain** stack output and put a Cloudflare Access application in front of that hostname — the UI is then reachable from any browser after the Access login, no local proxy needed.

| Page | Description |
|---|---|
| **Dashboard** | Account/rule counts, scan cadence, recent activity, **Scan Now** |
| **Accounts** | Add Gmail accounts via OAuth; enable/disable/delete |
| **Prompts** | Define labeling rules in plain English; drag to reorder |
| **Builder** | AI prompt builder — describe what to catch, let Bedrock write the classifier |
| **Prompt Updates** | AI-suggested rule rewrites grounded in each rule's example corpus, with a validation pass rate; review, comment, regenerate, apply |
| **History** | Searchable log of every labeling decision; recategorize one email or select many for a bulk action |
| **Settings** | Choose Bedrock models and Standard/Flex tier per model; reasoning-suppression override; suggestion validation toggle; backup/restore config |
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
| `MODE` | `web` | `web` (UI server), `scan` (EventBridge catch-up pass), or `push` (Pub/Sub webhook) |
| `PUBSUB_TOPIC` | _(empty)_ | `projects/<proj>/topics/<name>`; enables Gmail `watch()` registration/renewal |
| `PUSH_OIDC_AUDIENCE` | _(empty)_ | Expected OIDC audience on the Pub/Sub push token (the PushFunction URL) |
| `PUSH_OIDC_SA_EMAIL` | _(empty)_ | Service-account email the push token must be issued by |
| `CF_ACCESS_TEAM_DOMAIN` | _(empty)_ | Cloudflare Access team domain (`https://<team>.cloudflareaccess.com`); set by the template from `CfAccessTeamDomain` |
| `CF_ACCESS_AUD` | _(empty)_ | Cloudflare Access application audience (AUD) tag; set by the template from `CfAccessAud` |
| `AUTH_MODE` | _(set by template)_ | `iam` or `cfaccess` — mirrors the Function URL auth decision so the app can fail closed if the CF vars drift |
| `CLASSIFY_CONCURRENCY` | `6` | Max emails classified against Bedrock in parallel per account |
| `DEBUG_LOGGING` | `0` | Set to `1` for verbose logging |

Real-time labeling runs via the **PushFunction** (public Function URL, OIDC-verified). The scheduled scan is a **daily safety net** that also renews Gmail watches; it runs at a fixed **2 AM (`America/New_York`)** via the `ScanSchedule` cron in `template.yaml`, timed off-peak since classification may run on Bedrock's flex tier. The cadence isn't user-configurable — there's no Settings control for it.

---

## How It Works

1. **Gmail push** (when configured): a new message triggers a Pub/Sub notification to the PushFunction, which processes just that account immediately. **EventBridge** also invokes the ScanFunction daily as a catch-up + `watch()` renewal (or you click **Scan Now**). All paths share the same per-account processing.
2. For each active Gmail account, it fetches recent emails (bounded by `GMAIL_MAX_RESULTS` and `GMAIL_LOOKBACK_HOURS`).
3. Each email body is truncated to `EMAIL_BODY_TRUNCATION` characters and all active rules are sent to **Bedrock in a single call**, which returns a structured per-rule true/false decision.
4. For each matched rule, the configured action is applied via the Gmail API (label, archive, trash, spam, mark as read). Labels are created automatically if missing.
5. If a matched rule has **stop processing** enabled, no further rules are evaluated for that email.
6. Processed message IDs are stored in DynamoDB (TTL-bounded) so each email is evaluated only once per account.
7. OAuth access tokens are refreshed automatically; refreshed tokens are written back to their SSM SecureString (`/ollamail/accounts/<id>/token`).

---

## Development Setup

```bash
git clone https://github.com/sloccy/SloccyEmailLabelerAWS.git
cd SloccyEmailLabelerAWS

# Frontend vendor assets (Bootstrap, htmx) are pinned in package.json (Dependabot-managed)
# and fetched into static/vendor/ at build time — run once before serving the UI locally:
./scripts/vendor.sh

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
| Compute | AWS Lambda (zip, `provided.al2023`, `arm64`/Graviton) + Lambda Web Adapter layer |
| Real-time trigger | Gmail `watch()` → Cloud Pub/Sub → PushFunction (OIDC-verified) |
| Scheduling | EventBridge Scheduler (daily `cron(0 2 * * ? *)` America/New_York) |
| Backend | Go 1.25, net/http (stdlib) |
| UI | Bootstrap 5.3 (dark mode) + HTMX 2.0 |
| Web UI access | SigV4 proxy (AWS_IAM), or CloudFront + Cloudflare Access (optional) |
| Database | DynamoDB (single-table, provisioned 2 RCU/2 WCU free-tier, TTL) |
| LLM runtime | Amazon Bedrock (model selectable in Settings) |
| Gmail integration | Google OAuth 2.0 + Gmail REST API |
| Secrets | SSM Parameter Store (SecureString) |
| Deployment | AWS SAM + GitHub Actions (OIDC) |
</content>
