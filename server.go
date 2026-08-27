package main

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/sloccy/ollamail-aws/db"
	"github.com/sloccy/ollamail-aws/gmail"
	"github.com/sloccy/ollamail-aws/llm"
	"github.com/sloccy/ollamail-aws/processor"
)

//go:embed static
var staticFS embed.FS

const retentionUnitYears = "years"

// scanCadenceLabel is the human-readable schedule shown in the UI (sidebar + dashboard).
// The catch-up scan runs on a fixed daily 2 AM ET schedule (see ScanSchedule in
// template.yaml) — off-peak, so its flex-tier Bedrock traffic doesn't compete with
// real-time push traffic during business hours. This is intentionally not configurable:
// there is no EventBridge Scheduler rewrite path.
const scanCadenceLabel = "Daily · 2 AM ET"

const (
	triggerShowToast              = "showToast"
	triggerRefreshSuggestionBadge = "refreshSuggestionBadge"
	toastKeyMessage               = "message"
	jsonKeyType                   = "type"
	jsonKeyText                   = "text"
	toastTypeSuccess              = "success"
	encodingGzip                  = "gzip"
	headerAcceptEncoding          = "Accept-Encoding"
)

// generateStreamPath is the SSE endpoint that both ServeHTTP below (skips gzip —
// flushing one small chunk per generated token shouldn't pay a compress+flush cycle per
// chunk) and newSecurityMiddleware (security.go — CSRF-guards it despite being a GET,
// since it drives Bedrock spend) special-case. One shared constant so a route rename
// can't silently drop either exception out of sync with the other.
const generateStreamPath = "/api/prompts/generate-stream"

// server holds all dependencies and the route mux.
type server struct {
	ctx   context.Context
	store *db.Store
	llm   *llm.Client
	cfg   *Config
	auth  *gmail.Auth
	tmpl  *template.Template
	mux   *http.ServeMux

	// improver runs improve+replay rounds directly — used as the local-development/test
	// fallback in dispatchImprove (improve.go) when improveLambda is nil. The MODE=improve
	// worker Lambda builds its own separate instance in main.go; this one never runs in
	// the deployed WebFunction path once ImproveFunctionName is set.
	improver *improveRunner
	// improveLambda invokes the MODE=improve worker asynchronously (see dispatchImprove).
	// nil when cfg.ImproveFunctionName is unset (local dev, tests) or the AWS config
	// couldn't be loaded — either way, dispatchImprove falls back to improver directly.
	improveLambda *lambda.Client

	// OAuth state: short-lived in-memory map (single instance, no need for persistent
	// storage), keyed by the CSRF state token; carries the PKCE verifier for the exchange.
	oauthMu    sync.Mutex
	oauthState map[string]oauthPending

	// Cached Bedrock model list (refreshed at most once per hour)
	modelsMu        sync.Mutex
	modelsCache     []llm.ModelOption
	modelsFetchedAt time.Time
}

func newServer(ctx context.Context, store *db.Store, llmClient *llm.Client, auth *gmail.Auth, cfg *Config) http.Handler {
	s := &server{
		ctx:        ctx,
		store:      store,
		llm:        llmClient,
		cfg:        cfg,
		auth:       auth,
		oauthState: make(map[string]oauthPending),
		improver:   newImproveRunner(store, llmClient, cfg),
	}
	if cfg.ImproveFunctionName != "" {
		if awsCfg, err := awsconfig.LoadDefaultConfig(ctx); err != nil {
			// Not fatal: dispatchImprove falls back to running improve rounds in-process
			// via s.improver when improveLambda is nil, same as an unset
			// ImproveFunctionName — degraded (subject to the goroutine-freeze issue this
			// worker exists to avoid), not broken.
			slog.Error("load aws config for improve dispatch, falling back to in-process", "err", err)
		} else {
			s.improveLambda = lambda.NewFromConfig(awsCfg)
		}
	}

	var err error
	s.tmpl, err = loadTemplates()
	if err != nil {
		panic(fmt.Sprintf("load templates: %v", err))
	}

	s.mux = http.NewServeMux()
	s.registerRoutes()
	return s
}

const maxBodySize = 10 << 20 // 10 MB

var gzipPool = sync.Pool{
	New: func() any {
		return gzip.NewWriter(io.Discard)
	},
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	// /static/ serves pre-gzipped files itself (see registerRoutes); the SSE stream flushes
	// one small chunk per generated token, and wrapping it in gzip would force a compress+
	// flush cycle on every chunk for no real size benefit — both skip the wrapper entirely.
	if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == generateStreamPath {
		s.mux.ServeHTTP(w, r)
		return
	}
	if strings.Contains(r.Header.Get(headerAcceptEncoding), encodingGzip) {
		w.Header().Set("Content-Encoding", encodingGzip)
		w.Header().Set("Vary", headerAcceptEncoding)
		// sync.Pool.New only ever yields *gzip.Writer, but a checked assertion costs
		// nothing and degrades to a fresh writer rather than panicking mid-response if
		// anything ever Puts the wrong type back.
		gz, ok := gzipPool.Get().(*gzip.Writer)
		if !ok {
			gz = gzip.NewWriter(io.Discard)
		}
		gz.Reset(w)
		defer func() {
			_ = gz.Close()
			gzipPool.Put(gz)
		}()
		s.mux.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, Writer: gz}, r)
		return
	}
	s.mux.ServeHTTP(w, r)
}

// gzipResponseWriter wraps http.ResponseWriter to compress response bodies.
type gzipResponseWriter struct {
	http.ResponseWriter
	Writer *gzip.Writer
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	return g.Writer.Write(b)
}

func (g *gzipResponseWriter) Flush() {
	_ = g.Writer.Flush()
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// writeJSON encodes v as the response body, logging rather than returning a failure:
// the status line and headers are already on the wire by this point, so there is no
// way left to tell the client anything different.
func writeJSON(w http.ResponseWriter, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("write json response", "err", err)
	}
}

// writeSSEEvent writes one {type, text} frame to a Server-Sent Events stream. Same
// reasoning as writeJSON on the error: mid-stream there is nothing to report it to.
func writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, typ, text string) {
	b, err := json.Marshal(map[string]string{jsonKeyType: typ, jsonKeyText: text})
	if err != nil {
		slog.Warn("marshal sse event", "type", typ, "err", err)
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
	flusher.Flush()
}

func (s *server) registerRoutes() {
	// Static. Templates link these via {{asset}}, which puts a content hash in the path
	// (see assets.go), so a hashed URL names one immutable body and can be cached hard —
	// by browsers and by the CloudFront /static/* behavior. Anything without a current
	// hash still serves, but stays no-store: only the hash lets us promise immutability.
	staticSub, _ := fs.Sub(staticFS, "static")
	fileServer := http.StripPrefix("/static/", http.FileServer(http.FS(staticSub)))
	s.mux.HandleFunc("GET /static/", func(w http.ResponseWriter, r *http.Request) {
		relPath := strings.TrimPrefix(r.URL.Path, "/static/")
		cacheControl := "no-store"
		// Strip a leading hash segment whether or not it's current. A stale one means a
		// page rendered before the last deploy; serve it the fresh bytes uncached rather
		// than 404, and let the next render pick up the new URL.
		if seg, rest, ok := strings.Cut(relPath, "/"); ok && isAssetHash(seg) {
			if _, err := fs.Stat(staticSub, rest); err == nil {
				if assetHashes()[rest] == seg {
					cacheControl = assetImmutableCacheControl
				}
				relPath = rest
				r = r.Clone(r.Context())
				r.URL.Path = "/static/" + rest
			}
		}
		w.Header().Set("Cache-Control", cacheControl)
		if strings.Contains(r.Header.Get(headerAcceptEncoding), encodingGzip) {
			if f, err := staticSub.Open(relPath + ".gz"); err == nil {
				defer func() { _ = f.Close() }()
				ct := mime.TypeByExtension(path.Ext(relPath))
				if ct == "" {
					ct = "application/octet-stream"
				}
				w.Header().Set("Content-Type", ct)
				w.Header().Set("Content-Encoding", encodingGzip)
				w.Header().Set("Vary", headerAcceptEncoding)
				_, _ = io.Copy(w, f)
				return
			}
		}
		fileServer.ServeHTTP(w, r)
	})

	// Index
	s.mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		s.render(w, "index.html", nil)
	})

	// Fragments
	s.mux.HandleFunc("GET /fragments/dashboard", s.handleDashboard)
	s.mux.HandleFunc("GET /fragments/accounts", s.handleAccounts)
	s.mux.HandleFunc("POST /fragments/accounts/{id}/toggle", s.handleToggleAccount)
	s.mux.HandleFunc("DELETE /fragments/accounts/{id}", s.handleDeleteAccount)
	s.mux.HandleFunc("GET /fragments/prompts", s.handlePromptsList)
	s.mux.HandleFunc("POST /fragments/prompts", s.handleCreatePrompt)
	s.mux.HandleFunc("PUT /fragments/prompts/{id}", s.handleUpdatePrompt)
	s.mux.HandleFunc("DELETE /fragments/prompts/{id}", s.handleDeletePrompt)
	s.mux.HandleFunc("POST /fragments/prompts/{id}/toggle", s.handleTogglePrompt)
	s.mux.HandleFunc("GET /fragments/prompts/{id}/edit", s.handleEditPrompt)
	s.mux.HandleFunc("GET /fragments/prompts/{id}/view", s.handleViewPrompt)
	s.mux.HandleFunc("GET /fragments/prompts/{id}/examples-count", s.handlePromptExamplesBadge)
	s.mux.HandleFunc("GET /fragments/prompts/{id}/examples", s.handlePromptExamples)
	s.mux.HandleFunc("POST /fragments/prompts/{id}/clear-examples", s.handleClearPromptExamples)
	s.mux.HandleFunc("GET /fragments/settings", s.handleGetSettings)
	s.mux.HandleFunc("PATCH /fragments/settings", s.handleUpdateSettings)
	s.mux.HandleFunc("GET /fragments/logs", s.handleLogs)
	s.mux.HandleFunc("GET /fragments/history", s.handleHistory)
	s.mux.HandleFunc("GET /fragments/troubleshooting", s.handleTroubleshooting)
	s.mux.HandleFunc("GET /fragments/history/filters", s.handleHistoryFilters)
	s.mux.HandleFunc("GET /fragments/history/{id}/llm-response", s.handleHistoryLlmResponse)
	s.mux.HandleFunc("GET /fragments/history/{id}/recategorize", s.handleRecategorizeForm)
	s.mux.HandleFunc("POST /fragments/history/{id}/recategorize", s.handleRecategorize)
	s.mux.HandleFunc("POST /fragments/history/{id}/confirm", s.handleConfirmCategorization)
	s.mux.HandleFunc("GET /fragments/history/bulk-recategorize", s.handleBulkRecategorizeForm)
	s.mux.HandleFunc("POST /fragments/history/bulk-recategorize", s.handleBulkRecategorize)
	s.mux.HandleFunc("POST /fragments/history/bulk-confirm", s.handleBulkConfirmCategorization)
	s.mux.HandleFunc("GET /fragments/prompt-suggestions", s.handlePromptSuggestionsList)
	s.mux.HandleFunc("GET /fragments/prompt-suggestions/{id}", s.handlePromptSuggestionDetail)
	s.mux.HandleFunc("GET /fragments/prompt-suggestions/{id}/trace", s.handlePromptSuggestionTrace)
	s.mux.HandleFunc("POST /fragments/prompt-suggestions/{id}/regenerate", s.handlePromptSuggestionRegenerate)
	s.mux.HandleFunc("POST /fragments/prompt-suggestions/{id}/apply", s.handlePromptSuggestionApply)
	s.mux.HandleFunc("POST /fragments/prompt-suggestions/{id}/dismiss", s.handlePromptSuggestionDismiss)
	s.mux.HandleFunc("GET /fragments/retention/{id}", s.handleGetRetention)
	s.mux.HandleFunc("POST /fragments/retention/{id}", s.handleSetGlobalRetention)
	s.mux.HandleFunc("POST /fragments/retention/{id}/labels", s.handleAddLabelRetention)
	s.mux.HandleFunc("DELETE /fragments/retention/{id}/labels/{ruleId}", s.handleDeleteLabelRetention)
	s.mux.HandleFunc("POST /fragments/retention/{id}/exemptions", s.handleAddExemption)
	s.mux.HandleFunc("DELETE /fragments/retention/{id}/exemptions/{eid}", s.handleDeleteExemption)
	s.mux.HandleFunc("POST /fragments/oauth/start", s.handleOAuthStart)
	s.mux.HandleFunc("POST /fragments/oauth/exchange", s.handleOAuthExchange)
	s.mux.HandleFunc("POST /fragments/scan", s.handleScan)
	s.mux.HandleFunc("GET /fragments/account-options", s.handleAccountOptions)
	s.mux.HandleFunc("GET /fragments/retention-query", s.handleRetentionQuery)

	// JSON APIs
	s.mux.HandleFunc("POST /api/prompts/reorder", s.handleReorderPrompts)
	s.mux.HandleFunc("GET /api/prompts/export", s.handleExportPrompts)
	s.mux.HandleFunc("GET /api/config/export", s.handleExportConfig)
	s.mux.HandleFunc("POST /api/config/import", s.handleImportConfig)
	s.mux.HandleFunc("GET /api/logs/download", s.handleDownloadLogs)
	s.mux.HandleFunc("GET /api/prompts/generate-stream", s.handleGenerateStream)
}

