package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // register SQLite driver
)

// Store wraps the sqlc Queries with a *sql.DB and adds helpers
// that require transactions or dynamic SQL (cascade delete, reorder, etc.).
type Store struct {
	*Queries
	db *sql.DB
}

func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_journal_mode=wal&_busy_timeout=5000&_foreign_keys=off", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite WAL: single writer
	// Reduce SQLite page cache from default ~2MB to 512KB and disable mmap
	for _, pragma := range []string{"PRAGMA cache_size = -512", "PRAGMA mmap_size = 0"} {
		if _, err := db.ExecContext(context.Background(), pragma); err != nil {
			return nil, fmt.Errorf("sqlite pragma: %w", err)
		}
	}
	if err := db.PingContext(context.Background()); err != nil {
		return nil, err
	}
	return &Store{Queries: New(db), db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// Now returns the current UTC time in the format used for all TEXT timestamps.
func Now() string {
	return time.Now().UTC().Format("2006-01-02 15:04:05")
}

// ============================================================
// Migrations
// ============================================================

func (s *Store) Migrate() error {
	ctx := context.Background()

	// Ensure schema_version row exists
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO schema_version (version) VALUES (0)`)
	if err != nil {
		// Table may not exist yet; create schema first
		if err2 := s.createSchema(ctx); err2 != nil {
			return fmt.Errorf("create schema: %w", err2)
		}
		if _, err3 := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO schema_version (version) VALUES (0)`); err3 != nil {
			return fmt.Errorf("seed schema_version: %w", err3)
		}
	}

	ver, err := s.GetSchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("get schema version: %w", err)
	}

	migrations := []func(context.Context) error{
		s.migration001,
		s.migration002,
		s.migration003,
	}

	for i, m := range migrations {
		if int64(i) < ver {
			continue
		}
		if err := m(ctx); err != nil {
			return fmt.Errorf("migration %03d: %w", i+1, err)
		}
		if err := s.SetSchemaVersion(ctx, int64(i+1)); err != nil {
			return fmt.Errorf("update schema version: %w", err)
		}
	}
	return nil
}

func (s *Store) createSchema(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, schemaDDL)
	return err
}

// migration001 is a no-op for fresh Go installs; for Python-migrated DBs,
// the llm_response column already exists. We just ensure it's present.
func (s *Store) migration001(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`ALTER TABLE categorization_history ADD COLUMN llm_response TEXT NOT NULL DEFAULT ''`)
	if err != nil && !isSQLiteAlreadyExists(err) {
		return err
	}
	return nil
}

// migration002 adds email_corrections and prompt_suggestions tables.
func (s *Store) migration002(ctx context.Context) error {
	ddls := []string{
		`CREATE TABLE IF NOT EXISTS email_corrections (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%S', 'now')),
			account_id       INTEGER NOT NULL,
			message_id       TEXT NOT NULL,
			added_prompts    TEXT NOT NULL DEFAULT '',
			removed_prompts  TEXT NOT NULL DEFAULT '',
			current_prompt_ids TEXT NOT NULL DEFAULT '',
			note             TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS prompt_suggestions (
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
		)`,
	}
	for _, ddl := range ddls {
		if _, err := s.db.ExecContext(ctx, ddl); err != nil {
			return err
		}
	}
	return nil
}

