package db

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// FakeStore is an in-memory StoreIface implementation for tests.
// It is safe for concurrent use.
type FakeStore struct {
	mu sync.Mutex

	accounts  map[int64]*Account
	emailToID map[string]int64
	settings  map[string]string
	history   []*CategorizationHistory
	examples  []*PromptExample
	// processed mirrors the DynamoDB "PROC#" item's state machine: no entry means never
	// claimed, a zero time.Time means confirmed (matches "no leaseExp attribute" in
	// production), and a non-zero time is a claim's lease expiry.
	processed   map[int64]map[string]time.Time
	labelRet    map[int64][]LabelRetention
	labelExempt map[int64][]LabelExemption
	retention   map[int64]AccountRetention
	// suggestions and suggestionClaimed back InsertPromptSuggestion/ClaimPromptSuggestion —
	// enough of PromptSuggestion for db/claim_test.go's ClaimPromptSuggestion tests, not a
	// full mirror of the real Store's suggestion methods (FinalizePromptSuggestion, etc.
	// aren't needed on FakeStore; server.go uses the concrete *Store directly).
	suggestions      map[int64]*PromptSuggestion
	suggestionClaims map[int64]bool

	counters map[string]int64
}

func NewFake() *FakeStore {
	return &FakeStore{
		accounts:         make(map[int64]*Account),
		emailToID:        make(map[string]int64),
		settings:         make(map[string]string),
		processed:        make(map[int64]map[string]time.Time),
		labelRet:         make(map[int64][]LabelRetention),
		labelExempt:      make(map[int64][]LabelExemption),
		retention:        make(map[int64]AccountRetention),
		suggestions:      make(map[int64]*PromptSuggestion),
		suggestionClaims: make(map[int64]bool),
		counters:         make(map[string]int64),
	}
}

func (s *FakeStore) nextID(entity string) int64 {
	s.counters[entity]++
	return s.counters[entity]
}

func (s *FakeStore) Log(_, _ string) {}

func (s *FakeStore) GetSetting(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.settings[key]
	if !ok {
		return "", fmt.Errorf("setting %q not found", key)
	}
	return v, nil
}

func (s *FakeStore) ListAccounts(_ context.Context) ([]Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Account, 0, len(s.accounts))
	for _, acc := range s.accounts {
		result = append(result, *acc)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (s *FakeStore) ListActivePrompts(_ context.Context) ([]Prompt, error) {
	return nil, nil
}

func (s *FakeStore) UpsertAccount(_ context.Context, arg UpsertAccountParams) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.emailToID[arg.Email]; ok {
		acc := s.accounts[id]
		if arg.CredentialsJSON != "" {
			acc.CredentialsJSON = arg.CredentialsJSON
		}
		return id, nil
	}
	id := s.nextID("account")
	s.emailToID[arg.Email] = id
	s.accounts[id] = &Account{
		ID:              id,
		Email:           arg.Email,
		CredentialsJSON: arg.CredentialsJSON,
		AddedAt:         Now(),
		Active:          1,
	}
	return id, nil
}

func (s *FakeStore) GetAccount(_ context.Context, id int64) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc, ok := s.accounts[id]
	if !ok {
		return Account{}, fmt.Errorf("account %d not found", id)
	}
	return *acc, nil
}

func (s *FakeStore) ToggleAccount(_ context.Context, id int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc, ok := s.accounts[id]
	if !ok {
		return 0, fmt.Errorf("account %d not found", id)
	}
	if acc.Active == 0 {
		acc.Active = 1
	} else {
		acc.Active = 0
	}
	return acc.Active, nil
}

func (s *FakeStore) UpdateAccountCredentials(_ context.Context, arg UpdateAccountCredentialsParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc, ok := s.accounts[arg.ID]
	if !ok {
		return fmt.Errorf("account %d not found", arg.ID)
	}
	acc.CredentialsJSON = arg.CredentialsJSON
	return nil
}

func (s *FakeStore) UpdateLastScan(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc, ok := s.accounts[id]
	if !ok {
		return fmt.Errorf("account %d not found", id)
	}
	t := Now()
	acc.LastScanAt = &t
	return nil
}

func (s *FakeStore) FilterUnprocessed(_ context.Context, accountID int64, messageIDs []string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pm := s.processed[accountID]
	now := time.Now()
	var out []string
	for _, id := range messageIDs {
		exp, ok := pm[id]
		if !ok || (!exp.IsZero() && !exp.After(now)) {
			out = append(out, id) // never seen, or an expired (reclaimable) lease
		}
	}
	return out, nil
}