// ============================================================
// Template rendering helpers
// ============================================================

// render executes the template named name — either a whole file's basename (as embedded by
// loadTemplates) or a {{define}} block name — with data, writing a 500 if execution fails.
func (s *server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("render template", "name", name, "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

func (s *server) fragmentResponse(w http.ResponseWriter, name string, data any, toast string) {
	if toast != "" {
		setHxTrigger(w, map[string]any{triggerShowToast: toast})
	}
	s.render(w, name, data)
}

func setHxTrigger(w http.ResponseWriter, triggers map[string]any) {
	if b, err := json.Marshal(triggers); err == nil {
		w.Header().Set("Hx-Trigger", string(b))
	}
}

// ============================================================
// Dashboard
// ============================================================

func (s *server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	since30d := time.Now().AddDate(0, 0, -30)

	// Five independent reads, no data dependency between them — fanned out rather than run
	// serially, since this is the page users hit first and refreshDashboard re-triggers it.
	var accounts []db.ListAccountsSafeRow
	var activePrompts int64
	var logs []db.Log
	var turnaround []db.TurnaroundSample
	var timeoutCount int64
	var wg sync.WaitGroup
	wg.Go(func() { accounts, _ = s.store.ListAccountsSafe(ctx) })
	wg.Go(func() { activePrompts, _ = s.store.CountActivePrompts(ctx) })
	wg.Go(func() { logs, _ = s.store.GetLogs(ctx, 100) })
	wg.Go(func() { turnaround, _ = s.store.GetTurnaroundSamples(ctx, since30d) })
	wg.Go(func() { timeoutCount, _ = s.store.CountLogsByLevel(ctx, llm.LogLevelTimeout, since30d) })
	wg.Wait()

	data := map[string]any{
		"AccountCount":   len(accounts),
		"ActivePrompts":  activePrompts,
		"Logs":           logs,
		"ScanCadence":    scanCadenceLabel,
		"TimeoutCount":   timeoutCount,
		"TurnaroundBox":  buildBoxPlotSVG(turnaround),
		"TurnaroundLine": buildLatencyScatterSVG(turnaround),
	}
	s.fragmentResponse(w, "dashboard.html", data, "")
}

// ============================================================
// Accounts
// ============================================================

type accountView struct {
	ID         int64
	Email      string
	Active     bool
	AddedAt    string
	LastScanAt string
}

func (s *server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, _ := s.store.ListAccountsSafe(ctx)
	s.fragmentResponse(w, "accounts_list.html", toAccountViews(rows), "")
}

func (s *server) handleToggleAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := requireID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	_, _ = s.store.ToggleAccount(ctx, id)
	s.handleAccounts(w, r)
}

func (s *server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := requireID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	_ = s.store.DeleteAccountCascade(ctx, id)
	s.handleAccounts(w, r)
}

// ============================================================
// Prompts
// ============================================================

type promptView struct {
	ID             int64
	Name           string
	Instructions   string
	LabelName      string
	Active         bool
	CreatedAt      string
	ActionArchive  bool
	ActionSpam     bool
	ActionTrash    bool
	ActionMarkRead bool
	StopProcessing bool
	AccountID      int64
	AccountEmail   string
}

type promptEditView struct {
	Prompt   promptView
	Accounts []accountView
}

func (s *server) getPromptViews(ctx context.Context, accountIDFilter string) ([]promptView, error) {
	var prompts []db.Prompt
	var err error
	if accountIDFilter != "" && accountIDFilter != "0" {
		id, _ := strconv.ParseInt(accountIDFilter, 10, 64)
		prompts, err = s.store.ListPromptsByAccount(ctx, sql.NullInt64{Int64: id, Valid: true})
	} else {
		prompts, err = s.store.ListPrompts(ctx)
	}
	if err != nil {
		return nil, err
	}

	accounts, _ := s.store.ListAccountsSafe(ctx)
	accountMap := buildAccountMap(accounts)

	views := make([]promptView, len(prompts))
	for i, p := range prompts {
		views[i] = dbPromptToView(p, accountMap)
	}
	return views, nil
}

// renderPromptsList loads the prompt views (optionally filtered by accountIDFilter) and
// renders the shared prompts_list fragment. Shared by the list/create/update/delete
// handlers so the template path and load-then-render sequence can't drift between them.
func (s *server) renderPromptsList(w http.ResponseWriter, ctx context.Context, accountIDFilter, toast string) {
	views, _ := s.getPromptViews(ctx, accountIDFilter)
	s.fragmentResponse(w, "prompts_list.html", views, toast)
}

func (s *server) handlePromptsList(w http.ResponseWriter, r *http.Request) {
	s.renderPromptsList(w, r.Context(), r.URL.Query().Get("account_id"), "")
}

// promptFormFields holds the prompt fields shared by create and update — the name/label/
// instructions trim, the 5 boolToInt(action=="1") flags, and the account filter — parsed
// from r's form values by promptFieldsFromForm.
type promptFormFields struct {
	Name           string
	Instructions   string
	LabelName      string
	ActionArchive  int64
	ActionSpam     int64
	ActionTrash    int64
	ActionMarkRead int64
	StopProcessing int64
	AccountID      sql.NullInt64
}

func promptFieldsFromForm(r *http.Request) promptFormFields {
	return promptFormFields{
		Name:           strings.TrimSpace(r.FormValue("name")),
		Instructions:   strings.TrimSpace(r.FormValue("instructions")),
		LabelName:      strings.TrimSpace(r.FormValue("label_name")),
		ActionArchive:  boolToInt(r.FormValue("action_archive") == "1"),
		ActionSpam:     boolToInt(r.FormValue("action_spam") == "1"),
		ActionTrash:    boolToInt(r.FormValue("action_trash") == "1"),
		ActionMarkRead: boolToInt(r.FormValue("action_mark_read") == "1"),
		StopProcessing: boolToInt(r.FormValue("stop_processing") == "1"),
		AccountID:      parseAccountID(r),
	}
}

// parseAccountID parses the "account_id" form value into a sql.NullInt64 — empty or
// unparseable means "no account filter" (global).
func parseAccountID(r *http.Request) sql.NullInt64 {
	v := r.FormValue("account_id")
	if v == "" {
		return sql.NullInt64{}
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: id, Valid: true}
}

func (s *server) handleCreatePrompt(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_ = r.ParseForm()

	f := promptFieldsFromForm(r)
	if f.Name == "" {
		s.fragmentResponse(w, "prompts_list.html", nil, "Name is required")
		return
	}

	maxOrder, _ := s.store.MaxPromptSortOrder(ctx)
	_, err := s.store.CreatePrompt(ctx, db.CreatePromptParams{
		Name:           f.Name,
		Instructions:   f.Instructions,
		LabelName:      f.LabelName,
		ActionArchive:  f.ActionArchive,
		ActionSpam:     f.ActionSpam,
		ActionTrash:    f.ActionTrash,
		ActionMarkRead: f.ActionMarkRead,
		SortOrder:      maxOrder + 1,
		StopProcessing: f.StopProcessing,
		AccountID:      f.AccountID,
	})
	if err != nil {
		slog.Error("create prompt", "err", err)
		s.fragmentResponse(w, "prompts_list.html", nil, "Failed to create rule")
		return
	}

	// Pre-create label in background for all matching accounts. WithoutCancel keeps the
	// request's values while dropping its cancellation, so the work survives the response
	// without silently inheriting a context that is about to be cancelled.
	go s.ensureLabelForAccounts(context.WithoutCancel(r.Context()), f.LabelName, f.AccountID)

	s.renderPromptsList(w, ctx, "", "Rule saved")
}

func (s *server) handleUpdatePrompt(w http.ResponseWriter, r *http.Request) {
	id, ok := requireID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	_ = r.ParseForm()

	f := promptFieldsFromForm(r)

	_ = s.store.UpdatePrompt(ctx, db.UpdatePromptParams{
		Name:           f.Name,
		Instructions:   f.Instructions,
		LabelName:      f.LabelName,
		ActionArchive:  f.ActionArchive,
		ActionSpam:     f.ActionSpam,
		ActionTrash:    f.ActionTrash,
		ActionMarkRead: f.ActionMarkRead,
		StopProcessing: f.StopProcessing,
		AccountID:      f.AccountID,
		ID:             id,
	})

	go s.ensureLabelForAccounts(context.WithoutCancel(r.Context()), f.LabelName, f.AccountID)

	s.renderPromptsList(w, ctx, "", "Rule updated")
}

func (s *server) handleDeletePrompt(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	ctx := r.Context()
	_ = s.store.DeletePrompt(ctx, id)
	s.renderPromptsList(w, ctx, "", "Rule deleted")
}

// loadPromptView loads prompt id and returns it as a promptView (with its account email
// resolved), alongside the raw account list — the edit view needs the latter too, for its
// account-picker dropdown. Shared preamble for the toggle/edit/view prompt-card handlers.
func (s *server) loadPromptView(ctx context.Context, id int64) (promptView, []db.ListAccountsSafeRow, error) {
	p, err := s.store.GetPrompt(ctx, id)
	if err != nil {
		return promptView{}, nil, err
	}
	accounts, _ := s.store.ListAccountsSafe(ctx)
	return dbPromptToView(p, buildAccountMap(accounts)), accounts, nil
}

func (s *server) handleTogglePrompt(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	ctx := r.Context()
	_, _ = s.store.TogglePrompt(ctx, id)

	pv, _, err := s.loadPromptView(ctx, id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	s.render(w, "prompt_card_view", pv)
}

func (s *server) handleEditPrompt(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	ctx := r.Context()
	pv, accounts, err := s.loadPromptView(ctx, id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	data := promptEditView{
		Prompt:   pv,
		Accounts: toAccountViews(accounts),
	}
	s.render(w, "prompt_card_edit", data)
}

func (s *server) handleViewPrompt(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	pv, _, err := s.loadPromptView(r.Context(), id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	s.render(w, "prompt_card_view", pv)
}

// promptExamplesBadgeData feeds prompt_examples_badge.html.
type promptExamplesBadgeData struct {
	ID    int64
	Total int64
}

// handlePromptExamplesBadge renders a prompt card's example-corpus count. Lazy-loaded per
// card (hx-trigger="intersect once" in prompt_card_view.html) rather than fetched eagerly
// for every prompt when the list renders — CountExamplesByVerdict is 3 Query calls, and the
// prompts list can render on every page load, so eagerly summing it for every rule would be
// 3x the query volume for a count nobody's necessarily looking at.
func (s *server) handlePromptExamplesBadge(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	ctx := r.Context()
	counts, err := s.store.CountExamplesByVerdict(ctx, id)
	if err != nil {
		slog.Error("count prompt examples", "prompt_id", id, "err", err)
	}
	var total int64
	for _, n := range counts {
		total += n
	}
	s.fragmentResponse(w, "prompt_examples_badge.html", promptExamplesBadgeData{ID: id, Total: total}, "")
}

// promptExamplesPerVerdict bounds how many of each verdict's examples the expandable list on
// a prompt card shows. Deliberately small: a long-lived, actively-reviewed rule can still
// accumulate a large corpus over time, and the point of this panel is "what has this rule
// actually learned lately", not a full corpus dump — which would also be a much larger
// Query per expand against a 2-RCU table.
const promptExamplesPerVerdict = 25

// promptExamplesView feeds prompt_examples_list.html: one group per verdict, newest first.
type promptExamplesView struct {
	ID     int64
	Groups []exampleGroup
}

// handlePromptExamples renders the read-only expansion of a prompt card's example corpus.
// Lazy-loaded on the card's <details> opening (hx-trigger="toggle once") rather than with
// the card itself, for the same reason handlePromptExamplesBadge is: this is three Query
// calls, and the prompts list renders on every page load.
//
// Deliberately does not call CountExamplesByVerdict — the badge beside it already shows the
// corpus total, and the count path paginates a whole partition per verdict. Instead each
// verdict is fetched one row over the display cap, so a full group can say so without a
// second round trip.
func (s *server) handlePromptExamples(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	ctx := r.Context()
	view := promptExamplesView{ID: id}
	for _, v := range db.VerdictOrder {
		examples, err := s.store.ListExamplesByVerdict(ctx, id, v, promptExamplesPerVerdict+1)
		if err != nil {
			slog.Error("list prompt examples", "prompt_id", id, "verdict", v, "err", err)
			continue
		}
		if len(examples) == 0 {
			continue
		}
		g := exampleGroup{Verdict: v, Label: verdictLabels[v]}
		if len(examples) > promptExamplesPerVerdict {
			g.More = true
			examples = examples[:promptExamplesPerVerdict]
		}
		g.Examples = examples
		view.Groups = append(view.Groups, g)
	}
	s.fragmentResponse(w, "prompt_examples_list.html", view, "")
}

// handleClearPromptExamples deletes a rule's entire example corpus — the escape hatch for
// when the rule's intent has changed enough that its recorded history would mislead the
// next AI prompt improvement round (see DeleteExamplesForPrompt's doc comment). Returns the
// same badge fragment so the card updates in place to "0 examples".
func (s *server) handleClearPromptExamples(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	ctx := r.Context()
	if err := s.store.DeleteExamplesForPrompt(ctx, id); err != nil {
		slog.Error("clear prompt examples", "prompt_id", id, "err", err)
	}
	s.fragmentResponse(w, "prompt_examples_badge.html", promptExamplesBadgeData{ID: id, Total: 0}, "Examples cleared")
}

func buildAccountMap(accounts []db.ListAccountsSafeRow) map[int64]string {
	m := make(map[int64]string, len(accounts))
	for _, a := range accounts {
		m[a.ID] = a.Email
	}
	return m
}

func toAccountViews(accounts []db.ListAccountsSafeRow) []accountView {
	views := make([]accountView, len(accounts))
	for i, a := range accounts {
		views[i] = accountView{
			ID:         a.ID,
			Email:      a.Email,
			Active:     a.Active != 0,
			AddedAt:    a.AddedAt,
			LastScanAt: strOrEmpty(a.LastScanAt),
		}
	}
	return views
}

// strOrEmpty dereferences a possibly-nil *string, returning "" for nil.
func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func dbPromptToView(p db.Prompt, accountMap map[int64]string) promptView {
	pv := promptView{
		ID:             p.ID,
		Name:           p.Name,
		Instructions:   p.Instructions,
		LabelName:      p.LabelName,
		Active:         p.Active != 0,
		CreatedAt:      p.CreatedAt,
		ActionArchive:  p.ActionArchive != 0,
		ActionSpam:     p.ActionSpam != 0,
		ActionTrash:    p.ActionTrash != 0,
		ActionMarkRead: p.ActionMarkRead != 0,
		StopProcessing: p.StopProcessing != 0,
	}
	if p.AccountID != nil {
		pv.AccountID = *p.AccountID
		pv.AccountEmail = accountMap[*p.AccountID]
	}
	return pv
}

// ============================================================
// Settings
// ============================================================

const modelCacheTTL = time.Hour

// cachedModels returns the Bedrock model list, refreshing at most once per hour.
func (s *server) cachedModels(ctx context.Context) []llm.ModelOption {
	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()
	if time.Since(s.modelsFetchedAt) < modelCacheTTL && len(s.modelsCache) > 0 {
		return s.modelsCache
	}
	models, err := s.llm.ListAvailableModels(ctx)
	if err != nil {
		slog.Warn("list bedrock models", "err", err)
		return s.modelsCache // return stale on error
	}
	s.modelsCache = models
	s.modelsFetchedAt = time.Now()
	return models
}

// modelAllowedForTier mirrors the tier policy rendered into the per-tier <select>s in
// settings_form.html (classify and improve alike): Standard accepts any Converse-capable
// text model regardless of routing geography; Flex accepts any flex-capable model,
// likewise regardless of routing geography.
func modelAllowedForTier(m llm.ModelOption, tier string) bool {
	if tier == llm.TierFlex {
		return m.Flex
	}
	return true
}

// settingsView is every setting settings_form.html renders, resolved from storage (see
// resolveSettings). A struct instead of settingsTemplateData's old 11 positional params —
// which a wrong-order argument at either call site (handleGetSettings, handleUpdateSettings)
// would have compiled silently.
type settingsView struct {
	ClassifyModel          string
	ImproveModel           string
	ClassifyTier           string
	ImproveTier            string
	ReasoningDirective     string
	ImproveReplay          bool
	ImproveMaxRounds       int
	ImproveExampleCap      int
	ReplayExampleCap       int
	ImproveReasoningEffort string
}

// resolveSettings reads every setting settings_form.html needs out of settings (see
// loadSettings), applying the same defaults/parsing/clamping handleGetSettings and
// handleUpdateSettings both need — the latter as the "current value" baseline it patches
// with whatever the submitted form actually changes (see handleUpdateSettings).
func resolveSettings(settings map[string]string, defaultModel string) settingsView {
	return settingsView{
		ClassifyModel:          settingOr(settings, llm.SettingClassifyModel, defaultModel),
		ImproveModel:           settingOr(settings, llm.SettingImproveModel, defaultModel),
		ClassifyTier:           settingOr(settings, llm.SettingClassifyTier, llm.TierStandard),
		ImproveTier:            settingOr(settings, llm.SettingImproveTier, llm.TierStandard),
		ReasoningDirective:     settingOr(settings, llm.SettingClassifyReasoningDirective, ""),
		ImproveReplay:          settingOr(settings, llm.SettingImproveReplay, "1") == "1",
		ImproveMaxRounds:       parseImproveMaxRounds(settings[llm.SettingImproveMaxRounds]),
		ImproveExampleCap:      parseExampleCap(settings[llm.SettingImproveExampleCap], llm.ImproveExampleCapDefault, llm.ImproveExampleCapMax),
		ReplayExampleCap:       parseExampleCap(settings[llm.SettingReplayExampleCap], llm.ReplayExampleCapDefault, llm.ReplayExampleCapMax),
		ImproveReasoningEffort: settingOr(settings, llm.SettingImproveReasoningEffort, llm.ReasoningEffortOff),
	}
}

// settingsTemplateData builds the settings_form.html template data. models is used as-is for
// the Standard selects (already sorted cheapest-first by ListAvailableModels); FlexModels is a
// copy re-sorted by flex-tier cost (see llm.SortModelsByFlexCost) so the Flex selects sink their
// own unpriced entries to the bottom instead of inheriting the standard-cost order.
func settingsTemplateData(v settingsView, models []llm.ModelOption) map[string]any {
	return map[string]any{
		"ClassifyModel":      v.ClassifyModel,
		"ImproveModel":       v.ImproveModel,
		"ClassifyTier":       v.ClassifyTier,
		"ImproveTier":        v.ImproveTier,
		"ReasoningDirective": v.ReasoningDirective,
		"ImproveReplay":      v.ImproveReplay,
		"ImproveMaxRounds":   v.ImproveMaxRounds,
		// ImproveMaxRoundsOptions is the <select>'s option list — 1..llm.ImproveMaxRoundsCap
		// — computed here rather than hardcoded in the template so the two can't drift if
		// the cap ever changes.
		"ImproveMaxRoundsOptions": improveMaxRoundsOptions(),
		// ImproveExampleCap/ReplayExampleCap are number inputs (not <select>s — the ranges
		// are too wide, especially ReplayExampleCap's, to enumerate as options) bounded by
		// their *Max template values so the template can't drift from llm's actual caps.
		"ImproveExampleCap":      v.ImproveExampleCap,
		"ImproveExampleCapMax":   llm.ImproveExampleCapMax,
		"ReplayExampleCap":       v.ReplayExampleCap,
		"ReplayExampleCapMax":    llm.ReplayExampleCapMax,
		"ImproveReasoningEffort": v.ImproveReasoningEffort,
		// ImproveReasoningLevels is what the effort <select> actually renders as options —
		// not a fixed four values, but whatever llm.ReasoningEffortLevels reports the current
		// ImproveModel's family actually distinguishes (see reasoningEffortRegistry). Empty
		// means the model doesn't expose a controllable reasoning effort at all, and the
		// template disables the control rather than offering choices that would no-op.
		"ImproveReasoningLevels": llm.ReasoningEffortLevels(v.ImproveModel),
		"Models":                 models,
		"FlexModels":             llm.SortModelsByFlexCost(models),
	}
}

// improveMaxRoundsOptions returns [1, llm.ImproveMaxRoundsCap] for the settings form's
// round-budget <select>.
func improveMaxRoundsOptions() []int {
	opts := make([]int, llm.ImproveMaxRoundsCap)
	for i := range opts {
		opts[i] = i + 1
	}
	return opts
}

// loadSettings fetches every stored setting in one Query and returns it as a key->value
// map, for callers that need several keys in one call instead of a separate GetSetting
// round trip per key. A free function (not a *server method) so improveAndFinalizeSuggestion
// (improve.go) can call it with just a *db.Store, the same reasoning improveExampleCap's doc
// comment gives for its own free-function shape.
func loadSettings(ctx context.Context, store *db.Store) map[string]string {
	all, _ := store.GetAllSettings(ctx)
	m := make(map[string]string, len(all))
	for _, st := range all {
		m[st.Key] = st.Value
	}
	return m
}

// settingOr returns settings[key], falling back to def when unset or empty.
func settingOr(settings map[string]string, key, def string) string {
	if v := settings[key]; v != "" {
		return v
	}
	return def
}

func (s *server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	settings := loadSettings(ctx, s.store)
	v := resolveSettings(settings, s.cfg.BedrockModel)
	models := s.cachedModels(ctx)
	s.fragmentResponse(w, "settings_form.html", settingsTemplateData(v, models), "")
}

// applyTierSetting reads formKey from r's form; if it's a recognized tier ("standard" or
// "flex") it's persisted under settingKey and returned, otherwise cur (the current setting)
// is returned unchanged. Shared by the classify/improve tier fields in handleUpdateSettings.
func (s *server) applyTierSetting(ctx context.Context, r *http.Request, formKey, settingKey, cur string) string {
	v := r.FormValue(formKey)
	if v != llm.TierStandard && v != llm.TierFlex {
		return cur
	}
	_ = s.store.SetSetting(ctx, db.SetSettingParams{Key: settingKey, Value: v})
	return v
}

// applyModelSetting reads formKey from r's form; if it names a model (via findModel) allowed
// on tier, it's persisted under settingKey and returned, otherwise cur is returned unchanged
// — garbage/disallowed input is silently ignored. Shared by the classify/improve model fields.
func (s *server) applyModelSetting(ctx context.Context, r *http.Request, formKey, settingKey, cur, tier string, findModel func(string) *llm.ModelOption) string {
	v := r.FormValue(formKey)
	if v == "" {
		return cur
	}
	m := findModel(v)
	if m == nil || !modelAllowedForTier(*m, tier) {
		return cur
	}
	_ = s.store.SetSetting(ctx, db.SetSettingParams{Key: settingKey, Value: v})
	return v
}

// applyIntSetting reads formKey from r's form; if it parses as an int in [min, max] it's
// persisted under settingKey and returned, otherwise cur (the current setting) is returned
// unchanged — same validate-or-ignore shape as applyTierSetting/applyModelSetting, for
// handleUpdateSettings' three numeric fields (improve_max_rounds, improve_example_cap,
// replay_example_cap), which used to hand-roll this three times.
func (s *server) applyIntSetting(ctx context.Context, r *http.Request, formKey, settingKey string, minVal, maxVal, cur int) int {
	v := r.FormValue(formKey)
	if v == "" {
		return cur
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < minVal || n > maxVal {
		return cur
	}
	_ = s.store.SetSetting(ctx, db.SetSettingParams{Key: settingKey, Value: v})
	return n
}

func (s *server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_ = r.ParseForm()
	settings := loadSettings(ctx, s.store)
	cur := resolveSettings(settings, s.cfg.BedrockModel)

	// Model selections — validate against available models to ignore garbage input
	models := s.cachedModels(ctx)
	findModel := func(id string) *llm.ModelOption {
		for i := range models {
			if models[i].ID == id {
				return &models[i]
			}
		}
		return nil
	}

	v := cur

	// Classification tier — "standard" or "flex". Must be resolved before validating the
	// classify_model choice: the two tiers have different eligible-model policies (see
	// modelAllowedForTier) enforced below.
	v.ClassifyTier = s.applyTierSetting(ctx, r, "classify_tier", llm.SettingClassifyTier, cur.ClassifyTier)
	v.ClassifyModel = s.applyModelSetting(ctx, r, "classify_model", llm.SettingClassifyModel, cur.ClassifyModel, v.ClassifyTier, findModel)

	// Prompt-improver tier + model — same standard/flex policy as classification above.
	v.ImproveTier = s.applyTierSetting(ctx, r, "improve_tier", llm.SettingImproveTier, cur.ImproveTier)
	v.ImproveModel = s.applyModelSetting(ctx, r, "improve_model", llm.SettingImproveModel, cur.ImproveModel, v.ImproveTier, findModel)

	// Reasoning-suppression override: free text, no validation beyond trimming — it's an
	// escape hatch for a model family modelCapabilities (llm/reasoning.go) doesn't know
	// about yet, so any soft-switch string the model expects is valid. Empty clears the
	// override and falls back to the table (see resolveSetting).
	v.ReasoningDirective = strings.TrimSpace(r.FormValue("classify_reasoning_directive"))
	_ = s.store.SetSetting(ctx, db.SetSettingParams{Key: llm.SettingClassifyReasoningDirective, Value: v.ReasoningDirective})

	// Checkbox semantics: the browser only sends improve_replay when checked, so its
	// presence (any value) means enabled — persisted as "1"/"0" to match
	// improveAndFinalizeSuggestion's (improve.go) unset-means-enabled default for this
	// setting.
	v.ImproveReplay = r.FormValue("improve_replay") != ""
	replayValue := "0"
	if v.ImproveReplay {
		replayValue = "1"
	}
	_ = s.store.SetSetting(ctx, db.SetSettingParams{Key: llm.SettingImproveReplay, Value: replayValue})

	// Round budget for the improve<->replay loop, and the example-selection caps
	// (improve.go's sampleExamples) — each validated the same way applyModelSetting/
	// applyTierSetting validate their own fields: a value outside range (garbage, blank,
	// out of bounds) is silently ignored, leaving the current setting in place.
	v.ImproveMaxRounds = s.applyIntSetting(ctx, r, "improve_max_rounds", llm.SettingImproveMaxRounds, 1, llm.ImproveMaxRoundsCap, cur.ImproveMaxRounds)
	v.ImproveExampleCap = s.applyIntSetting(ctx, r, "improve_example_cap", llm.SettingImproveExampleCap, 1, llm.ImproveExampleCapMax, cur.ImproveExampleCap)
	v.ReplayExampleCap = s.applyIntSetting(ctx, r, "replay_example_cap", llm.SettingReplayExampleCap, 1, llm.ReplayExampleCapMax, cur.ReplayExampleCap)

	// Reasoning effort for the improve call: accepted values depend on improveModel's family
	// (llm.ReasoningEffortLevels) — not a fixed four, since most families support none of
	// them and GLM only really supports one. Anything else (including a level valid for a
	// different model than the one now selected) is ignored the same way applyModelSetting
	// ignores an unrecognized model id.
	if val := r.FormValue("improve_reasoning_effort"); val == llm.ReasoningEffortOff || slices.Contains(llm.ReasoningEffortLevels(v.ImproveModel), val) {
		v.ImproveReasoningEffort = val
		_ = s.store.SetSetting(ctx, db.SetSettingParams{Key: llm.SettingImproveReasoningEffort, Value: val})
	}

	s.fragmentResponse(w, "settings_form.html", settingsTemplateData(v, models), "Settings saved")
}

// ============================================================
// Logs
// ============================================================

func (s *server) handleLogs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logs, _ := s.store.GetLogs(ctx, 100)
	s.render(w, "logs_list", logs)
}

// ============================================================
// History
// ============================================================

type historyRow struct {
	ID             int64
	AccountID      int64  // for the bulk-select checkbox's data-aid (see history_table.html)
	MessageID      string // for the bulk-select checkbox's data-mid
	Timestamp      string
	AccountEmail   string
	Subject        string
	Sender         string
	PromptName     string
	LabelName      string
	ExtraActions   []string
	HasLlmResponse bool
}

// historyTableView is what history_table.html renders: one page of rows plus enough to
// drive infinite scroll. NextURL is the sentinel row's hx-get target — "" means either
// this is the last page or HistoryMaxLimit (Truncated) was reached, and no sentinel
// should be rendered. FirstPage gates the "No matching history found" empty state, so an
// empty *later* page (a sparse filter can legitimately return zero matches on a given
// page — see GetHistoryFiltered's doc comment) doesn't render it mid-scroll.
type historyTableView struct {
	Rows      []historyRow
	NextURL   string
	FirstPage bool
	Truncated bool
	MaxLimit  int // only meaningful when Truncated; shown in the terminal row's message

	// MoreURL turns Truncated from a dead end into a "Search older history" button: the
	// same resume URL the sentinel would have used, but with the row budget reset and
	// triggered by a click instead of by scrolling into view. That distinction is the point
	// — hitting the ceiling means this search has already read HistoryMaxLimit rows without
	// filling a page, so the next batch of reads should be something the user asks for.
	// Without it a search simply could not reach an email older than the ceiling.
	MoreURL string
}

func (s *server) handleHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()

	firstPage := q.Get("cursor") == ""
	var loaded int64
	if v := q.Get("loaded"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			loaded = n
		}
	}

	filter := db.HistoryFilter{
		Cursor: q.Get("cursor"),
	}
	if v := q.Get("account_id"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			filter.AccountID = &id
		}
	}
	if v := q.Get("prompt_id"); v == "none" {
		filter.Unmatched = true
	} else if v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			filter.PromptID = &id
		}
	}
	filter.SubjectQ = q.Get("subject")
	filter.SenderQ = q.Get("sender")

	view := historyTableView{FirstPage: firstPage, MaxLimit: s.cfg.HistoryMaxLimit}
	limit, ceilingHit := historyPageLimit(s.cfg.HistoryPageSize, s.cfg.HistoryMaxLimit, loaded)
	if ceilingHit {
		// Unreachable through the UI — the ceiling is now detected after a page is rendered
		// (below), which hands back a MoreURL with loaded reset to 0 rather than a URL
		// already past the budget. Kept for a hand-edited or stale link, and it offers the
		// same continue button so that case isn't a dead end either.
		view.Truncated = true
		if cur := q.Get("cursor"); cur != "" {
			view.MoreURL = historyNextURL(q, cur, 0)
		}
		s.fragmentResponse(w, "history_table.html", view, "")
		return
	}
	filter.Limit = limit

	page, err := s.store.GetHistoryFiltered(ctx, filter)
	if err != nil {
		slog.Error("history query", "err", err)
	}

	view.Rows = make([]historyRow, len(page.Rows))
	for i, h := range page.Rows {
		view.Rows[i] = historyRow{
			ID:             h.ID,
			AccountID:      h.AccountID,
			MessageID:      h.MessageID,
			Timestamp:      h.Timestamp,
			AccountEmail:   h.AccountEmail,
			Subject:        h.Subject,
			Sender:         h.Sender,
			PromptName:     strOrEmpty(h.PromptName),
			LabelName:      strOrEmpty(h.LabelName),
			HasLlmResponse: h.LlmResponse != "",
		}
		if h.Actions != "" {
			view.Rows[i].ExtraActions = strings.Split(h.Actions, ",")
		}
	}

	// The budget counts rows *scanned*, not rows matched. With no filters those are the same
	// number, so plain browsing still stops at HistoryMaxLimit rows exactly as before; with
	// a subject/sender search they diverge sharply, and counting matches would let a search
	// that matches nothing scroll forever, one near-empty page of DynamoDB reads at a time.
	newLoaded := loaded + page.Scanned
	if page.NextCursor != "" {
		if newLoaded >= int64(s.cfg.HistoryMaxLimit) {
			view.Truncated = true
			// loaded=0: the click starts a fresh budget from this cursor, so older matches
			// stay reachable a batch at a time instead of being walled off.
			view.MoreURL = historyNextURL(q, page.NextCursor, 0)
		} else {
			view.NextURL = historyNextURL(q, page.NextCursor, newLoaded)
		}
	}
	s.fragmentResponse(w, "history_table.html", view, "")
}