// migration003 adds the llm_debug table for troubleshooting diagnostics.
func (s *Store) migration003(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS llm_debug (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%S','now')),
		account_id    INTEGER NOT NULL,
		account_email TEXT NOT NULL DEFAULT '',
		message_id    TEXT NOT NULL DEFAULT '',
		subject       TEXT NOT NULL DEFAULT '',
		sender        TEXT NOT NULL DEFAULT '',
		gmail_raw     TEXT NOT NULL DEFAULT '',
		llm_request   TEXT NOT NULL DEFAULT '',
		llm_response  TEXT NOT NULL DEFAULT ''
	)`)
	return err
}

func isSQLiteAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "duplicate column name") ||
		strings.Contains(err.Error(), "already exists")
}

//go:embed schema.sql
var schemaDDL string

// ============================================================
// Secret key
// ============================================================

func (s *Store) GetOrCreateSecretKey() ([]byte, error) {
	ctx := context.Background()
	val, err := s.GetSetting(ctx, "secret_key")
	if err == nil {
		b, e := hex.DecodeString(val)
		if e == nil {
			return b, nil
		}
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := s.Queries.SetSetting(ctx, SetSettingParams{Key: "secret_key", Value: hex.EncodeToString(key)}); err != nil {
		return nil, err
	}
	return key, nil
}

// ============================================================
// Seed
// ============================================================

func (s *Store) SeedSetting(key, value string) error {
	return s.Queries.SeedSetting(context.Background(), SeedSettingParams{Key: key, Value: value})
}

// ============================================================
// Logs helper
// ============================================================

func (s *Store) Log(level, message string) {
	_ = s.AddLog(context.Background(), AddLogParams{Level: level, Message: message})
}

// ============================================================
// Account cascade delete (transaction)
// ============================================================

func (s *Store) DeleteAccountCascade(ctx context.Context, accountID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	q := s.WithTx(tx)
	for _, fn := range []func() error{
		func() error { return q.DeletePromptsByAccount(ctx, sql.NullInt64{Int64: accountID, Valid: true}) },
		func() error { return q.DeleteHistoryByAccount(ctx, accountID) },
		func() error { return q.DeleteAccountRetention(ctx, accountID) },
		func() error { return q.DeleteLabelRetentionByAccount(ctx, accountID) },
		func() error { return q.DeleteLabelExemptionsByAccount(ctx, accountID) },
		func() error { return q.DeleteProcessedEmailsByAccount(ctx, accountID) },
		func() error { return q.DeleteAccount(ctx, accountID) },
	} {
		if err := fn(); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ============================================================
// Prompt reorder (transaction)
// ============================================================

func (s *Store) ReorderPrompts(ctx context.Context, ids []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	q := s.WithTx(tx)
	for i, id := range ids {
		if err := q.UpdatePromptSortOrder(ctx, UpdatePromptSortOrderParams{SortOrder: int64(i), ID: id}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ============================================================
// FilterUnprocessed — returns subset of messageIDs not yet processed
// ============================================================

func (s *Store) FilterUnprocessed(ctx context.Context, accountID int64, messageIDs []string) ([]string, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}

	// Build query with IN clause
	var qb strings.Builder
	qb.WriteString(`SELECT message_id FROM processed_emails WHERE account_id = ? AND message_id IN (`)
	args := make([]any, 0, len(messageIDs)+1)
	args = append(args, accountID)
	for i, id := range messageIDs {
		if i > 0 {
			qb.WriteByte(',')
		}
		qb.WriteByte('?')
		args = append(args, id)
	}
	qb.WriteByte(')')
	query := qb.String()

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	processed := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		processed[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var unprocessed []string
	for _, id := range messageIDs {
		if !processed[id] {
			unprocessed = append(unprocessed, id)
		}
	}
	return unprocessed, nil
}

// ============================================================
// History with dynamic filters
// ============================================================

type HistoryFilter struct {
	AccountID *int64
	PromptID  *int64
	Unmatched bool
	SubjectQ  string
	SenderQ   string
	Limit     int64
}

func (s *Store) GetHistoryFiltered(ctx context.Context, f HistoryFilter) ([]CategorizationHistory, error) {
	// Omit llm_response from list query; use HasLlmResponse to indicate availability.
	// Full response is fetched on demand via GetHistoryLlmResponse.
	query := `SELECT id, timestamp, account_id, account_email, message_id, subject, sender,
		prompt_id, prompt_name, label_name, actions, (llm_response != '') AS has_llm_response
		FROM categorization_history WHERE 1=1`
	args := []any{}

	if f.AccountID != nil {
		query += " AND account_id = ?"
		args = append(args, *f.AccountID)
	}
	if f.Unmatched {
		query += " AND prompt_id IS NULL"
	} else if f.PromptID != nil {
		query += " AND prompt_id = ?"
		args = append(args, *f.PromptID)
	}
	if f.SubjectQ != "" {
		query += " AND subject LIKE ?"
		args = append(args, "%"+f.SubjectQ+"%")
	}
	if f.SenderQ != "" {
		query += " AND sender LIKE ?"
		args = append(args, "%"+f.SenderQ+"%")
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, f.Limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []CategorizationHistory
	for rows.Next() {
		var r CategorizationHistory
		var hasLlm bool
		if err := rows.Scan(
			&r.ID, &r.Timestamp, &r.AccountID, &r.AccountEmail,
			&r.MessageID, &r.Subject, &r.Sender,
			&r.PromptID, &r.PromptName, &r.LabelName,
			&r.Actions, &hasLlm,
		); err != nil {
			return nil, err
		}
		if hasLlm {
			r.LlmResponse = "1" // sentinel: response exists, not yet loaded
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// GetHistoryLlmResponse fetches only the llm_response text for a single history row.
func (s *Store) GetHistoryLlmResponse(ctx context.Context, id int64) (string, error) {
	var resp string
	err := s.db.QueryRowContext(ctx, `SELECT llm_response FROM categorization_history WHERE id = ?`, id).Scan(&resp)
	return resp, err
}

// ============================================================
// Batch insert helpers for email processing
// ============================================================

type LogEntry struct {
	Level   string
	Message string
}

type HistoryEntry struct {
	AccountID    int64
	AccountEmail string
	MessageID    string
	Subject      string
	Sender       string
	PromptID     sql.NullInt64
	PromptName   sql.NullString
	LabelName    sql.NullString
	Actions      string
	LlmResponse  string
}

func (s *Store) BatchInsertProcessingResults(ctx context.Context, logs []LogEntry, history []HistoryEntry, accountID int64, messageID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	q := s.WithTx(tx)

	for _, l := range logs {
		if err := q.AddLog(ctx, AddLogParams(l)); err != nil {
			return err
		}
	}
	for _, h := range history {
		if err := q.AddHistory(ctx, AddHistoryParams(h)); err != nil {
			return err
		}
	}
	if err := q.MarkProcessed(ctx, MarkProcessedParams{AccountID: accountID, MessageID: messageID}); err != nil {
		return err
	}
	return tx.Commit()
}

// ============================================================
// Prompt suggestion helpers
// ============================================================

// ApplyPromptSuggestionAndUpdatePrompt updates prompt.instructions and marks the suggestion
// as applied in a single transaction.
func (s *Store) ApplyPromptSuggestionAndUpdatePrompt(ctx context.Context, suggestionID int64, promptID int64, newInstructions string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	q := s.WithTx(tx)
	if err := q.UpdatePromptInstructions(ctx, UpdatePromptInstructionsParams{
		Instructions: newInstructions,
		ID:           promptID,
	}); err != nil {
		return err
	}
	if err := q.ApplyPromptSuggestion(ctx, suggestionID); err != nil {
		return err
	}
	return tx.Commit()
}

// GetCurrentPromptIDsForMessage returns the set of prompt IDs currently applied
// to the given message, derived directly from categorization_history.
func (s *Store) GetCurrentPromptIDsForMessage(ctx context.Context, messageID string) (map[int64]bool, error) {
	nullIDs, err := s.GetPromptIDsByMessageID(ctx, messageID)
	if err != nil {
		return nil, err
	}
	set := make(map[int64]bool)
	for _, nid := range nullIDs {
		if nid.Valid {
			set[nid.Int64] = true
		}
	}
	return set, nil
}

// RewriteHistoryForMessage rewrites categorization_history for the given message
// so it mirrors the post-correction labeling state. Rows for keptIDs are preserved;
// all other rows for the message are deleted. Fresh rows are inserted for each prompt
// in addedPrompts. If the result would be zero rows, a "no match" sentinel is inserted
// so the message still appears in history.
func (s *Store) RewriteHistoryForMessage(ctx context.Context, messageID string, keptIDs []int64, addedPrompts []Prompt, base CategorizationHistory) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if len(keptIDs) == 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM categorization_history WHERE message_id = ?`, messageID); err != nil {
			return err
		}
	} else {
		placeholders := strings.Repeat("?,", len(keptIDs))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, 0, 1+len(keptIDs))
		args = append(args, messageID)
		for _, id := range keptIDs {
			args = append(args, id)
		}
		q := fmt.Sprintf( //nolint:gosec // placeholders built from len(keptIDs), not user input
			`DELETE FROM categorization_history WHERE message_id = ? AND (prompt_id IS NULL OR prompt_id NOT IN (%s))`,
			placeholders,
		)
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return err
		}
	}

	qw := s.WithTx(tx)
	for _, p := range addedPrompts {
		if err := qw.AddHistory(ctx, AddHistoryParams{
			AccountID:    base.AccountID,
			AccountEmail: base.AccountEmail,
			MessageID:    messageID,
			Subject:      base.Subject,
			Sender:       base.Sender,
			PromptID:     sql.NullInt64{Int64: p.ID, Valid: true},
			PromptName:   sql.NullString{String: p.Name, Valid: true},
			LabelName:    sql.NullString{String: p.LabelName, Valid: p.LabelName != ""},
			Actions:      "manual",
		}); err != nil {
			return err
		}
	}

	// Ensure a sentinel row exists if everything was removed
	if len(keptIDs) == 0 && len(addedPrompts) == 0 {
		if err := qw.AddHistory(ctx, AddHistoryParams{
			AccountID:    base.AccountID,
			AccountEmail: base.AccountEmail,
			MessageID:    messageID,
			Subject:      base.Subject,
			Sender:       base.Sender,
		}); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// ============================================================
// LLM Debug helpers
// ============================================================

func (s *Store) RecordLlmDebug(ctx context.Context, e AddLlmDebugParams) error {
	if err := s.AddLlmDebug(ctx, e); err != nil {
		return err
	}
	return s.TrimLlmDebug(ctx)
}

// ============================================================
// Trim helpers
// ============================================================

func (s *Store) TrimLogs(ctx context.Context, retentionDays int) error {
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays).Format("2006-01-02 15:04:05")
	return s.Queries.TrimLogs(ctx, cutoff)
}

func (s *Store) TrimProcessedEmails(ctx context.Context, lookbackHours int) error {
	cutoff := time.Now().UTC().Add(-time.Duration(lookbackHours*2) * time.Hour).Format("2006-01-02 15:04:05")
	return s.Queries.TrimProcessedEmails(ctx, sql.NullString{String: cutoff, Valid: true})
}

func (s *Store) TrimHistory(ctx context.Context, retentionDays int) error {
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays).Format("2006-01-02 15:04:05")
	return s.Queries.TrimHistory(ctx, cutoff)
}