// ClaimMessages mirrors Store.ClaimMessages: wins each id whose entry is absent, an
// expired lease, or — matching ClaimMessages' actual condition, which only checks the
// lease, not confirmation — never overwrites a confirmed (zero-time) entry.
func (s *FakeStore) ClaimMessages(_ context.Context, accountID int64, messageIDs []string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.processed[accountID] == nil {
		s.processed[accountID] = make(map[string]time.Time)
	}
	pm := s.processed[accountID]
	now := time.Now()
	claimed := make([]string, 0, len(messageIDs))
	for _, id := range messageIDs {
		exp, ok := pm[id]
		if ok && (exp.IsZero() || exp.After(now)) {
			continue // confirmed, or an active lease owned by someone else
		}
		pm[id] = now.Add(claimLeaseSeconds * time.Second)
		claimed = append(claimed, id)
	}
	return claimed, nil
}

// ReleaseClaim mirrors Store.ReleaseClaim: only removes a live claim, never a confirmed
// (zero-time) marker.
func (s *FakeStore) ReleaseClaim(_ context.Context, accountID int64, messageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	pm := s.processed[accountID]
	if pm == nil {
		return nil
	}
	if exp, ok := pm[messageID]; ok && !exp.IsZero() {
		delete(pm, messageID)
	}
	return nil
}

func (s *FakeStore) BatchInsertProcessingResults(_ context.Context, _ []LogEntry, history []HistoryEntry, examples []PromptExample, accountID int64, messageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range history {
		entry := &CategorizationHistory{
			ID:           s.nextID("history"),
			Timestamp:    Now(),
			AccountID:    h.AccountID,
			AccountEmail: h.AccountEmail,
			MessageID:    h.MessageID,
			Subject:      h.Subject,
			Sender:       h.Sender,
			PromptID:     h.PromptID,
			PromptName:   h.PromptName,
			LabelName:    h.LabelName,
			Actions:      h.Actions,
			LlmResponse:  h.LlmResponse,
			DurationMs:   h.DurationMs,
		}
		s.history = append(s.history, entry)
	}
	ts := Now()
	for _, e := range examples {
		e.ID = s.nextID("examples")
		e.CreatedAt = ts
		s.examples = append(s.examples, &e)
	}
	if messageID != "" {
		if s.processed[accountID] == nil {
			s.processed[accountID] = make(map[string]time.Time)
		}
		s.processed[accountID][messageID] = time.Time{} // confirmed
	}
	return nil
}

// ListExamplesByVerdict mirrors Store.ListExamplesByVerdict for tests: the newest (highest
// ID) up to limit examples of one verdict for a prompt, newest first — same contract real
// callers (selectExamplesForPrompt) depend on.
func (s *FakeStore) ListExamplesByVerdict(_ context.Context, promptID int64, verdict string, limit int32) ([]PromptExample, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var matched []PromptExample
	for _, e := range s.examples {
		if e.PromptID == promptID && e.Verdict == verdict {
			matched = append(matched, *e)
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].ID > matched[j].ID })
	// Widen limit to int rather than narrow len(matched) to int32 — avoids a lossy
	// conversion on either side regardless of platform int width.
	if len(matched) > int(limit) {
		matched = matched[:limit]
	}
	return matched, nil
}

func (s *FakeStore) RecordLlmDebug(_ context.Context, _ AddLlmDebugParams) error { return nil }

