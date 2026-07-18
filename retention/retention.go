package retention

import (
	"context"
	"log/slog"
	"runtime/debug"

	"github.com/sloccy/ollamail-aws/db"
	gmailpkg "github.com/sloccy/ollamail-aws/gmail"
)

const maxRetentionIDs = 2500
const maxPages = 5

// maxRetentionDays caps a rule's day count at 100 years — far beyond any real mailbox.
const maxRetentionDays = 36500

// clampDays bounds a stored retention day-count to [1, maxRetentionDays] before the
// int64->int conversion. The write path validates days > 0, but rows also arrive via
// config import, and DynamoDB stores int64: an unchecked conversion truncates on 32-bit
// platforms (CodeQL go/incorrect-integer-conversion), and a negative value would make
// FetchEmailsOlderThan's before: date land in the future — a query matching every email.
func clampDays(d int64) int {
	if d < 1 {
		return 1
	}
	if d > maxRetentionDays {
		return maxRetentionDays
	}
	return int(d)
}

// Cleanup trashes emails that exceed retention rules for the given account.
func Cleanup(ctx context.Context, store db.StoreIface, svc *gmailpkg.Client, accountID int64) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("retention panic", "account_id", accountID, "err", r, "stack", string(debug.Stack()))
		}
	}()

	if err := cleanup(ctx, store, svc, accountID); err != nil {
		slog.Error("retention error", "account_id", accountID, "err", err)
	}
}

func cleanup(ctx context.Context, store db.StoreIface, svc *gmailpkg.Client, accountID int64) error {
	labelRules, err := store.GetLabelRetention(ctx, accountID)
	if err != nil {
		return err
	}

	exemptions, err := store.GetLabelExemptions(ctx, accountID)
	if err != nil {
		return err
	}
	exemptSet := make(map[string]bool, len(exemptions))
	for _, e := range exemptions {
		exemptSet[e.LabelName] = true
	}

	trashed := make(map[string]bool)

	// Per-label retention rules
	for _, rule := range labelRules {
		if exemptSet[rule.LabelName] {
			continue
		}
		trashOlderThan(ctx, svc, trashed, "label "+rule.LabelName, func() ([]string, error) {
			return gmailpkg.FetchEmailsOlderThan(ctx, svc, clampDays(rule.Days), rule.LabelName, nil, maxPages)
		})
	}

	// Global retention rule
	retention, err := store.GetAccountRetention(ctx, accountID)
	if err != nil {
		return nil //nolint:nilerr // no global rule configured
	}
	if !retention.GlobalDays.Valid {
		return nil
	}

	// Build exclusion list: labels with specific rules + exemptions
	var excludeLabels []string
	for _, rule := range labelRules {
		excludeLabels = append(excludeLabels, rule.LabelName)
	}
	for _, e := range exemptions {
		excludeLabels = append(excludeLabels, e.LabelName)
	}

	trashOlderThan(ctx, svc, trashed, "global", func() ([]string, error) {
		return gmailpkg.FetchEmailsOlderThan(ctx, svc, clampDays(retention.GlobalDays.Int64), "", excludeLabels, maxPages)
	})
	return nil
}

// trashOlderThan pages through fetch (a FetchEmailsOlderThan call bound to one label rule
// or the global rule) and trashes newly-seen ids, deduping against trashed (shared across
// both the per-label and global loops in cleanup, since a message can match more than one
// rule). Stops once maxRetentionIDs total ids have been trashed this run, fetch errors, a
// page yields nothing new, or fetch returns fewer than a full page (no more pages).
// logLabel identifies the rule in error/log output.
func trashOlderThan(ctx context.Context, svc *gmailpkg.Client, trashed map[string]bool, logLabel string, fetch func() ([]string, error)) {
	for len(trashed) < maxRetentionIDs {
		ids, err := fetch()
		if err != nil {
			slog.Error("fetch older emails", "rule", logLabel, "err", err)
			return
		}
		var toTrash []string
		for _, id := range ids {
			if !trashed[id] {
				toTrash = append(toTrash, id)
				trashed[id] = true
			}
		}
		if len(toTrash) == 0 {
			return
		}
		if err := gmailpkg.BatchTrashEmails(ctx, svc, toTrash); err != nil {
			slog.Error("trash emails", "rule", logLabel, "err", err)
		}
		if len(ids) < gmailpkg.PageSize {
			return // no more pages
		}
	}
}
