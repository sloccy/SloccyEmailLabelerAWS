package db

import "context"

// StoreIface is the subset of Store used by processor, poller, retention, and
// their tests. *Store satisfies this interface; *FakeStore satisfies it for tests.
type StoreIface interface {
	Log(level, message string)

	GetSetting(ctx context.Context, key string) (string, error)

	ListAccounts(ctx context.Context) ([]Account, error)
	ListActivePrompts(ctx context.Context) ([]Prompt, error)

	UpsertAccount(ctx context.Context, arg UpsertAccountParams) (int64, error)
	GetAccount(ctx context.Context, id int64) (Account, error)
	ToggleAccount(ctx context.Context, id int64) (int64, error)
	UpdateAccountCredentials(ctx context.Context, arg UpdateAccountCredentialsParams) error
	UpdateLastScan(ctx context.Context, id int64) error

	FilterUnprocessed(ctx context.Context, accountID int64, messageIDs []string) ([]string, error)
	ClaimMessages(ctx context.Context, accountID int64, messageIDs []string) ([]string, error)
	ReleaseClaim(ctx context.Context, accountID int64, messageID string) error
	BatchInsertProcessingResults(ctx context.Context, logs []LogEntry, history []HistoryEntry, examples []PromptExample, accountID int64, messageID string) error
	RecordLlmDebug(ctx context.Context, e AddLlmDebugParams) error

	GetHistoryFiltered(ctx context.Context, f HistoryFilter) ([]CategorizationHistory, error)

	GetLabelRetention(ctx context.Context, accountID int64) ([]LabelRetention, error)
	AddLabelRetention(ctx context.Context, arg AddLabelRetentionParams) error
	GetLabelExemptions(ctx context.Context, accountID int64) ([]LabelExemption, error)
	AddLabelExemption(ctx context.Context, arg AddLabelExemptionParams) error
	GetAccountRetention(ctx context.Context, accountID int64) (AccountRetention, error)
	SetGlobalRetention(ctx context.Context, arg SetGlobalRetentionParams) error
}