// historyPageLimit decides this request's DynamoDB Limit given the configured page size,
// the HistoryMaxLimit ceiling on rows loaded across an entire scroll session, and how
// many rows have already been loaded so far. ceilingHit means the ceiling was already
// reached before this request — the caller should render a Truncated terminal row
// without querying at all, rather than a (potentially zero-size) Limit.
func historyPageLimit(pageSize, maxLimit int, loaded int64) (limit int64, ceilingHit bool) {
	remaining := int64(maxLimit) - loaded
	if remaining <= 0 {
		return 0, true
	}
	limit = min(int64(pageSize), remaining)
	return limit, false
}

// historyNextURL builds the infinite-scroll sentinel's hx-get target: the current
// request's query values with cursor/loaded overwritten to resume after this page. q is
// not mutated — url.Values is a map, so mutating it in place would also change the
// filters the caller already read from it.
func historyNextURL(q url.Values, cursor string, loaded int64) string {
	next := url.Values{}
	maps.Copy(next, q)
	next.Set("cursor", cursor)
	next.Set("loaded", strconv.FormatInt(loaded, 10))
	return "/fragments/history?" + next.Encode()
}

func (s *server) handleTroubleshooting(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := s.store.GetLatestLlmDebug(ctx)
	if err != nil {
		slog.Error("troubleshooting query", "err", err)
		rows = nil
	}
	s.fragmentResponse(w, "troubleshooting_list.html", rows, "")
}