// GetHistoryFiltered mirrors Store.GetHistoryFiltered's cursor-page contract: newest
// first (ts desc, id desc — the same order as the real store's SK), a cursor that
// resumes strictly below a given SK, and a NextCursor that only goes empty once every
// row below the cursor has actually been examined. Unlike the real store it doesn't
// split work per account or per DynamoDB page (everything's already in memory), but the
// examined/consumed bookkeeping is kept in step with it so tests exercise the same
// short-page and cursor-advance-without-a-match behavior a sparse filter produces there.
func (s *FakeStore) GetHistoryFiltered(_ context.Context, f HistoryFilter) (HistoryPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	pageSize := f.Limit
	if pageSize <= 0 {
		pageSize = 50
	}

	var accountIDs []int64
	if f.AccountID != nil {
		accountIDs = []int64{*f.AccountID}
	} else {
		seenAcct := map[int64]bool{}
		for _, h := range s.history {
			if !seenAcct[h.AccountID] {
				seenAcct[h.AccountID] = true
				accountIDs = append(accountIDs, h.AccountID)
			}
		}
	}

	byTSIDDesc := func(rows []*CategorizationHistory) func(i, j int) bool {
		return func(i, j int) bool {
			if rows[i].Timestamp != rows[j].Timestamp {
				return rows[i].Timestamp > rows[j].Timestamp
			}
			return rows[i].ID > rows[j].ID
		}
	}

	// Per account: sort newest-first, apply the cursor bound, then cap at pageSize — this
	// mirrors Store.GetHistoryFiltered's per-account ddb.Query(Limit: pageSize), which is
	// what actually bounds work per request there. Capping only the *merged* list (as an
	// earlier version of this fake did) doesn't reproduce that: it lets one call walk an
	// unbounded number of items in-memory looking for pageSize matches, silently defeating
	// the short/empty-page behavior a sparse filter produces against the real store.
	all := make([]*CategorizationHistory, 0, int(pageSize)*len(accountIDs))
	moreBeyondFetched := false
	for _, aid := range accountIDs {
		var acctRows []*CategorizationHistory
		for _, h := range s.history {
			if h.AccountID == aid {
				acctRows = append(acctRows, h)
			}
		}
		sort.Slice(acctRows, byTSIDDesc(acctRows))
		if f.Cursor != "" {
			cut := 0
			for cut < len(acctRows) && tsKey(acctRows[cut].Timestamp, acctRows[cut].ID) >= f.Cursor {
				cut++
			}
			acctRows = acctRows[cut:]
		}
		if int64(len(acctRows)) > pageSize {
			moreBeyondFetched = true // this account's partition has more past what we took
			acctRows = acctRows[:pageSize]
		}
		all = append(all, acctRows...)
	}
	sort.Slice(all, byTSIDDesc(all))

	// Walk the merge applying the (in the real store, Go-only) filters, same as
	// Store.GetHistoryFiltered: the cursor tracks the last item *examined*, not the last
	// matched, so a resumed page can't skip or repeat rows this call didn't get to.
	var filtered []CategorizationHistory
	lastConsumedSK := ""
	consumedAll := true
	for i, h := range all {
		lastConsumedSK = tsKey(h.Timestamp, h.ID)
		switch {
		case f.Unmatched && h.PromptID != nil:
			continue
		case f.PromptID != nil && (h.PromptID == nil || *h.PromptID != *f.PromptID):
			continue
		case f.SubjectQ != "" && !strings.Contains(strings.ToLower(h.Subject), strings.ToLower(f.SubjectQ)):
			continue
		case f.SenderQ != "" && !strings.Contains(strings.ToLower(h.Sender), strings.ToLower(f.SenderQ)):
			continue
		}
		filtered = append(filtered, *h)
		if int64(len(filtered)) >= pageSize {
			consumedAll = i == len(all)-1
			break
		}
	}

	nextCursor := ""
	if !consumedAll || moreBeyondFetched {
		nextCursor = lastConsumedSK
	}
	return HistoryPage{Rows: filtered, NextCursor: nextCursor}, nil
}

func (s *FakeStore) GetLabelRetention(_ context.Context, accountID int64) ([]LabelRetention, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]LabelRetention(nil), s.labelRet[accountID]...), nil
}

func (s *FakeStore) AddLabelRetention(_ context.Context, arg AddLabelRetentionParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID("labelret")
	s.labelRet[arg.AccountID] = append(s.labelRet[arg.AccountID], LabelRetention{
		ID: id, AccountID: arg.AccountID, LabelName: arg.LabelName, Days: arg.Days,
	})
	return nil
}

func (s *FakeStore) GetLabelExemptions(_ context.Context, accountID int64) ([]LabelExemption, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]LabelExemption(nil), s.labelExempt[accountID]...), nil
}

func (s *FakeStore) AddLabelExemption(_ context.Context, arg AddLabelExemptionParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID("labelexempt")
	s.labelExempt[arg.AccountID] = append(s.labelExempt[arg.AccountID], LabelExemption{
		ID: id, AccountID: arg.AccountID, LabelName: arg.LabelName,
	})
	return nil
}

func (s *FakeStore) GetAccountRetention(_ context.Context, accountID int64) (AccountRetention, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.retention[accountID]
	if !ok {
		return AccountRetention{AccountID: accountID}, fmt.Errorf("no retention rule for account %d", accountID)
	}
	return r, nil
}

func (s *FakeStore) SetGlobalRetention(_ context.Context, arg SetGlobalRetentionParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retention[arg.AccountID] = AccountRetention(arg)
	return nil
}

// InsertPromptSuggestion mirrors Store.InsertPromptSuggestion — enough of it for
// ClaimPromptSuggestion's own tests to have a row to claim.
func (s *FakeStore) InsertPromptSuggestion(_ context.Context, arg InsertPromptSuggestionParams) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID("suggestions")
	s.suggestions[id] = &PromptSuggestion{
		ID: id, CreatedAt: Now(), UpdatedAt: Now(),
		PromptID: arg.PromptID, TriggerKind: arg.TriggerKind,
		MessageID: arg.MessageID, EmailSubject: arg.EmailSubject, EmailSender: arg.EmailSender,
		EmailBodySnapshot: arg.EmailBodySnapshot, OriginalInstructions: arg.OriginalInstructions,
		SuggestedInstructions: arg.SuggestedInstructions, ConversationJSON: arg.ConversationJSON,
		Status: arg.Status,
	}
	return id, nil
}

// ClaimPromptSuggestion mirrors Store.ClaimPromptSuggestion: wins the claim only the first
// time for a given id, matching the real conditional-write's
// attribute_not_exists(claimedAt) gate.
func (s *FakeStore) ClaimPromptSuggestion(_ context.Context, id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.suggestionClaims[id] {
		return false, nil
	}
	s.suggestionClaims[id] = true
	return true, nil
}
