package poller

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sloccy/ollamail/db"
	gmailpkg "github.com/sloccy/ollamail/gmail"
	"github.com/sloccy/ollamail/llm"
	"github.com/sloccy/ollamail/processor"
)

func newTestStore(t *testing.T) *db.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := s.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// newTestPoller creates a Poller wired to a test store with no-op
// processAccount and cleanup functions by default. The interval is set to
// a short duration so tests don't have to wait long for ticks.
func newTestPoller(t *testing.T, store *db.Store) *Poller {
	t.Helper()
	cfg := &Config{
		LookbackHours:  1,
		MaxResults:     10,
		BodyTruncation: 1000,
		LogRetention:   30,
		DebugLogging:   false,
	}
	// NewClient/NewAuth args don't matter; the seam fns replace their usage.
	p := New(store, llm.NewClient("http://localhost", "m", 4096, time.Second), gmailpkg.NewAuth("/dev/null"), cfg)
	// Override with no-ops so tests don't hit real dependencies.
	p.processAccount = func(_ context.Context, _ *db.Store, _ *llm.Client, _ *gmailpkg.Auth, _ db.Account, _ []db.Prompt, _ processor.ProcessConfig) (*gmailpkg.ServiceWrapper, error) {
		return nil, nil //nolint:nilnil
	}
	p.cleanup = func(_ context.Context, _ *db.Store, _ *gmailpkg.Client, _ int64) {}
	return p
}

// ============================================================
// Start / Stop lifecycle
// ============================================================

func TestStartStop(t *testing.T) {
	store := newTestStore(t)
	p := newTestPoller(t, store)

	p.Start()
	// Give the goroutine a moment to spin up.
	time.Sleep(10 * time.Millisecond)

	doneCh := make(chan struct{})
	go func() {
		p.Stop()
		close(doneCh)
	}()

	select {
	case <-doneCh:
		// Stop returned promptly — goroutine exits when context is cancelled.
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return within 2s")
	}
}

// ============================================================
// RunNow blocks until the scan completes
// ============================================================

func TestRunNow(t *testing.T) {
	store := newTestStore(t)
	p := newTestPoller(t, store)

	var called atomic.Int32
	p.processAccount = func(_ context.Context, _ *db.Store, _ *llm.Client, _ *gmailpkg.Auth, _ db.Account, _ []db.Prompt, _ processor.ProcessConfig) (*gmailpkg.ServiceWrapper, error) {
		called.Add(1)
		return nil, nil //nolint:nilnil
	}

	// No accounts in the store → processAccount should never be called.
	p.RunNow()
	if called.Load() != 0 {
		t.Errorf("RunNow with no accounts called processAccount %d times", called.Load())
	}

	// Add an account → processAccount should be called once.
	accID, err := store.UpsertAccount(context.Background(), db.UpsertAccountParams{
		Email:           "test@example.com",
		CredentialsJson: `{}`,
	})
	if err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	// Ensure account is active (default should be 1, but toggle once to confirm).
	active, _ := store.GetAccount(context.Background(), accID)
	if active.Active == 0 {
		store.ToggleAccount(context.Background(), accID) //nolint:errcheck,gosec
	}

	p.RunNow()
	if called.Load() != 1 {
		t.Errorf("RunNow with 1 active account: processAccount called %d times, want 1", called.Load())
	}
}

// ============================================================
// RunNow is not re-entrant (scanMu)
// ============================================================

func TestRunNow_NonReentrant(t *testing.T) {
	store := newTestStore(t)
	p := newTestPoller(t, store)

	// Need an active account so processAccount is actually invoked.
	_, err := store.UpsertAccount(context.Background(), db.UpsertAccountParams{
		Email:           "block@test.com",
		CredentialsJson: `{}`,
	})
	if err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var callCount atomic.Int32

	p.processAccount = func(_ context.Context, _ *db.Store, _ *llm.Client, _ *gmailpkg.Auth, _ db.Account, _ []db.Prompt, _ processor.ProcessConfig) (*gmailpkg.ServiceWrapper, error) {
		callCount.Add(1)
		close(started)  // signal scan #1 is running
		<-release       // block until released
		return nil, nil //nolint:nilnil
	}

	// Launch scan #1 — will block inside processAccount.
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.RunNow()
	}()

	// Wait until scan #1 holds the lock.
	<-started

	// Scan #2 must return immediately (TryLock fails) and not increment callCount.
	if p.RunNow() {
		t.Error("second RunNow: expected false (already running), got true")
	}
	if callCount.Load() != 1 {
		t.Errorf("second RunNow invoked processAccount: callCount = %d, want 1", callCount.Load())
	}

	// Release scan #1 and confirm it finished cleanly.
	close(release)
	<-done
	if callCount.Load() != 1 {
		t.Errorf("total processAccount calls = %d, want 1", callCount.Load())
	}
}

// ============================================================
// GetStatus
// ============================================================

func TestGetStatus_Idle(t *testing.T) {
	store := newTestStore(t)
	p := newTestPoller(t, store)

	status := p.GetStatus()
	if status.Running {
		t.Error("expected Running=false before Start")
	}
}

// ============================================================
// UpdateInterval changes the interval
// ============================================================

func TestUpdateInterval(t *testing.T) {
	store := newTestStore(t)
	p := newTestPoller(t, store)
	p.interval = 5 * time.Minute

	p.UpdateInterval(60) // 60 seconds

	p.mu.RLock()
	got := p.interval
	p.mu.RUnlock()

	if got != 60*time.Second {
		t.Errorf("interval = %v, want 60s", got)
	}
}

