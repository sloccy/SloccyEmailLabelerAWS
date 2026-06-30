package retention

import (
	"context"
	"log/slog"
	"runtime/debug"

	"github.com/sloccy/ollamail/db"
	gmailpkg "github.com/sloccy/ollamail/gmail"
)

const maxRetentionIDs = 2500
const maxPages = 5

// Cleanup trashes emails that exceed retention rules for the given account.
func Cleanup(ctx context.Context, store *db.Store, svc *gmailpkg.Client, accountID int64) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("retention panic", "account_id", accountID, "err", r, "stack", string(debug.Stack()))
		}
	}()

	if err := cleanup(ctx, store, svc, accountID); err != nil {
		slog.Error("retention error", "account_id", accountID, "err", err)
	}
}

func cleanup(ctx context.Context, store *db.Store, svc *gmailpkg.Client, accountID int64) error {
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
		for len(trashed) < maxRetentionIDs {
			ids, err := gmailpkg.FetchEmailsOlderThan(ctx, svc, int(rule.Days), rule.LabelName, nil, maxPages)
			if err != nil {
				slog.Error("fetch older emails", "label", rule.LabelName, "err", err)
				break
			}
			var toTrash []string
			for _, id := range ids {
				if !trashed[id] {
					toTrash = append(toTrash, id)
					trashed[id] = true
				}
			}
			if len(toTrash) == 0 {
				break
			}
			if err := gmailpkg.BatchTrashEmails(ctx, svc, toTrash); err != nil {
				slog.Error("trash emails", "label", rule.LabelName, "err", err)
			}
			if len(ids) < 500 {
				break // no more pages
			}
		}
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

	for len(trashed) < maxRetentionIDs {
		ids, err := gmailpkg.FetchEmailsOlderThan(ctx, svc, int(retention.GlobalDays.Int64), "", excludeLabels, maxPages)
		if err != nil {
			slog.Error("fetch older emails (global)", "err", err)
			break
		}
		var toTrash []string
		for _, id := range ids {
			if !trashed[id] {
				toTrash = append(toTrash, id)
				trashed[id] = true
			}
		}
		if len(toTrash) == 0 {
			break
		}
		if err := gmailpkg.BatchTrashEmails(ctx, svc, toTrash); err != nil {
			slog.Error("trash emails (global)", "err", err)
		}
		if len(ids) < 500 {
			break
		}
	}
	return nil
}
