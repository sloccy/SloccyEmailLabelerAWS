package db

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// FakeStore is an in-memory StoreIface implementation for tests.
// It is safe for concurrent use.
type FakeStore struct {
	mu sync.Mutex

	accounts    map[int64]*Account
	emailToID   map[string]int64
	settings    map[string]string
	history     []*CategorizationHistory
	examples    []*PromptExample
	processed   map[int64]map[string]bool
	labelRet    map[int64][]LabelRetention
	labelExempt map[int64][]LabelExemption
	retention   map[int64]AccountRetention

	counters map[string]int64
}

func NewFake() *FakeStore {
	return &FakeStore{
		accounts:    make(map[int64]*Account),
		emailToID:   make(map[string]int64),
		settings:    make(map[string]string),
		processed:   make(map[int64]map[string]bool),
		labelRet:    make(map[int64][]LabelRetention),
		labelExempt: make(map[int64][]LabelExemption),
		retention:   make(map[int64]AccountRetention),
		counters:    make(map[string]int64),
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
	var out []string
	for _, id := range messageIDs {
		if pm == nil || !pm[id] {
			out = append(out, id)
		}
	}
	return out, nil
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
			s.processed[accountID] = make(map[string]bool)
		}
		s.processed[accountID][messageID] = true
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

func (s *FakeStore) GetHistoryFiltered(_ context.Context, f HistoryFilter) ([]CategorizationHistory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []CategorizationHistory
	for _, h := range s.history {
		if f.AccountID != nil && h.AccountID != *f.AccountID {
			continue
		}
		if f.PromptID != nil && (h.PromptID == nil || *h.PromptID != *f.PromptID) {
			continue
		}
		if f.Unmatched && h.PromptID != nil {
			continue
		}
		if f.SubjectQ != "" && !strings.Contains(h.Subject, f.SubjectQ) {
			continue
		}
		if f.SenderQ != "" && !strings.Contains(h.Sender, f.SenderQ) {
			continue
		}
		result = append(result, *h)
	}
	if f.Limit > 0 && int64(len(result)) > f.Limit {
		result = result[:f.Limit]
	}
	return result, nil
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