// ============================================================
// processAccount is called for each active account on tick
// ============================================================

func TestLoop_CallsProcessorPerActiveAccount(t *testing.T) {
	store := newTestStore(t)
	p := newTestPoller(t, store)

	// Insert 2 active accounts.
	for _, email := range []string{"acc1@test.com", "acc2@test.com"} {
		store.UpsertAccount(context.Background(), db.UpsertAccountParams{ //nolint:errcheck,gosec
			Email: email, CredentialsJson: `{}`,
		})
	}
	// Insert 1 inactive account.
	inactiveID, _ := store.UpsertAccount(context.Background(), db.UpsertAccountParams{
		Email: "inactive@test.com", CredentialsJson: `{}`,
	})
	store.ToggleAccount(context.Background(), inactiveID) //nolint:errcheck,gosec

	var callCount atomic.Int32
	p.processAccount = func(_ context.Context, _ *db.Store, _ *llm.Client, _ *gmailpkg.Auth, _ db.Account, _ []db.Prompt, _ processor.ProcessConfig) (*gmailpkg.ServiceWrapper, error) {
		callCount.Add(1)
		return nil, nil //nolint:nilnil
	}

	p.RunNow() // triggers runScan synchronously
	if callCount.Load() != 2 {
		t.Errorf("expected 2 processAccount calls (2 active accounts), got %d", callCount.Load())
	}
}

// ============================================================
// Inactive accounts are skipped
// ============================================================

func TestLoop_SkipsInactiveAccounts(t *testing.T) {
	store := newTestStore(t)
	p := newTestPoller(t, store)

	id, _ := store.UpsertAccount(context.Background(), db.UpsertAccountParams{
		Email: "inactive@test.com", CredentialsJson: `{}`,
	})
	// Toggle to make inactive (Active starts at 1 from UpsertAccount schema default).
	acc, _ := store.GetAccount(context.Background(), id)
	if acc.Active != 0 {
		store.ToggleAccount(context.Background(), id) //nolint:errcheck,gosec
	}

	var called atomic.Int32
	p.processAccount = func(_ context.Context, _ *db.Store, _ *llm.Client, _ *gmailpkg.Auth, _ db.Account, _ []db.Prompt, _ processor.ProcessConfig) (*gmailpkg.ServiceWrapper, error) {
		called.Add(1)
		return nil, nil //nolint:nilnil
	}

	p.RunNow()
	if called.Load() != 0 {
		t.Errorf("expected 0 calls for inactive account, got %d", called.Load())
	}
}

// ============================================================
// cleanup is called when ProcessAccount returns a ServiceWrapper
// ============================================================

func TestLoop_CallsCleanupWhenWrapperReturned(t *testing.T) {
	store := newTestStore(t)
	p := newTestPoller(t, store)

	store.UpsertAccount(context.Background(), db.UpsertAccountParams{ //nolint:errcheck,gosec
		Email: "clean@test.com", CredentialsJson: `{}`,
	})

	p.processAccount = func(_ context.Context, _ *db.Store, _ *llm.Client, _ *gmailpkg.Auth, _ db.Account, _ []db.Prompt, _ processor.ProcessConfig) (*gmailpkg.ServiceWrapper, error) {
		// Return a non-nil wrapper → cleanup should be called.
		return &gmailpkg.ServiceWrapper{Svc: nil}, nil
	}

	var cleanupCalled atomic.Int32
	p.cleanup = func(_ context.Context, _ *db.Store, _ *gmailpkg.Client, _ int64) {
		cleanupCalled.Add(1)
	}

	p.RunNow()
	if cleanupCalled.Load() != 1 {
		t.Errorf("expected 1 cleanup call, got %d", cleanupCalled.Load())
	}
}

// ============================================================
// Cancellation during scan exits cleanly
// ============================================================

func TestCancellationDuringScan(t *testing.T) {
	store := newTestStore(t)
	p := newTestPoller(t, store)

	store.UpsertAccount(context.Background(), db.UpsertAccountParams{ //nolint:errcheck,gosec
		Email: "slow@test.com", CredentialsJson: `{}`,
	})

	scanStarted := make(chan struct{})
	p.processAccount = func(ctx context.Context, _ *db.Store, _ *llm.Client, _ *gmailpkg.Auth, _ db.Account, _ []db.Prompt, _ processor.ProcessConfig) (*gmailpkg.ServiceWrapper, error) {
		close(scanStarted)
		// Block until context is canceled.
		<-ctx.Done()
		return nil, ctx.Err()
	}

	p.Start()
	<-scanStarted
	p.Stop()

	// Test passes if we don't hang here.
}

// Verify that db.Account.Active=0 rows really are returned from the DB
// (regression guard: the SQL schema stores Active as int64, so zero-value matters).
func TestActiveZeroMeansFalse(t *testing.T) {
	store := newTestStore(t)
	id, _ := store.UpsertAccount(context.Background(), db.UpsertAccountParams{
		Email: "toggle@test.com", CredentialsJson: `{}`,
	})

	acc, _ := store.GetAccount(context.Background(), id)
	if acc.Active == 0 {
		t.Fatalf("new account should be active (Active=1), got %d", acc.Active)
	}

	// Toggle to inactive.
	store.ToggleAccount(context.Background(), id) //nolint:errcheck,gosec
	acc2, _ := store.GetAccount(context.Background(), id)
	if acc2.Active != 0 {
		t.Errorf("toggled account should be inactive (Active=0), got %d", acc2.Active)
	}
}