func (s *server) handleHistoryFilters(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accounts, _ := s.store.ListAccountsSafe(ctx)
	prompts, _ := s.store.ListPrompts(ctx)

	type promptOption struct {
		ID   int64
		Name string
	}
	options := make([]promptOption, len(prompts))
	for i, p := range prompts {
		options[i] = promptOption{ID: p.ID, Name: p.Name}
	}

	s.fragmentResponse(w, "history_filters.html", map[string]any{
		"Accounts": toAccountViews(accounts),
		"Prompts":  options,
	}, "")
}

func (s *server) handleHistoryLlmResponse(w http.ResponseWriter, r *http.Request) {
	id, ok := requireID(w, r)
	if !ok {
		return
	}
	resp, err := s.store.GetHistoryLlmResponse(r.Context(), id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<pre class="mt-1 p-1 border rounded bg-body-secondary font-monospace" style="font-size:.7rem;max-width:100%%;white-space:pre-wrap;word-break:break-all;">%s</pre>`,
		template.HTMLEscapeString(resp))
}

// ============================================================
// Retention
// ============================================================

type retentionPanelData struct {
	AccountID                   int64
	GlobalEnabled               bool
	GlobalValue                 string
	GlobalUnit                  string
	Exemptions                  []db.LabelExemption
	LabelRules                  []db.LabelRetention
	AvailableLabelsForExemption []string
	AvailableLabelsForRules     []string
}

func (s *server) buildRetentionData(ctx context.Context, accountID int64) retentionPanelData {
	data := retentionPanelData{AccountID: accountID, GlobalUnit: "days"}

	ret, err := s.store.GetAccountRetention(ctx, accountID)
	if err == nil && ret.GlobalDays.Valid {
		data.GlobalEnabled = true
		gd := ret.GlobalDays.Int64
		if gd >= 365 && gd%365 == 0 {
			data.GlobalUnit = retentionUnitYears
			data.GlobalValue = strconv.FormatInt(gd/365, 10)
		} else {
			data.GlobalValue = strconv.FormatInt(gd, 10)
		}
	}

	data.Exemptions, _ = s.store.GetLabelExemptions(ctx, accountID)
	data.LabelRules, _ = s.store.GetLabelRetention(ctx, accountID)

	return data
}

// renderRetentionPanel loads the retention data (with Gmail label options) for accountID
// and renders the shared retention_panel fragment. Shared by every retention handler below
// so the template path and build-then-render sequence can't drift between them.
func (s *server) renderRetentionPanel(w http.ResponseWriter, ctx context.Context, accountID int64, toast string) {
	data := s.buildRetentionDataWithGmail(ctx, accountID)
	s.fragmentResponse(w, "retention_panel.html", data, toast)
}

// daysFromForm parses the "value"/"unit" retention form fields into a day count, treating
// unit=="years" as 365 days/year. Shared by the global- and label-retention handlers so the
// years-to-days convention is defined once.
func daysFromForm(r *http.Request) int64 {
	val, _ := strconv.ParseInt(r.FormValue("value"), 10, 64)
	if r.FormValue("unit") == retentionUnitYears {
		return val * 365
	}
	return val
}

func (s *server) handleGetRetention(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	s.renderRetentionPanel(w, r.Context(), id, "")
}

func (s *server) handleSetGlobalRetention(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	ctx := r.Context()
	_ = r.ParseForm()

	if r.FormValue("enabled") == "1" {
		days := daysFromForm(r)
		if days > 0 {
			_ = s.store.SetGlobalRetention(ctx, db.SetGlobalRetentionParams{AccountID: id, GlobalDays: sql.NullInt64{Int64: days, Valid: true}})
		}
	} else {
		_ = s.store.ClearGlobalRetention(ctx, id)
	}
	s.renderRetentionPanel(w, ctx, id, "Saved")
}

func (s *server) handleAddLabelRetention(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	ctx := r.Context()
	_ = r.ParseForm()
	label := strings.TrimSpace(r.FormValue("label_name"))
	days := daysFromForm(r)
	if label != "" && days > 0 {
		_ = s.store.AddLabelRetention(ctx, db.AddLabelRetentionParams{AccountID: id, LabelName: label, Days: days})
	}
	s.renderRetentionPanel(w, ctx, id, "Rule added")
}

func (s *server) handleDeleteLabelRetention(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	ruleID := pathInt(r, "ruleId")
	ctx := r.Context()
	_ = s.store.DeleteLabelRetention(ctx, db.DeleteLabelRetentionParams{ID: ruleID, AccountID: id})
	s.renderRetentionPanel(w, ctx, id, "Rule removed")
}

func (s *server) handleAddExemption(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	ctx := r.Context()
	_ = r.ParseForm()
	label := strings.TrimSpace(r.FormValue("label_name"))
	if label != "" {
		_ = s.store.AddLabelExemption(ctx, db.AddLabelExemptionParams{AccountID: id, LabelName: label})
	}
	s.renderRetentionPanel(w, ctx, id, "Exemption added")
}

func (s *server) handleDeleteExemption(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	eid := pathInt(r, "eid")
	ctx := r.Context()
	_ = s.store.DeleteLabelExemption(ctx, db.DeleteLabelExemptionParams{ID: eid, AccountID: id})
	s.renderRetentionPanel(w, ctx, id, "Exemption removed")
}

func (s *server) handleRetentionQuery(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("account_id")
	if idStr == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id, _ := strconv.ParseInt(idStr, 10, 64)
	if id == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.renderRetentionPanel(w, r.Context(), id, "")
}

// gmailServiceFor builds an authenticated Gmail client for account, wiring credential
// refresh back to the store. Centralizes the OAuth-config + NewService + refresh-closure
// boilerplate otherwise repeated at every call site.
func (s *server) gmailServiceFor(ctx context.Context, account db.Account) (*gmail.Client, error) {
	return processor.NewAccountGmailService(ctx, s.store, s.auth, account)
}

func (s *server) buildRetentionDataWithGmail(ctx context.Context, accountID int64) retentionPanelData {
	data := s.buildRetentionData(ctx, accountID)

	// Try to fetch Gmail labels for the dropdown
	account, err := s.store.GetAccount(ctx, accountID)
	if err != nil {
		return data // graceful: no labels, return empty dropdowns
	}
	svc, err := s.gmailServiceFor(ctx, account)
	if err != nil {
		return data // graceful: credentials/oauth unavailable
	}
	labels, err := gmail.ListLabels(ctx, svc)
	if err != nil {
		return data // graceful: label fetch failure
	}

	exemptSet := map[string]bool{}
	for _, e := range data.Exemptions {
		exemptSet[strings.ToLower(e.LabelName)] = true
	}
	ruleSet := map[string]bool{}
	for _, r := range data.LabelRules {
		ruleSet[strings.ToLower(r.LabelName)] = true
	}

	for _, l := range labels {
		lower := strings.ToLower(l.Name)
		if !exemptSet[lower] {
			data.AvailableLabelsForExemption = append(data.AvailableLabelsForExemption, l.Name)
		}
		if !ruleSet[lower] {
			data.AvailableLabelsForRules = append(data.AvailableLabelsForRules, l.Name)
		}
	}

	return data
}

// ============================================================
// OAuth
// ============================================================

// oauthPending is an in-flight OAuth attempt: the PKCE verifier minted alongside the
// state token, and when the attempt expires.
type oauthPending struct {
	verifier string
	expires  time.Time
}

func (s *server) handleOAuthStart(w http.ResponseWriter, _ *http.Request) {
	state := generateToken(16)
	verifier := gmail.GenerateVerifier()
	s.oauthMu.Lock()
	now := time.Now()
	for k, p := range s.oauthState {
		if now.After(p.expires) {
			delete(s.oauthState, k)
		}
	}
	s.oauthState[state] = oauthPending{verifier: verifier, expires: now.Add(10 * time.Minute)}
	s.oauthMu.Unlock()

	authURL, err := s.auth.GetAuthURL(state, verifier)
	if err != nil {
		http.Error(w, "Could not generate auth URL: "+err.Error(), http.StatusInternalServerError)
		return
	}
	data := map[string]string{"AuthURL": authURL}
	s.fragmentResponse(w, "oauth_step2.html", data, "")
}

func (s *server) handleOAuthExchange(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_ = r.ParseForm()
	rawURL := r.FormValue("url")
	parsed, err := url.Parse(rawURL)
	if err != nil {
		s.fragmentResponse(w, "accounts_list.html", nil, "Invalid URL")
		return
	}
	code := parsed.Query().Get("code")
	state := parsed.Query().Get("state")

	s.oauthMu.Lock()
	pending, ok := s.oauthState[state]
	if ok {
		delete(s.oauthState, state)
	}
	s.oauthMu.Unlock()

	if !ok || time.Now().After(pending.expires) {
		s.fragmentResponse(w, "accounts_list.html", nil, "OAuth state expired — try again")
		return
	}

	emailAddr, credJSON, err := s.auth.ExchangeCode(ctx, code, pending.verifier)
	if err != nil {
		slog.Error("oauth exchange", "err", err)
		s.fragmentResponse(w, "accounts_list.html", nil, "OAuth failed: "+err.Error())
		return
	}

	accountID, err := s.store.UpsertAccount(ctx, db.UpsertAccountParams{Email: emailAddr, CredentialsJSON: credJSON})
	if err != nil {
		slog.Error("upsert account", "err", err)
	} else {
		// Register the Gmail push subscription so this account gets real-time
		// notifications immediately (not just after the next scheduled renewal).
		s.startWatch(ctx, accountID, credJSON)
	}

	s.handleAccounts(w, r)
}

// startWatch registers a Gmail push subscription for a freshly authorized account.
// No-op when Pub/Sub isn't configured (e.g. local dev). Failures are logged, not fatal —
// the scheduled scan renews watches and still catches mail as a fallback.
func (s *server) startWatch(ctx context.Context, accountID int64, credJSON string) {
	if s.cfg.PubSubTopic == "" {
		return
	}
	svc, err := s.gmailServiceFor(ctx, db.Account{ID: accountID, CredentialsJSON: credJSON})
	if err != nil {
		slog.Error("watch: gmail service", "err", err)
		return
	}
	res, err := svc.Watch(ctx, s.cfg.PubSubTopic)
	if err != nil {
		slog.Error("watch: register", "err", err)
		return
	}
	_ = s.store.UpdateAccountWatch(ctx, db.UpdateAccountWatchParams{
		ID: accountID, HistoryID: res.HistoryID, Expiration: res.Expiration,
	})
}

// ============================================================
// Scan
// ============================================================

func (s *server) handleScan(w http.ResponseWriter, r *http.Request) {
	s.store.Log("INFO", "Manual scan triggered")
	scanOnce(r.Context(), s.store, s.llm, s.auth, s.cfg)
	setHxTrigger(w, map[string]any{
		triggerShowToast:   map[string]any{toastKeyMessage: "Scan complete", jsonKeyType: toastTypeSuccess},
		"refreshDashboard": "",
	})
	w.WriteHeader(http.StatusOK)
}

// ============================================================
// Account options (dropdown)
// ============================================================

func (s *server) handleAccountOptions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accounts, _ := s.store.ListAccountsSafe(ctx)

	optType := r.URL.Query().Get("type")
	var firstOption template.HTML
	switch optType {
	case "filter":
		firstOption = template.HTML(`<option value="">All accounts</option>`)
	case "retention":
		firstOption = template.HTML(`<option value="">Select account…</option>`)
	default:
		firstOption = template.HTML(`<option value="">All accounts (global)</option>`)
	}

	s.fragmentResponse(w, "account_options.html", map[string]any{
		"FirstOption": firstOption,
		"Accounts":    toAccountViews(accounts),
	}, "")
}

// ============================================================
// JSON APIs
// ============================================================

func (s *server) handleReorderPrompts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		OrderedIDs []int64 `json:"ordered_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if err := s.store.ReorderPrompts(ctx, body.OrderedIDs); err != nil {
		http.Error(w, "reorder failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *server) handleExportPrompts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	prompts, _ := s.store.ListPrompts(ctx)
	w.Header().Set("Content-Disposition", "attachment; filename=prompts.json")
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, prompts)
}

type configExport struct {
	Accounts  []db.Account       `json:"accounts"`
	Prompts   []db.Prompt        `json:"prompts"`
	Settings  []db.Setting       `json:"settings"`
	Retention []accountRetExport `json:"retention"`
}

type accountRetExport struct {
	AccountEmail string              `json:"account_email"`
	GlobalDays   *int64              `json:"global_days,omitempty"`
	Labels       []db.LabelRetention `json:"labels"`
	Exemptions   []db.LabelExemption `json:"exemptions"`
}

func (s *server) handleExportConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accounts, _ := s.store.ListAccounts(ctx)
	prompts, _ := s.store.ListPrompts(ctx)
	allSettings, _ := s.store.GetAllSettings(ctx)
	var settings []db.Setting
	for _, setting := range allSettings {
		if setting.Key == "secret_key" {
			continue
		}
		settings = append(settings, setting)
	}

	// Strip credentials from export
	safeAccounts := make([]db.Account, len(accounts))
	for i, a := range accounts {
		a.CredentialsJSON = ""
		safeAccounts[i] = a
	}

	retentions := make([]accountRetExport, 0, len(accounts))
	for _, a := range accounts {
		entry := accountRetExport{AccountEmail: a.Email}
		ret, err := s.store.GetAccountRetention(ctx, a.ID)
		if err == nil && ret.GlobalDays.Valid {
			entry.GlobalDays = &ret.GlobalDays.Int64
		}
		entry.Labels, _ = s.store.GetLabelRetention(ctx, a.ID)
		entry.Exemptions, _ = s.store.GetLabelExemptions(ctx, a.ID)
		retentions = append(retentions, entry)
	}

	w.Header().Set("Content-Disposition", "attachment; filename=ollamail-config.json")
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, configExport{
		Accounts:  safeAccounts,
		Prompts:   prompts,
		Settings:  settings,
		Retention: retentions,
	})
}

