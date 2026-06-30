-- ============================================================
-- Settings
-- ============================================================

-- name: GetSetting :one
SELECT value FROM settings WHERE key = ? LIMIT 1;

-- name: SetSetting :exec
INSERT INTO settings (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value;

-- name: SeedSetting :exec
INSERT OR IGNORE INTO settings (key, value) VALUES (?, ?);

-- name: GetAllSettings :many
SELECT key, value FROM settings;

-- ============================================================
-- Accounts
-- ============================================================

-- name: ListAccounts :many
SELECT id, email, credentials_json, added_at, last_scan_at, active
FROM accounts
ORDER BY added_at DESC;

-- name: ListAccountsSafe :many
SELECT id, email, added_at, last_scan_at, active
FROM accounts
ORDER BY added_at DESC;

-- name: GetAccount :one
SELECT id, email, credentials_json, added_at, last_scan_at, active
FROM accounts WHERE id = ? LIMIT 1;

-- name: UpsertAccount :one
INSERT INTO accounts (email, credentials_json, active)
VALUES (?, ?, 1)
ON CONFLICT(email) DO UPDATE SET credentials_json = excluded.credentials_json, active = 1
RETURNING id;

-- name: UpdateAccountCredentials :exec
UPDATE accounts SET credentials_json = ? WHERE id = ?;

-- name: UpdateLastScan :exec
UPDATE accounts SET last_scan_at = strftime('%Y-%m-%d %H:%M:%S', 'now') WHERE id = ?;

-- name: ToggleAccount :one
UPDATE accounts SET active = 1 - active WHERE id = ? RETURNING active;

-- name: DeleteAccount :exec
DELETE FROM accounts WHERE id = ?;

-- name: CreateAccountPlaceholder :one
INSERT OR IGNORE INTO accounts (email, credentials_json, active) VALUES (?, '', 1)
RETURNING id;

-- name: GetAccountByEmail :one
SELECT id FROM accounts WHERE email = ? LIMIT 1;

-- ============================================================
-- Prompts
-- ============================================================

-- name: ListPrompts :many
SELECT id, name, instructions, label_name, active, created_at,
       action_archive, action_spam, action_trash, action_mark_read,
       sort_order, stop_processing, account_id
FROM prompts
ORDER BY sort_order ASC, id ASC;

-- name: ListPromptsByAccount :many
SELECT id, name, instructions, label_name, active, created_at,
       action_archive, action_spam, action_trash, action_mark_read,
       sort_order, stop_processing, account_id
FROM prompts
WHERE account_id = ? OR account_id IS NULL
ORDER BY sort_order ASC, id ASC;

-- name: ListActivePrompts :many
SELECT id, name, instructions, label_name, active, created_at,
       action_archive, action_spam, action_trash, action_mark_read,
       sort_order, stop_processing, account_id
FROM prompts
WHERE active = 1
ORDER BY sort_order ASC, id ASC;

-- name: ListActivePromptsByAccount :many
SELECT id, name, instructions, label_name, active, created_at,
       action_archive, action_spam, action_trash, action_mark_read,
       sort_order, stop_processing, account_id
FROM prompts
WHERE active = 1 AND (account_id = ? OR account_id IS NULL)
ORDER BY sort_order ASC, id ASC;

-- name: GetPrompt :one
SELECT id, name, instructions, label_name, active, created_at,
       action_archive, action_spam, action_trash, action_mark_read,
       sort_order, stop_processing, account_id
FROM prompts WHERE id = ? LIMIT 1;

-- name: MaxPromptSortOrder :one
SELECT COALESCE(MAX(sort_order), -1) FROM prompts;

-- name: CreatePrompt :one
INSERT INTO prompts (name, instructions, label_name, active, action_archive, action_spam, action_trash, action_mark_read, sort_order, stop_processing, account_id)
VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: UpdatePrompt :exec
UPDATE prompts SET
    name = ?,
    instructions = ?,
    label_name = ?,
    action_archive = ?,
    action_spam = ?,
    action_trash = ?,
    action_mark_read = ?,
    stop_processing = ?,
    account_id = ?
WHERE id = ?;

-- name: TogglePrompt :one
UPDATE prompts SET active = 1 - active WHERE id = ? RETURNING active;

-- name: UpdatePromptSortOrder :exec
UPDATE prompts SET sort_order = ? WHERE id = ?;

-- name: DeletePrompt :exec
DELETE FROM prompts WHERE id = ?;

-- name: CountActivePrompts :one
SELECT COUNT(*) FROM prompts WHERE active = 1;

-- name: PromptExistsForAccount :one
SELECT EXISTS(SELECT 1 FROM prompts WHERE name = ? AND account_id = ?) AS exists_val;

-- name: PromptExistsGlobal :one
SELECT EXISTS(SELECT 1 FROM prompts WHERE name = ? AND account_id IS NULL) AS exists_val;

-- ============================================================
-- Processed Emails
-- ============================================================

-- name: GetProcessedMessageIDs :many
SELECT message_id FROM processed_emails
WHERE account_id = ? AND message_id IN (/*SLICE:message_ids*/?)
;

-- name: MarkProcessed :exec
INSERT INTO processed_emails (account_id, message_id)
VALUES (?, ?)
ON CONFLICT(account_id, message_id) DO NOTHING;

-- name: TrimProcessedEmails :exec
DELETE FROM processed_emails
WHERE processed_at IS NOT NULL AND processed_at < ?;

-- ============================================================
-- Logs
-- ============================================================

-- name: AddLog :exec
INSERT INTO logs (level, message) VALUES (?, ?);

-- name: GetLogs :many
SELECT id, timestamp, level, message
FROM logs
ORDER BY id DESC
LIMIT ?;

-- name: GetLogsRange :many
SELECT id, timestamp, level, message
FROM logs
WHERE timestamp >= ? AND timestamp <= ?
ORDER BY id ASC;

-- name: TrimLogs :exec
DELETE FROM logs WHERE timestamp < ?;

-- ============================================================
-- Categorization History
-- ============================================================

-- name: AddHistory :exec
INSERT INTO categorization_history
    (account_id, account_email, message_id, subject, sender, prompt_id, prompt_name, label_name, actions, llm_response)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetHistory :many
SELECT id, timestamp, account_id, account_email, message_id, subject, sender,
       prompt_id, prompt_name, label_name, actions, llm_response
FROM categorization_history
ORDER BY id DESC
LIMIT ?;

-- name: GetHistoryByAccount :many
SELECT id, timestamp, account_id, account_email, message_id, subject, sender,
       prompt_id, prompt_name, label_name, actions, llm_response
FROM categorization_history
WHERE account_id = ?
ORDER BY id DESC
LIMIT ?;

-- name: GetHistoryByPrompt :many
SELECT id, timestamp, account_id, account_email, message_id, subject, sender,
       prompt_id, prompt_name, label_name, actions, llm_response
FROM categorization_history
WHERE prompt_id = ?
ORDER BY id DESC
LIMIT ?;

-- name: GetHistoryUncategorized :many
SELECT id, timestamp, account_id, account_email, message_id, subject, sender,
       prompt_id, prompt_name, label_name, actions, llm_response
FROM categorization_history
WHERE prompt_id IS NULL
ORDER BY id DESC
LIMIT ?;

-- name: TrimHistory :exec
DELETE FROM categorization_history WHERE timestamp < ?;

-- ============================================================
-- Retention
-- ============================================================

-- name: GetAccountRetention :one
SELECT account_id, global_days FROM account_retention WHERE account_id = ? LIMIT 1;

-- name: SetGlobalRetention :exec
INSERT INTO account_retention (account_id, global_days) VALUES (?, ?)
ON CONFLICT(account_id) DO UPDATE SET global_days = excluded.global_days;

-- name: ClearGlobalRetention :exec
DELETE FROM account_retention WHERE account_id = ?;

-- name: HasGlobalRetention :one
SELECT EXISTS(SELECT 1 FROM account_retention WHERE account_id = ?) AS exists_val;

-- name: GetLabelRetention :many
SELECT id, account_id, label_name, days FROM label_retention
WHERE account_id = ? ORDER BY id ASC;

-- name: AddLabelRetention :exec
INSERT INTO label_retention (account_id, label_name, days) VALUES (?, ?, ?)
ON CONFLICT(account_id, label_name) DO UPDATE SET days = excluded.days;

-- name: DeleteLabelRetention :exec
DELETE FROM label_retention WHERE id = ? AND account_id = ?;

-- name: LabelRetentionExists :one
SELECT EXISTS(SELECT 1 FROM label_retention WHERE account_id = ? AND label_name = ?) AS exists_val;

-- name: GetLabelExemptions :many
SELECT id, account_id, label_name FROM label_exemptions
WHERE account_id = ? ORDER BY label_name ASC;

-- name: AddLabelExemption :exec
INSERT INTO label_exemptions (account_id, label_name) VALUES (?, ?)
ON CONFLICT(account_id, label_name) DO NOTHING;

-- name: DeleteLabelExemption :exec
DELETE FROM label_exemptions WHERE id = ? AND account_id = ?;

-- ============================================================
-- Cascade delete helpers (called manually in a transaction)
-- ============================================================

-- name: DeletePromptsByAccount :exec
DELETE FROM prompts WHERE account_id = ?;

-- name: DeleteHistoryByAccount :exec
DELETE FROM categorization_history WHERE account_id = ?;

-- name: DeleteAccountRetention :exec
DELETE FROM account_retention WHERE account_id = ?;

-- name: DeleteLabelRetentionByAccount :exec
DELETE FROM label_retention WHERE account_id = ?;

-- name: DeleteLabelExemptionsByAccount :exec
DELETE FROM label_exemptions WHERE account_id = ?;

-- name: DeleteProcessedEmailsByAccount :exec
DELETE FROM processed_emails WHERE account_id = ?;

-- ============================================================
-- Categorization History (additional)
-- ============================================================

-- name: GetHistoryRow :one
SELECT id, timestamp, account_id, account_email, message_id, subject, sender,
       prompt_id, prompt_name, label_name, actions, llm_response
FROM categorization_history WHERE id = ? LIMIT 1;

-- name: GetPromptIDsByMessageID :many
SELECT DISTINCT prompt_id FROM categorization_history
WHERE message_id = ? AND prompt_id IS NOT NULL;

-- ============================================================
-- Email Corrections
-- ============================================================

-- name: InsertEmailCorrection :one
INSERT INTO email_corrections (account_id, message_id, added_prompts, removed_prompts, current_prompt_ids, note)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: GetLatestCorrectionForMessage :one
SELECT id, created_at, account_id, message_id, added_prompts, removed_prompts, current_prompt_ids, note
FROM email_corrections
WHERE message_id = ?
ORDER BY id DESC
LIMIT 1;

-- ============================================================
-- Prompt Suggestions
-- ============================================================

-- name: InsertPromptSuggestion :one
INSERT INTO prompt_suggestions
    (prompt_id, correction_id, trigger_kind, message_id, email_subject, email_sender, email_body_snapshot, original_instructions, suggested_instructions, conversation_json, status)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: GetPromptSuggestion :one
SELECT id, created_at, updated_at, prompt_id, correction_id, trigger_kind,
       message_id, email_subject, email_sender, email_body_snapshot,
       original_instructions, suggested_instructions, conversation_json,
       user_comment, status
FROM prompt_suggestions WHERE id = ? LIMIT 1;

-- name: ListPromptSuggestions :many
SELECT id, created_at, updated_at, prompt_id, correction_id, trigger_kind,
       message_id, email_subject, email_sender, email_body_snapshot,
       original_instructions, suggested_instructions, conversation_json,
       user_comment, status
FROM prompt_suggestions
WHERE status NOT IN ('applied', 'dismissed')
ORDER BY CASE status WHEN 'generating' THEN 0 WHEN 'pending' THEN 1 ELSE 2 END ASC, id DESC;

-- name: UpdatePromptSuggestion :exec
UPDATE prompt_suggestions SET
    suggested_instructions = ?,
    conversation_json = ?,
    user_comment = ?,
    status = 'pending',
    updated_at = strftime('%Y-%m-%d %H:%M:%S', 'now')
WHERE id = ?;

-- name: DismissPromptSuggestion :exec
UPDATE prompt_suggestions SET status = 'dismissed', updated_at = strftime('%Y-%m-%d %H:%M:%S', 'now')
WHERE id = ?;

-- name: ApplyPromptSuggestion :exec
UPDATE prompt_suggestions SET status = 'applied', updated_at = strftime('%Y-%m-%d %H:%M:%S', 'now')
WHERE id = ?;

-- name: FinalizePromptSuggestion :exec
UPDATE prompt_suggestions SET
    suggested_instructions = ?,
    conversation_json      = ?,
    status                 = ?,
    user_comment           = ?,
    updated_at             = strftime('%Y-%m-%d %H:%M:%S', 'now')
WHERE id = ?;

-- name: CountPendingPromptSuggestions :one
SELECT COUNT(*) FROM prompt_suggestions WHERE status = 'pending';

-- name: UpdatePromptInstructions :exec
UPDATE prompts SET instructions = ? WHERE id = ?;

-- ============================================================
-- LLM Debug
-- ============================================================

-- name: AddLlmDebug :exec
INSERT INTO llm_debug (account_id, account_email, message_id, subject, sender, gmail_raw, llm_request, llm_response)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetLatestLlmDebug :many
SELECT id, timestamp, account_id, account_email, message_id, subject, sender, gmail_raw, llm_request, llm_response
FROM llm_debug ORDER BY id DESC LIMIT 3;

-- name: TrimLlmDebug :exec
DELETE FROM llm_debug WHERE id NOT IN (SELECT id FROM llm_debug ORDER BY id DESC LIMIT 3);

-- name: DeleteIncompleteLlmDebug :exec
DELETE FROM llm_debug WHERE gmail_raw = '' OR llm_request = '';

-- ============================================================
-- Schema version
-- ============================================================

-- name: GetSchemaVersion :one
SELECT version FROM schema_version LIMIT 1;

-- name: SetSchemaVersion :exec
UPDATE schema_version SET version = ?;
