package db

import "context"

// StoreIface is the subset of Store actually invoked through an interface-typed value —
// by processor.ProcessAccount/ProcessAccountHistory (the scan/push path) and
// retention.Cleanup. Everything else processor/retention touch (UpsertAccount,
// AddLabelRetention, GetHistoryFiltered, ...) is called on the concrete *db.FakeStore
// directly in test setup, never through this interface, so it doesn't belong here — a
// wider interface than what's actually dispatched through it only forces FakeStore to carry
// stub methods for nothing (see db/fake.go's ListActivePrompts before this trim).
// *Store and *FakeStore both satisfy this; see the var _ assertions in store.go/fake.go.
type StoreIface interface {
	Log(level, message string)

	UpdateAccountCredentials(ctx context.Context, arg UpdateAccountCredentialsParams) error
	UpdateLastScan(ctx context.Context, id int64) error

	FilterUnprocessed(ctx context.Context, accountID int64, messageIDs []string) ([]string, error)
	ClaimMessages(ctx context.Context, accountID int64, messageIDs []string) ([]string, error)
	ReleaseClaim(ctx context.Context, accountID int64, messageID string) error
	BatchInsertProcessingResults(ctx context.Context, r ProcessingResults) error
	RecordLlmDebug(ctx context.Context, e AddLlmDebugParams) error

	GetLabelRetention(ctx context.Context, accountID int64) ([]LabelRetention, error)
	GetLabelExemptions(ctx context.Context, accountID int64) ([]LabelExemption, error)
	GetAccountRetention(ctx context.Context, accountID int64) (AccountRetention, error)
}