func (s *server) handleImportConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_ = r.ParseMultipartForm(10 << 20)
	file, _, err := r.FormFile("file")
	if err != nil {
		jsonError(w, "no file", 400)
		return
	}
	defer func() { _ = file.Close() }()

	var cfg configExport
	if err := json.NewDecoder(file).Decode(&cfg); err != nil { //nolint:musttag // sqlc-generated nested structs lack json tags by design
		jsonError(w, "invalid JSON", 400)
		return
	}

	// One partition read for every existing global prompt's name, instead of
	// PromptExistsGlobal's own full PROMPT partition scan once per imported prompt — an
	// import of 40 prompts used to cost 40 full partition reads to check a name set that's
	// static except for what this very loop adds to it (see the map update below, which
	// keeps a global-named duplicate within the same import file still caught, matching
	// PromptExistsGlobal's original per-iteration re-query).
	existingGlobalNames := make(map[string]bool)
	if existing, err := s.store.ListPrompts(ctx); err == nil {
		for _, p := range existing {
			if p.AccountID == nil {
				existingGlobalNames[p.Name] = true
			}
		}
	}

	imported := 0
	for _, p := range cfg.Prompts {
		if existingGlobalNames[p.Name] {
			continue
		}
		var accountID sql.NullInt64
		if p.AccountID != nil {
			accountID = sql.NullInt64{Int64: *p.AccountID, Valid: true}
		}
		_, _ = s.store.CreatePrompt(ctx, db.CreatePromptParams{
			Name:           p.Name,
			Instructions:   p.Instructions,
			LabelName:      p.LabelName,
			ActionArchive:  p.ActionArchive,
			ActionSpam:     p.ActionSpam,
			ActionTrash:    p.ActionTrash,
			ActionMarkRead: p.ActionMarkRead,
			SortOrder:      p.SortOrder,
			StopProcessing: p.StopProcessing,
			AccountID:      accountID,
		})
		if p.AccountID == nil {
			existingGlobalNames[p.Name] = true
		}
		imported++
	}
	for _, setting := range cfg.Settings {
		if setting.Key == "secret_key" {
			continue
		}
		_ = s.store.SeedSetting(setting.Key, setting.Value)
	}

	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, map[string]any{"imported": imported})
}

