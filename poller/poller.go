package poller

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/sloccy/ollamail/db"
	"github.com/sloccy/ollamail/gmail"
	"github.com/sloccy/ollamail/llm"
	"github.com/sloccy/ollamail/processor"
	"github.com/sloccy/ollamail/retention"
)

const cleanupInterval = time.Hour

type processorFn func(ctx context.Context, store *db.Store, ollamaClient *llm.Client, gmailAuth *gmail.Auth, account db.Account, prompts []db.Prompt, cfg processor.ProcessConfig) (*gmail.ServiceWrapper, error)

type cleanupFn func(ctx context.Context, store *db.Store, svc *gmail.Client, accountID int64)

// Poller runs background email scans on a configurable interval.
type Poller struct {
	store        *db.Store
	ollamaClient *llm.Client
	gmailAuth    *gmail.Auth
	cfg          *Config

	processAccount processorFn
	cleanup        cleanupFn

	mu          sync.RWMutex
	interval    time.Duration
	lastRun     time.Time
	nextRun     time.Time
	lastCleanup time.Time

	scanMu  sync.Mutex    // non-blocking try-lock for scan exclusion
	resetCh chan struct{} // signals loop to reset the timer after interval change

	wg      sync.WaitGroup
	loopCtx context.Context // set by Start, read by RunNow; guarded by mu
	cancel  context.CancelFunc
}

// Config holds the runtime configuration needed by the poller.
type Config struct {
	LookbackHours  int
	MaxResults     int64
	BodyTruncation int
	LogRetention   int
	DebugLogging   bool
}

// Status is returned by GetStatus.
type Status struct {
	Running bool
	LastRun string
	NextRun string
}

func New(store *db.Store, ollamaClient *llm.Client, auth *gmail.Auth, cfg *Config) *Poller {
	return &Poller{
		store:          store,
		ollamaClient:   ollamaClient,
		gmailAuth:      auth,
		cfg:            cfg,
		resetCh:        make(chan struct{}, 1),
		processAccount: processor.ProcessAccount,
		cleanup:        retention.Cleanup,
	}
}

// Start begins the polling loop. Reads interval from DB settings.
func (p *Poller) Start() {
	ctx := context.Background()
	val, err := p.store.GetSetting(ctx, "poll_interval")
	if err == nil {
		if n, err := strconv.Atoi(val); err == nil && n > 0 {
			p.mu.Lock()
			p.interval = time.Duration(n) * time.Second
			p.mu.Unlock()
		}
	}
	if p.interval == 0 {
		p.interval = 5 * time.Minute
	}

	p.mu.Lock()
	p.nextRun = time.Now()
	p.mu.Unlock()

	loopCtx, cancel := context.WithCancel(context.Background())
	p.mu.Lock()
	p.loopCtx = loopCtx
	p.cancel = cancel
	p.mu.Unlock()
	p.wg.Go(func() { p.loop(loopCtx) })
}

// Stop signals the poller loop to exit and waits for any in-flight scan to finish.
func (p *Poller) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
}

// RunNow triggers a scan and blocks until it completes.
// Returns false if a scan is already running (scan was skipped).
func (p *Poller) RunNow() bool {
	ctx := context.Background()
	p.mu.RLock()
	if p.loopCtx != nil {
		ctx = p.loopCtx
	}
	p.mu.RUnlock()
	return p.runScan(ctx)
}

// UpdateInterval changes the polling interval and reschedules the next run.
func (p *Poller) UpdateInterval(seconds int) {
	p.mu.Lock()
	p.interval = time.Duration(seconds) * time.Second
	p.nextRun = time.Now().Add(p.interval)
	p.mu.Unlock()
	// Signal loop to reset the timer so the new interval takes effect immediately.
	select {
	case p.resetCh <- struct{}{}:
	default:
	}
}

// GetStatus returns current poller state.
func (p *Poller) GetStatus() Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var lastRun, nextRun string
	if !p.lastRun.IsZero() {
		lastRun = p.lastRun.Local().Format("2006-01-02 15:04:05")
	}
	if !p.nextRun.IsZero() {
		nextRun = p.nextRun.Local().Format("2006-01-02 15:04:05")
	}
	running := !p.scanMu.TryLock()
	if !running {
		p.scanMu.Unlock()
	}
	return Status{
		Running: running,
		LastRun: lastRun,
		NextRun: nextRun,
	}
}

func (p *Poller) loop(ctx context.Context) {
	p.mu.RLock()
	d := time.Until(p.nextRun)
	p.mu.RUnlock()
	if d < 0 {
		d = 0
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.resetCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			p.mu.RLock()
			d = time.Until(p.nextRun)
			p.mu.RUnlock()
			if d < 0 {
				d = 0
			}
			timer.Reset(d)
		case <-timer.C:
			p.mu.Lock()
			now := time.Now()
			p.nextRun = p.nextRun.Add(p.interval)
			if p.nextRun.Before(now) {
				// Fell behind (e.g. scan ran longer than interval or system was
				// suspended). Skip missed ticks and resume on the wall clock.
				p.nextRun = now.Add(p.interval)
			}
			d = time.Until(p.nextRun)
			p.mu.Unlock()
			timer.Reset(d)
			p.wg.Go(func() { p.runScan(ctx) })
		}
	}
}

func (p *Poller) runScan(ctx context.Context) bool {
	if !p.scanMu.TryLock() {
		return false // scan already running
	}
	defer p.scanMu.Unlock()

	now := time.Now()
	p.mu.Lock()
	p.lastRun = now
	doCleanup := now.Sub(p.lastCleanup) >= cleanupInterval
	if doCleanup {
		p.lastCleanup = now
	}
	p.mu.Unlock()

	if doCleanup {
		_ = p.store.TrimLogs(ctx, p.cfg.LogRetention)
		_ = p.store.TrimProcessedEmails(ctx, p.cfg.LookbackHours)
		_ = p.store.TrimHistory(ctx, p.cfg.LogRetention)
	}

	accounts, err := p.store.ListAccounts(ctx)
	if err != nil {
		slog.Error("list accounts", "err", err)
		return true
	}

	prompts, err := p.store.ListActivePrompts(ctx)
	if err != nil {
		slog.Error("list prompts", "err", err)
		return true
	}

	procCfg := processor.ProcessConfig{
		LookbackHours:  p.cfg.LookbackHours,
		MaxResults:     p.cfg.MaxResults,
		BodyTruncation: p.cfg.BodyTruncation,
		DebugLogging:   p.cfg.DebugLogging,
	}

	for _, account := range accounts {
		if ctx.Err() != nil {
			break
		}
		if account.Active == 0 {
			continue
		}
		wrapper, err := p.processAccount(ctx, p.store, p.ollamaClient, p.gmailAuth, account, prompts, procCfg)
		if err != nil {
			slog.Error("process account", "email", account.Email, "err", err)
			p.store.Log("ERROR", "Scan failed for "+account.Email+": "+err.Error())
			continue
		}
		if wrapper != nil {
			p.cleanup(ctx, p.store, wrapper.Svc, account.ID)
		}
	}
	return true
}
