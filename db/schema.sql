CREATE TABLE IF NOT EXISTS accounts (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    email            TEXT NOT NULL UNIQUE,
    credentials_json TEXT NOT NULL DEFAULT '',
    added_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%S', 'now')),
    last_scan_at     TEXT,
    active           INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS prompts (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    name             TEXT NOT NULL,
    instructions     TEXT NOT NULL DEFAULT '',
    label_name       TEXT NOT NULL DEFAULT '',
    active           INTEGER NOT NULL DEFAULT 1,
    created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%S', 'now')),
    action_archive   INTEGER NOT NULL DEFAULT 0,
    action_spam      INTEGER NOT NULL DEFAULT 0,
    action_trash     INTEGER NOT NULL DEFAULT 0,
    action_mark_read INTEGER NOT NULL DEFAULT 0,
    sort_order       INTEGER NOT NULL DEFAULT 0,
    stop_processing  INTEGER NOT NULL DEFAULT 0,
    account_id       INTEGER
);

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS processed_emails (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id   INTEGER NOT NULL,
    message_id   TEXT NOT NULL,
    processed_at TEXT DEFAULT (strftime('%Y-%m-%d %H:%M:%S', 'now')),
    UNIQUE(account_id, message_id)
);

CREATE TABLE IF NOT EXISTS logs (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%S', 'now')),
    level     TEXT NOT NULL,
    message   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS categorization_history (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%S', 'now')),
    account_id    INTEGER NOT NULL,
    account_email TEXT NOT NULL DEFAULT '',
    message_id    TEXT NOT NULL DEFAULT '',
    subject       TEXT NOT NULL DEFAULT '',
    sender        TEXT NOT NULL DEFAULT '',
    prompt_id     INTEGER,
    prompt_name   TEXT,
    label_name    TEXT,
    actions       TEXT NOT NULL DEFAULT '',
    llm_response  TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS account_retention (
    account_id  INTEGER PRIMARY KEY,
    global_days INTEGER
);

CREATE TABLE IF NOT EXISTS label_retention (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id INTEGER NOT NULL,
    label_name TEXT NOT NULL,
    days       INTEGER NOT NULL,
    UNIQUE(account_id, label_name)
);

CREATE TABLE IF NOT EXISTS label_exemptions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id INTEGER NOT NULL,
    label_name TEXT NOT NULL,
    UNIQUE(account_id, label_name)
);

CREATE TABLE IF NOT EXISTS email_corrections (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%S', 'now')),
    account_id       INTEGER NOT NULL,
    message_id       TEXT NOT NULL,
    added_prompts    TEXT NOT NULL DEFAULT '',
    removed_prompts  TEXT NOT NULL DEFAULT '',
    current_prompt_ids TEXT NOT NULL DEFAULT '',
    note             TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS prompt_suggestions (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at            TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%S', 'now')),
    updated_at            TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%S', 'now')),
    prompt_id             INTEGER NOT NULL,
    correction_id         INTEGER,
    trigger_kind          TEXT NOT NULL DEFAULT 'false_negative',
    message_id            TEXT NOT NULL DEFAULT '',
    email_subject         TEXT NOT NULL DEFAULT '',
    email_sender          TEXT NOT NULL DEFAULT '',
    email_body_snapshot   TEXT NOT NULL DEFAULT '',
    original_instructions TEXT NOT NULL DEFAULT '',
    suggested_instructions TEXT NOT NULL DEFAULT '',
    conversation_json     TEXT NOT NULL DEFAULT '[]',
    user_comment          TEXT NOT NULL DEFAULT '',
    status                TEXT NOT NULL DEFAULT 'pending'
);

CREATE TABLE IF NOT EXISTS llm_debug (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%S', 'now')),
    account_id    INTEGER NOT NULL,
    account_email TEXT NOT NULL DEFAULT '',
    message_id    TEXT NOT NULL DEFAULT '',
    subject       TEXT NOT NULL DEFAULT '',
    sender        TEXT NOT NULL DEFAULT '',
    gmail_raw     TEXT NOT NULL DEFAULT '',
    llm_request   TEXT NOT NULL DEFAULT '',
    llm_response  TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER NOT NULL DEFAULT 0
);