func (s *server) handleDownloadLogs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	start := q.Get("start")
	end := q.Get("end")

	var logs []db.Log
	if start != "" && end != "" {
		// Convert datetime-local (2006-01-02T15:04) to DB format
		start = strings.Replace(start, "T", " ", 1) + ":00"
		end = strings.Replace(end, "T", " ", 1) + ":00"
		logs, _ = s.store.GetLogsRange(ctx, db.GetLogsRangeParams{Timestamp: start, Timestamp2: end})
	} else {
		logs, _ = s.store.GetLogs(ctx, 10000)
	}

	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", "attachment; filename=logs.csv")
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"timestamp", "level", "message"})
	for _, l := range logs {
		_ = cw.Write([]string{l.Timestamp, l.Level, l.Message})
	}
	cw.Flush()
}

func (s *server) handleGenerateStream(w http.ResponseWriter, r *http.Request) {
	description := r.URL.Query().Get("description")
	if description == "" {
		http.Error(w, "description required", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	ch := s.llm.StreamGeneratePromptInstruction(r.Context(), description)
	for chunk := range ch {
		if chunk.Err != nil {
			writeSSEEvent(w, flusher, "error", chunk.Err.Error())
			break
		}
		if chunk.Reasoning != "" {
			writeSSEEvent(w, flusher, "reasoning", chunk.Reasoning)
		}
		if chunk.Text != "" {
			writeSSEEvent(w, flusher, "content", chunk.Text)
		}
	}
	_, _ = fmt.Fprintf(w, "data: {\"type\":\"done\"}\n\n")
	flusher.Flush()
}

// ============================================================
// Prompt Suggestions
// ============================================================

type suggestionView struct {
	ID                    int64
	CreatedAt             string
	UpdatedAt             string
	PromptID              int64
	PromptName            string
	TriggerKind           string
	EmailSubject          string
	EmailSender           string
	EmailBodySnapshot     string
	OriginalInstructions  string
	SuggestedInstructions string
	UserComment           string
	Status                string

	// Detail-view only (populated by suggestionDetailView, left zero-value by the compact
	// list view in handlePromptSuggestionsList — that view never renders them, and fetching
	// a rule's example corpus for every card in the list would be N*3 extra queries for
	// nothing shown). ExampleGroups is the corpus the improver/replay actually used, in
	// place of the single mishandled-email snapshot the pre-corpus UI showed.
	ExampleGroups  []exampleGroup
	ReplayModel    string
	ReplayTotal    int64
	ReplayPassed   int64
	ReplayBaseline int64
	ReplayFailures []llm.ReplayFailure

	// Rounds is the improve<->replay trajectory (improve.go's loop), parsed from
	// PromptSuggestion.RoundsJSON — empty for a suggestion generated before the loop
	// existed, or one with only a single round (the template only bothers showing this
	// when there's more than one attempt to compare). BestRound is which entry won.
	Rounds    []db.SuggestionRoundSummary
	BestRound int64
}

// exampleGroup is one verdict's worth of a rule's example corpus, labeled for display.
// Shared by prompt_suggestion_detail.html (the curated set the improver saw) and
// prompt_examples_list.html (the newest rows of the live corpus, on the prompt card).
type exampleGroup struct {
	Verdict  string
	Label    string
	Examples []db.PromptExample

	// More reports that the corpus holds further rows past Examples. Only the prompt-card
	// list sets it — the suggestion detail view shows exactly what went into the improve
	// call, so "there are more" would be meaningless there.
	More bool
}

// verdictLabels turns a stored verdict into the phrasing both example views show. Written
// from the rule's point of view rather than as the raw constant.
var verdictLabels = map[string]string{
	db.VerdictConfirmedPositive: "Should match (confirmed by you)",
	db.VerdictConfirmedNegative: "Should not match (confirmed by you)",
}

// toSuggestionView converts a stored suggestion + its prompt's name into the view shape
// rendered by the prompt-suggestion templates. Shared by all three suggestion handlers
// below so the 13-field mapping can't drift between them; callers that need a value other
// than the suggestion's own (e.g. a freshly regenerated UpdatedAt/Status) overwrite the
// specific field on the returned view.
func toSuggestionView(sg db.PromptSuggestion, promptName string) suggestionView {
	return suggestionView{
		ID:                    sg.ID,
		CreatedAt:             sg.CreatedAt,
		UpdatedAt:             sg.UpdatedAt,
		PromptID:              sg.PromptID,
		PromptName:            promptName,
		TriggerKind:           sg.TriggerKind,
		EmailSubject:          sg.EmailSubject,
		EmailSender:           sg.EmailSender,
		EmailBodySnapshot:     sg.EmailBodySnapshot,
		OriginalInstructions:  sg.OriginalInstructions,
		SuggestedInstructions: sg.SuggestedInstructions,
		UserComment:           sg.UserComment,
		Status:                sg.Status,
	}
}

// suggestionDetailView builds the detail-page view for a suggestion: the base fields plus
// its rule's example corpus (the same curated set selectExamplesForImprove fed into the
// improve call — deliberately not the larger set ReplayAgainstExamples was scored against,
// so this page shows exactly what the model saw) and the stored replay result, if any
// (ReplayTotal == 0 means replay didn't run — improve_replay disabled, or an older
// suggestion from before this field existed — and the template renders nothing for that
// case rather than a misleading "0/0").
func (s *server) suggestionDetailView(ctx context.Context, sg db.PromptSuggestion, promptName string) suggestionView {
	view := toSuggestionView(sg, promptName)

	examples := selectExamplesForImprove(ctx, s.store, sg.PromptID)
	for _, v := range db.VerdictOrder {
		var grouped []db.PromptExample
		for _, ex := range examples {
			if ex.Verdict == v {
				grouped = append(grouped, ex)
			}
		}
		if len(grouped) > 0 {
			view.ExampleGroups = append(view.ExampleGroups, exampleGroup{Verdict: v, Label: verdictLabels[v], Examples: grouped})
		}
	}

	view.ReplayModel = sg.ReplayModel
	view.ReplayTotal = sg.ReplayTotal
	view.ReplayPassed = sg.ReplayPassed
	view.ReplayBaseline = sg.ReplayBaseline
	if sg.ReplayFailures != "" {
		_ = json.Unmarshal([]byte(sg.ReplayFailures), &view.ReplayFailures)
	}
	view.BestRound = sg.BestRound
	if sg.RoundsJSON != "" {
		_ = json.Unmarshal([]byte(sg.RoundsJSON), &view.Rounds)
	}
	return view
}

// suggestionsListView wraps the suggestion cards for prompt_suggestions_list.html with the
// poll interval the fragment's own self-replacing driver (#suggestions-poll) should use —
// see suggestionsPollInterval.
type suggestionsListView struct {
	Items     []suggestionView
	PollEvery string
}

// suggestionsPollInterval picks the list's next poll cadence: fast while anything is still
// generating, so a finished round (or a newly seeded one) is picked up quickly without
// anyone needing to have a detail trace open; slow otherwise, since there's nothing to
// watch for. Pure and unit-tested (server_test.go) independent of the *db.Store this
// handler otherwise needs.
func suggestionsPollInterval(views []suggestionView) string {
	for _, v := range views {
		if v.Status == db.SuggestionStatusGenerating {
			return "5s"
		}
	}
	return "60s"
}

func (s *server) handlePromptSuggestionsList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	suggestions, _ := s.store.ListPromptSuggestions(ctx)

	// Load all prompts for name lookup
	allPrompts, _ := s.store.ListPrompts(ctx)
	promptNames := make(map[int64]string, len(allPrompts))
	for _, p := range allPrompts {
		promptNames[p.ID] = p.Name
	}

	views := make([]suggestionView, len(suggestions))
	for i, sg := range suggestions {
		views[i] = toSuggestionView(sg, promptNames[sg.PromptID])
	}
	s.fragmentResponse(w, "prompt_suggestions_list.html", suggestionsListView{
		Items:     views,
		PollEvery: suggestionsPollInterval(views),
	}, "")
}

// suggestionTraceResponse is the trace endpoint's JSON body. The browser's polling loop
// (static/app.js) appends Events to the live pane, advances its cursor to LastSeq, and
// stops polling once Status is no longer "generating" — see handlePromptSuggestionTrace.
type suggestionTraceResponse struct {
	Status  string                    `json:"status"`
	LastSeq int64                     `json:"lastSeq"`
	Stalled bool                      `json:"stalled"`
	Events  []db.SuggestionTraceEvent `json:"events"`
}

// handlePromptSuggestionTrace serves a suggestion's live progress log with a seq cursor
// (?after=N) — this, not SSE or a WebSocket, is what makes "Generating…" observable: the
// improve worker (a separate Lambda, see improve.go's package doc comment) has no channel
// back to WebFunction, but both write to and read from the same DynamoDB table, so polling
// this endpoint is the only connection between them that doesn't require re-architecting
// where the long-running work happens. Returns JSON, not an HTML fragment — the frontend
// appends deltas into existing DOM nodes rather than re-rendering the whole pane on every
// poll (see prompt_suggestion_detail.html's generating branch).
func (s *server) handlePromptSuggestionTrace(w http.ResponseWriter, r *http.Request) {
	id, ok := requireID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	after := parseTraceAfter(r.URL.Query().Get("after"))

	sg, err := s.store.GetPromptSuggestion(ctx, id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	events, err := s.store.ListSuggestionTrace(ctx, id, after)
	if err != nil {
		http.Error(w, "trace query failed", http.StatusInternalServerError)
		return
	}
	lastSeq := traceLastSeq(after, events)
	// Best-effort: a staleness-check error just means this particular poll doesn't report
	// stalled — the next poll tries again, and the hard 20-minute backstop
	// (generatingStaleAfter, db/store.go) still applies regardless.
	stalled, _ := s.store.IsSuggestionTraceStale(ctx, sg)

	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, suggestionTraceResponse{
		Status:  sg.Status,
		LastSeq: lastSeq,
		Stalled: stalled,
		Events:  events,
	})
}

// parseTraceAfter parses the trace endpoint's ?after= cursor, defaulting to 0 (the
// beginning) for anything missing or malformed — a bad cursor value should replay the
// whole trace, not 400. Factored out so its edge cases (empty, negative, garbage) are
// unit-testable without a *db.Store — see this file's history-pagination tests for the
// same rationale (server.store is a concrete *db.Store, not db.StoreIface, so a full
// handler test would need a much wider fake than this one value's parsing warrants).
func parseTraceAfter(v string) int64 {
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// traceLastSeq computes the cursor the browser should poll with next: the highest Seq
// across the returned events, or after unchanged if the page was empty (nothing new since
// last poll — the cursor must not regress).
func traceLastSeq(after int64, events []db.SuggestionTraceEvent) int64 {
	lastSeq := after
	for _, e := range events {
		if e.Seq > lastSeq {
			lastSeq = e.Seq
		}
	}
	return lastSeq
}

func (s *server) handlePromptSuggestionDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := requireID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	sg, err := s.store.GetPromptSuggestion(ctx, id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	p, _ := s.store.GetPrompt(ctx, sg.PromptID)
	view := s.suggestionDetailView(ctx, sg, p.Name)
	s.fragmentResponse(w, "prompt_suggestion_detail.html", view, "")
}

// handlePromptSuggestionRegenerate kicks off a fresh improve+replay round for an existing
// suggestion, async via the improve worker (see dispatchImprove, improve.go) — a full round
// (improve call plus ~30 replay classify calls) can't run synchronously in the request.
// Returns the same 'generating' view the detail page already renders with a spinner for a
// brand-new suggestion (prompt_suggestion_detail.html); its own poll while generating (see
// prompt_suggestion_detail.html) picks up the finished result.
func (s *server) handlePromptSuggestionRegenerate(w http.ResponseWriter, r *http.Request) {
	id, ok := requireID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	userComment := r.FormValue("user_comment")

	sg, err := s.store.GetPromptSuggestion(ctx, id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	p, err := s.store.GetPrompt(ctx, sg.PromptID)
	if err != nil {
		http.Error(w, "prompt not found", http.StatusNotFound)
		return
	}

	var conv []llm.ChatMessage
	if sg.ConversationJSON != "" && sg.ConversationJSON != "[]" {
		_ = json.Unmarshal([]byte(sg.ConversationJSON), &conv)
	}

	if err := s.store.MarkPromptSuggestionGenerating(ctx, id); err != nil {
		http.Error(w, "regenerate failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.dispatchImprove(ctx, []improveTarget{{
		SuggestionID:         id,
		PromptID:             sg.PromptID,
		OriginalInstructions: sg.OriginalInstructions,
		PriorConversation:    conv,
		UserComment:          userComment,
	}})

	view := toSuggestionView(sg, p.Name)
	view.Status = db.SuggestionStatusGenerating
	s.fragmentResponse(w, "prompt_suggestion_detail.html", view, "")
}

func (s *server) handlePromptSuggestionApply(w http.ResponseWriter, r *http.Request) {
	id, ok := requireID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	sg, err := s.store.GetPromptSuggestion(ctx, id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// Which false_negative/false_positive examples this suggestion was built from — marks
	// them resolved once applied, so future improve rounds for this rule stop re-showing
	// problems it already fixed (see db.PromptExample.ResolvedBySuggestionID). Empty for
	// suggestions generated before this field existed; that's fine, there's nothing to mark.
	var problemKeys []db.ResolvedExampleKey
	if sg.ProblemExampleKeys != "" {
		_ = json.Unmarshal([]byte(sg.ProblemExampleKeys), &problemKeys)
	}
	if err := s.store.ApplyPromptSuggestionAndUpdatePrompt(ctx, sg, problemKeys); err != nil {
		http.Error(w, "apply failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	setHxTrigger(w, map[string]any{triggerRefreshSuggestionBadge: "1"})
	w.WriteHeader(http.StatusOK)
}

func (s *server) handlePromptSuggestionDismiss(w http.ResponseWriter, r *http.Request) {
	id, ok := requireID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	_ = s.store.DismissPromptSuggestion(ctx, id)
	setHxTrigger(w, map[string]any{triggerRefreshSuggestionBadge: "1"})
	w.WriteHeader(http.StatusOK)
}

// ============================================================
// Label pre-creation
// ============================================================

func (s *server) ensureLabelForAccounts(ctx context.Context, labelName string, accountID sql.NullInt64) {
	if labelName == "" {
		return
	}
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		return
	}
	for _, account := range accounts {
		if accountID.Valid && accountID.Int64 != account.ID {
			continue
		}
		svc, err := s.gmailServiceFor(ctx, account)
		if err != nil {
			continue
		}
		_ = gmail.EnsureLabel(ctx, svc, labelName)
	}
}

// ============================================================
// Helpers
// ============================================================

func pathInt(r *http.Request, key string) int64 {
	v := r.PathValue(key)
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

// requireID reads the "id" path parameter, writing a 400 and returning ok=false if it's
// missing or non-numeric (pathInt returns 0). Shared by the handlers that take a single
// numeric path id and reject a bad one outright, so the 400 guard can't drift between
// them. (Other handlers read id/eid/ruleId via pathInt directly and fall through to a
// downstream lookup instead of a hard 400 — that's a different, intentional behavior.)
func requireID(w http.ResponseWriter, r *http.Request) (id int64, ok bool) {
	id = pathInt(r, "id")
	if id == 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func generateToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	writeJSON(w, map[string]string{"error": msg})
}
