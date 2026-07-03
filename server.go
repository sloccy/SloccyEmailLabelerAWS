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
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sloccy/ollamail-aws/db"
	"github.com/sloccy/ollamail-aws/gmail"
	"github.com/sloccy/ollamail-aws/llm"
)

//go:embed static
var staticFS embed.FS

const retentionUnitYears = "years"

const (
	triggerShowToast              = "showToast"
	triggerRefreshSuggestionBadge = "refreshSuggestionBadge"
	toastKeyMessage               = "message"
	jsonKeyType                   = "type"
	encodingGzip                  = "gzip"
	headerAcceptEncoding          = "Accept-Encoding"
)

// server holds all dependencies and the route mux.
type server struct {
	ctx       context.Context
	store     *db.Store
	llm       *llm.Client
	cfg       *Config
	auth      *gmail.Auth
	secretKey []byte
	tmpl      *template.Template
	mux       *http.ServeMux

	// OAuth state: short-lived in-memory map (single instance, no need for persistent storage)
	oauthMu    sync.Mutex
	oauthState map[string]time.Time

	// Cached Bedrock model list (refreshed at most once per hour)
	modelsMu        sync.Mutex
	modelsCache     []llm.ModelOption
	modelsFetchedAt time.Time
}

func newServer(ctx context.Context, store *db.Store, llmClient *llm.Client, auth *gmail.Auth, cfg *Config, secretKey []byte) http.Handler {
	s := &server{
		ctx:        ctx,
		store:      store,
		llm:        llmClient,
		cfg:        cfg,
		auth:       auth,
		secretKey:  secretKey,
		oauthState: make(map[string]time.Time),
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
	if strings.HasPrefix(r.URL.Path, "/static/") {
		s.mux.ServeHTTP(w, r)
		return
	}
	if strings.Contains(r.Header.Get(headerAcceptEncoding), encodingGzip) {
		w.Header().Set("Content-Encoding", encodingGzip)
		w.Header().Set("Vary", headerAcceptEncoding)
		gz := gzipPool.Get().(*gzip.Writer) //nolint:forcetypeassert // pool only contains *gzip.Writer
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

func (s *server) registerRoutes() {
	// Static (deliberately uncached — no cache-busting/versioned filenames exist yet,
	// so an aggressive Cache-Control here means fixes deployed to app.js/style.css
	// can take up to a day to reach returning users, behind CloudFront/Cloudflare too).
	staticSub, _ := fs.Sub(staticFS, "static")
	fileServer := http.StripPrefix("/static/", http.FileServer(http.FS(staticSub)))
	s.mux.HandleFunc("GET /static/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		relPath := strings.TrimPrefix(r.URL.Path, "/static/")
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
		s.render(w, "index.html", map[string]any{
			"ScanCadence": scanCadenceLabel,
		})
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
	s.mux.HandleFunc("GET /fragments/settings", s.handleGetSettings)
	s.mux.HandleFunc("PATCH /fragments/settings", s.handleUpdateSettings)
	s.mux.HandleFunc("GET /fragments/logs", s.handleLogs)
	s.mux.HandleFunc("GET /fragments/history", s.handleHistory)
	s.mux.HandleFunc("GET /fragments/troubleshooting", s.handleTroubleshooting)
	s.mux.HandleFunc("GET /fragments/history/filters", s.handleHistoryFilters)
	s.mux.HandleFunc("GET /fragments/history/{id}/llm-response", s.handleHistoryLlmResponse)
	s.mux.HandleFunc("GET /fragments/history/{id}/recategorize", s.handleRecategorizeForm)
	s.mux.HandleFunc("POST /fragments/history/{id}/recategorize", s.handleRecategorize)
	s.mux.HandleFunc("GET /fragments/prompt-suggestions", s.handlePromptSuggestionsList)
	s.mux.HandleFunc("GET /fragments/prompt-suggestions/{id}", s.handlePromptSuggestionDetail)
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

func (s *server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("render template", "name", name, "err", err)
	}
}

// renderFragmentFile renders a pre-parsed fragment template by its base filename.
func (s *server) renderFragmentFile(w http.ResponseWriter, path string, data any) {
	name := path[strings.LastIndex(path, "/")+1:]
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("execute fragment", "name", name, "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

func (s *server) fragmentResponse(w http.ResponseWriter, path string, data any, toast string) {
	if toast != "" {
		setHxTrigger(w, map[string]any{triggerShowToast: toast})
	}
	s.renderFragmentFile(w, path, data)
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
	accounts, _ := s.store.ListAccountsSafe(ctx)
	activePrompts, _ := s.store.CountActivePrompts(ctx)
	logs, _ := s.store.GetLogs(ctx, 100)

	data := map[string]any{
		"AccountCount":  len(accounts),
		"ActivePrompts": activePrompts,
		"Logs":          logs,
		"ScanCadence":   scanCadenceLabel,
	}
	s.fragmentResponse(w, "templates/fragments/dashboard.html", data, "")
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
	s.fragmentResponse(w, "templates/fragments/accounts_list.html", toAccountViews(rows), "")
}

func (s *server) handleToggleAccount(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	if id == 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	_, _ = s.store.ToggleAccount(ctx, id)
	s.handleAccounts(w, r)
}

func (s *server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	if id == 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
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

func (s *server) handlePromptsList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accountFilter := r.URL.Query().Get("account_id")
	views, _ := s.getPromptViews(ctx, accountFilter)
	s.fragmentResponse(w, "templates/fragments/prompts_list.html", views, "")
}

func (s *server) handleCreatePrompt(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_ = r.ParseForm()

	name := strings.TrimSpace(r.FormValue("name"))
	labelName := strings.TrimSpace(r.FormValue("label_name"))
	instructions := strings.TrimSpace(r.FormValue("instructions"))
	if name == "" {
		s.fragmentResponse(w, "templates/fragments/prompts_list.html", nil, "Name is required")
		return
	}

	var accountID sql.NullInt64
	if v := r.FormValue("account_id"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			accountID = sql.NullInt64{Int64: id, Valid: true}
		}
	}

	maxOrderRaw, _ := s.store.MaxPromptSortOrder(ctx)
	var maxOrder int64
	if v, ok := maxOrderRaw.(int64); ok {
		maxOrder = v
	} else {
		slog.Warn("MaxPromptSortOrder returned unexpected type", "type", fmt.Sprintf("%T", maxOrderRaw))
	}
	_, err := s.store.CreatePrompt(ctx, db.CreatePromptParams{
		Name:           name,
		Instructions:   instructions,
		LabelName:      labelName,
		ActionArchive:  boolToInt(r.FormValue("action_archive") == "1"),
		ActionSpam:     boolToInt(r.FormValue("action_spam") == "1"),
		ActionTrash:    boolToInt(r.FormValue("action_trash") == "1"),
		ActionMarkRead: boolToInt(r.FormValue("action_mark_read") == "1"),
		SortOrder:      maxOrder + 1,
		StopProcessing: boolToInt(r.FormValue("stop_processing") == "1"),
		AccountID:      accountID,
	})
	if err != nil {
		slog.Error("create prompt", "err", err)
		s.fragmentResponse(w, "templates/fragments/prompts_list.html", nil, "Failed to create rule")
		return
	}

	// Pre-create label in background for all matching accounts
	go s.ensureLabelForAccounts(context.Background(), labelName, accountID) //nolint:gosec // G118: must outlive request

	views, _ := s.getPromptViews(ctx, "")
	s.fragmentResponse(w, "templates/fragments/prompts_list.html", views, "Rule saved")
}

func (s *server) handleUpdatePrompt(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	if id == 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	_ = r.ParseForm()

	var accountID sql.NullInt64
	if v := r.FormValue("account_id"); v != "" {
		if aid, err := strconv.ParseInt(v, 10, 64); err == nil {
			accountID = sql.NullInt64{Int64: aid, Valid: true}
		}
	}
	labelName := strings.TrimSpace(r.FormValue("label_name"))

	_ = s.store.UpdatePrompt(ctx, db.UpdatePromptParams{
		Name:           strings.TrimSpace(r.FormValue("name")),
		Instructions:   strings.TrimSpace(r.FormValue("instructions")),
		LabelName:      labelName,
		ActionArchive:  boolToInt(r.FormValue("action_archive") == "1"),
		ActionSpam:     boolToInt(r.FormValue("action_spam") == "1"),
		ActionTrash:    boolToInt(r.FormValue("action_trash") == "1"),
		ActionMarkRead: boolToInt(r.FormValue("action_mark_read") == "1"),
		StopProcessing: boolToInt(r.FormValue("stop_processing") == "1"),
		AccountID:      accountID,
		ID:             id,
	})

	go s.ensureLabelForAccounts(context.Background(), labelName, accountID) //nolint:gosec // G118: must outlive request

	views, _ := s.getPromptViews(ctx, "")
	s.fragmentResponse(w, "templates/fragments/prompts_list.html", views, "Rule updated")
}

func (s *server) handleDeletePrompt(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	ctx := r.Context()
	_ = s.store.DeletePrompt(ctx, id)
	views, _ := s.getPromptViews(ctx, "")
	s.fragmentResponse(w, "templates/fragments/prompts_list.html", views, "Rule deleted")
}

func (s *server) handleTogglePrompt(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	ctx := r.Context()
	_, _ = s.store.TogglePrompt(ctx, id)

	p, err := s.store.GetPrompt(ctx, id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	accounts, _ := s.store.ListAccountsSafe(ctx)
	pv := dbPromptToView(p, buildAccountMap(accounts))
	s.render(w, "prompt_card_view", pv)
}

func (s *server) handleEditPrompt(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	ctx := r.Context()
	p, err := s.store.GetPrompt(ctx, id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	accounts, _ := s.store.ListAccountsSafe(ctx)
	data := promptEditView{
		Prompt:   dbPromptToView(p, buildAccountMap(accounts)),
		Accounts: toAccountViews(accounts),
	}
	s.render(w, "prompt_card_edit", data)
}

func (s *server) handleViewPrompt(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	ctx := r.Context()
	p, err := s.store.GetPrompt(ctx, id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	accounts, _ := s.store.ListAccountsSafe(ctx)
	s.render(w, "prompt_card_view", dbPromptToView(p, buildAccountMap(accounts)))
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

// classifyModelAllowed mirrors the geo/tier policy rendered into the two <select>s in
// settings_form.html: Standard is limited to single-datacenter, US-, or Global-routed
// models; Flex accepts any flex-capable model regardless of routing region.
func classifyModelAllowed(m llm.ModelOption, tier string) bool {
	if tier == llm.ClassifyTierFlex {
		return m.Flex
	}
	return m.ProfileRegion == "" || m.ProfileRegion == "us" || m.ProfileRegion == "global"
}

func settingsTemplateData(classifyModel, improveModel, classifyTier string, models []llm.ModelOption) map[string]any {
	return map[string]any{
		"ClassifyModel": classifyModel,
		"ImproveModel":  improveModel,
		"ClassifyTier":  classifyTier,
		"Models":        models,
	}
}

// settingOr returns the stored setting value, falling back to def when unset or empty.
func (s *server) settingOr(ctx context.Context, key, def string) string {
	if v, err := s.store.GetSetting(ctx, key); err == nil && v != "" {
		return v
	}
	return def
}

func (s *server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	classifyModel := s.settingOr(ctx, llm.SettingClassifyModel, s.cfg.BedrockModel)
	improveModel := s.settingOr(ctx, llm.SettingImproveModel, s.cfg.BedrockModel)
	classifyTier := s.settingOr(ctx, llm.SettingClassifyTier, llm.ClassifyTierStandard)
	models := s.cachedModels(ctx)
	s.fragmentResponse(w, "templates/fragments/settings_form.html",
		settingsTemplateData(classifyModel, improveModel, classifyTier, models), "")
}

func (s *server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_ = r.ParseForm()

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
	validID := func(id string) bool { return findModel(id) != nil }

	// Classification tier — "standard" or "flex". Must be resolved before validating the
	// classify_model choice: the two tiers have different eligible-model policies (see
	// classifyModelAllowed) enforced below.
	classifyTier := s.settingOr(ctx, llm.SettingClassifyTier, llm.ClassifyTierStandard)
	if v := r.FormValue("classify_tier"); v == llm.ClassifyTierStandard || v == llm.ClassifyTierFlex {
		classifyTier = v
		_ = s.store.SetSetting(ctx, db.SetSettingParams{Key: llm.SettingClassifyTier, Value: v})
	}

	classifyModel := s.settingOr(ctx, llm.SettingClassifyModel, s.cfg.BedrockModel)
	if v := r.FormValue("classify_model"); v != "" {
		if m := findModel(v); m != nil && classifyModelAllowed(*m, classifyTier) {
			classifyModel = v
			_ = s.store.SetSetting(ctx, db.SetSettingParams{Key: llm.SettingClassifyModel, Value: v})
		}
	}

	improveModel := s.settingOr(ctx, llm.SettingImproveModel, s.cfg.BedrockModel)
	if v := r.FormValue("improve_model"); v != "" && validID(v) {
		improveModel = v
		_ = s.store.SetSetting(ctx, db.SetSettingParams{Key: llm.SettingImproveModel, Value: v})
	}

	data := settingsTemplateData(classifyModel, improveModel, classifyTier, models)
	s.fragmentResponse(w, "templates/fragments/settings_form.html", data, "Settings saved")
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
	Timestamp      string
	AccountEmail   string
	Subject        string
	Sender         string
	PromptName     string
	LabelName      string
	ExtraActions   []string
	HasLlmResponse bool
}

func (s *server) handleHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()

	filter := db.HistoryFilter{
		Limit: int64(s.cfg.HistoryMaxLimit),
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

	rows, err := s.store.GetHistoryFiltered(ctx, filter)
	if err != nil {
		slog.Error("history query", "err", err)
		rows = nil
	}

	views := make([]historyRow, len(rows))
	for i, h := range rows {
		views[i] = historyRow{
			ID:             h.ID,
			Timestamp:      h.Timestamp,
			AccountEmail:   h.AccountEmail,
			Subject:        h.Subject,
			Sender:         h.Sender,
			PromptName:     strOrEmpty(h.PromptName),
			LabelName:      strOrEmpty(h.LabelName),
			HasLlmResponse: h.LlmResponse != "",
		}
		if h.Actions != "" {
			views[i].ExtraActions = strings.Split(h.Actions, ",")
		}
	}
	s.fragmentResponse(w, "templates/fragments/history_table.html", views, "")
}

func (s *server) handleTroubleshooting(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := s.store.GetLatestLlmDebug(ctx)
	if err != nil {
		slog.Error("troubleshooting query", "err", err)
		rows = nil
	}
	s.fragmentResponse(w, "templates/fragments/troubleshooting_list.html", rows, "")
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

	s.fragmentResponse(w, "templates/fragments/history_filters.html", map[string]any{
		"Accounts": toAccountViews(accounts),
		"Prompts":  options,
	}, "")
}

func (s *server) handleHistoryLlmResponse(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
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

func (s *server) handleGetRetention(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	ctx := r.Context()
	data := s.buildRetentionDataWithGmail(ctx, id)
	s.fragmentResponse(w, "templates/fragments/retention_panel.html", data, "")
}

func (s *server) handleSetGlobalRetention(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	ctx := r.Context()
	_ = r.ParseForm()

	if r.FormValue("enabled") == "1" {
		val, _ := strconv.ParseInt(r.FormValue("value"), 10, 64)
		unit := r.FormValue("unit")
		days := val
		if unit == retentionUnitYears {
			days = val * 365
		}
		if days > 0 {
			_ = s.store.SetGlobalRetention(ctx, db.SetGlobalRetentionParams{AccountID: id, GlobalDays: sql.NullInt64{Int64: days, Valid: true}})
		}
	} else {
		_ = s.store.ClearGlobalRetention(ctx, id)
	}
	data := s.buildRetentionDataWithGmail(ctx, id)
	s.fragmentResponse(w, "templates/fragments/retention_panel.html", data, "Saved")
}

func (s *server) handleAddLabelRetention(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	ctx := r.Context()
	_ = r.ParseForm()
	label := strings.TrimSpace(r.FormValue("label_name"))
	val, _ := strconv.ParseInt(r.FormValue("value"), 10, 64)
	unit := r.FormValue("unit")
	days := val
	if unit == retentionUnitYears {
		days = val * 365
	}
	if label != "" && days > 0 {
		_ = s.store.AddLabelRetention(ctx, db.AddLabelRetentionParams{AccountID: id, LabelName: label, Days: days})
	}
	data := s.buildRetentionDataWithGmail(ctx, id)
	s.fragmentResponse(w, "templates/fragments/retention_panel.html", data, "Rule added")
}

func (s *server) handleDeleteLabelRetention(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	ruleID := pathInt(r, "ruleId")
	ctx := r.Context()
	_ = s.store.DeleteLabelRetention(ctx, db.DeleteLabelRetentionParams{ID: ruleID, AccountID: id})
	data := s.buildRetentionDataWithGmail(ctx, id)
	s.fragmentResponse(w, "templates/fragments/retention_panel.html", data, "Rule removed")
}

func (s *server) handleAddExemption(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	ctx := r.Context()
	_ = r.ParseForm()
	label := strings.TrimSpace(r.FormValue("label_name"))
	if label != "" {
		_ = s.store.AddLabelExemption(ctx, db.AddLabelExemptionParams{AccountID: id, LabelName: label})
	}
	data := s.buildRetentionDataWithGmail(ctx, id)
	s.fragmentResponse(w, "templates/fragments/retention_panel.html", data, "Exemption added")
}

func (s *server) handleDeleteExemption(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	eid := pathInt(r, "eid")
	ctx := r.Context()
	_ = s.store.DeleteLabelExemption(ctx, db.DeleteLabelExemptionParams{ID: eid, AccountID: id})
	data := s.buildRetentionDataWithGmail(ctx, id)
	s.fragmentResponse(w, "templates/fragments/retention_panel.html", data, "Exemption removed")
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
	ctx := r.Context()
	data := s.buildRetentionDataWithGmail(ctx, id)
	s.fragmentResponse(w, "templates/fragments/retention_panel.html", data, "")
}

// gmailServiceFor builds an authenticated Gmail client for account, wiring credential
// refresh back to the store. Centralizes the OAuth-config + NewService + refresh-closure
// boilerplate otherwise repeated at every call site.
func (s *server) gmailServiceFor(ctx context.Context, account db.Account) (*gmail.Client, error) {
	oauthCfg, err := s.auth.Config()
	if err != nil {
		return nil, err
	}
	return gmail.NewService(ctx, account.CredentialsJSON, oauthCfg, func(newCreds string) {
		_ = s.store.UpdateAccountCredentials(ctx, db.UpdateAccountCredentialsParams{
			CredentialsJSON: newCreds, ID: account.ID,
		})
	})
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
		switch {
		case !exemptSet[lower] && !ruleSet[lower]:
			data.AvailableLabelsForExemption = append(data.AvailableLabelsForExemption, l.Name)
			data.AvailableLabelsForRules = append(data.AvailableLabelsForRules, l.Name)
		case !exemptSet[lower]:
			data.AvailableLabelsForExemption = append(data.AvailableLabelsForExemption, l.Name)
		case !ruleSet[lower]:
			data.AvailableLabelsForRules = append(data.AvailableLabelsForRules, l.Name)
		}
	}

	return data
}

// ============================================================
// OAuth
// ============================================================

func (s *server) handleOAuthStart(w http.ResponseWriter, _ *http.Request) {
	state := generateToken(16)
	s.oauthMu.Lock()
	now := time.Now()
	for k, exp := range s.oauthState {
		if now.After(exp) {
			delete(s.oauthState, k)
		}
	}
	s.oauthState[state] = now.Add(10 * time.Minute)
	s.oauthMu.Unlock()

	authURL, err := s.auth.GetAuthURL(state)
	if err != nil {
		http.Error(w, "Could not generate auth URL: "+err.Error(), http.StatusInternalServerError)
		return
	}
	data := map[string]string{"AuthURL": authURL}
	s.fragmentResponse(w, "templates/fragments/oauth_step2.html", data, "")
}

func (s *server) handleOAuthExchange(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_ = r.ParseForm()
	rawURL := r.FormValue("url")
	parsed, err := url.Parse(rawURL)
	if err != nil {
		s.fragmentResponse(w, "templates/fragments/accounts_list.html", nil, "Invalid URL")
		return
	}
	code := parsed.Query().Get("code")
	state := parsed.Query().Get("state")

	s.oauthMu.Lock()
	exp, ok := s.oauthState[state]
	if ok {
		delete(s.oauthState, state)
	}
	s.oauthMu.Unlock()

	if !ok || time.Now().After(exp) {
		s.fragmentResponse(w, "templates/fragments/accounts_list.html", nil, "OAuth state expired — try again")
		return
	}

	emailAddr, credJSON, err := s.auth.ExchangeCode(ctx, code)
	if err != nil {
		slog.Error("oauth exchange", "err", err)
		s.fragmentResponse(w, "templates/fragments/accounts_list.html", nil, "OAuth failed: "+err.Error())
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
		triggerShowToast:   map[string]any{toastKeyMessage: "Scan complete", jsonKeyType: "success"},
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

	s.fragmentResponse(w, "templates/fragments/account_options.html", map[string]any{
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
	_ = json.NewEncoder(w).Encode(prompts) //nolint:musttag,errchkjson // sqlc-generated struct; HTTP write error is unrecoverable
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
	_ = json.NewEncoder(w).Encode(configExport{ //nolint:musttag,errchkjson // sqlc-generated struct; HTTP write error is unrecoverable
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

	imported := 0
	for _, p := range cfg.Prompts {
		exists, _ := s.store.PromptExistsGlobal(ctx, p.Name)
		if exists != 0 {
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
		imported++
	}
	for _, setting := range cfg.Settings {
		if setting.Key == "secret_key" {
			continue
		}
		_ = s.store.SeedSetting(setting.Key, setting.Value)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"imported": imported}) //nolint:errchkjson // HTTP write error is unrecoverable
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
			break
		}
		b, _ := json.Marshal(map[string]string{jsonKeyType: "content", "text": chunk.Text}) //nolint:errchkjson // map[string]string cannot fail
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
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
		views[i] = suggestionView{
			ID:                    sg.ID,
			CreatedAt:             sg.CreatedAt,
			UpdatedAt:             sg.UpdatedAt,
			PromptID:              sg.PromptID,
			PromptName:            promptNames[sg.PromptID],
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
	s.fragmentResponse(w, "templates/fragments/prompt_suggestions_list.html", views, "")
}

func (s *server) handlePromptSuggestionDetail(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	if id == 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	sg, err := s.store.GetPromptSuggestion(ctx, id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	p, _ := s.store.GetPrompt(ctx, sg.PromptID)
	view := suggestionView{
		ID:                    sg.ID,
		CreatedAt:             sg.CreatedAt,
		UpdatedAt:             sg.UpdatedAt,
		PromptID:              sg.PromptID,
		PromptName:            p.Name,
		TriggerKind:           sg.TriggerKind,
		EmailSubject:          sg.EmailSubject,
		EmailSender:           sg.EmailSender,
		EmailBodySnapshot:     sg.EmailBodySnapshot,
		OriginalInstructions:  sg.OriginalInstructions,
		SuggestedInstructions: sg.SuggestedInstructions,
		UserComment:           sg.UserComment,
		Status:                sg.Status,
	}
	s.fragmentResponse(w, "templates/fragments/prompt_suggestion_detail.html", view, "")
}

func (s *server) handlePromptSuggestionRegenerate(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	if id == 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
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

	suggested, newConv, llmErr := s.llm.ImprovePromptInstructions(ctx, llm.ImproveRequest{
		PromptName:           p.Name,
		LabelName:            p.LabelName,
		OriginalInstructions: sg.OriginalInstructions,
		TriggerKind:          sg.TriggerKind,
		EmailSubject:         sg.EmailSubject,
		EmailSender:          sg.EmailSender,
		EmailBody:            sg.EmailBodySnapshot,
		PriorConversation:    conv,
		UserComment:          userComment,
	})
	if llmErr != nil {
		http.Error(w, "AI error: "+llmErr.Error(), http.StatusInternalServerError)
		return
	}

	convJSON, _ := json.Marshal(newConv) //nolint:errchkjson // []ChatMessage cannot fail
	_ = s.store.UpdatePromptSuggestion(ctx, db.UpdatePromptSuggestionParams{
		SuggestedInstructions: suggested,
		ConversationJSON:      string(convJSON),
		UserComment:           userComment,
		ID:                    id,
	})

	view := suggestionView{
		ID:                    sg.ID,
		CreatedAt:             sg.CreatedAt,
		UpdatedAt:             db.Now(),
		PromptID:              sg.PromptID,
		PromptName:            p.Name,
		TriggerKind:           sg.TriggerKind,
		EmailSubject:          sg.EmailSubject,
		EmailSender:           sg.EmailSender,
		EmailBodySnapshot:     sg.EmailBodySnapshot,
		OriginalInstructions:  sg.OriginalInstructions,
		SuggestedInstructions: suggested,
		UserComment:           userComment,
		Status:                db.SuggestionStatusPending,
	}
	s.fragmentResponse(w, "templates/fragments/prompt_suggestion_detail.html", view, "")
}

func (s *server) handlePromptSuggestionApply(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	if id == 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	sg, err := s.store.GetPromptSuggestion(ctx, id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := s.store.ApplyPromptSuggestionAndUpdatePrompt(ctx, id, sg.PromptID, sg.SuggestedInstructions); err != nil {
		http.Error(w, "apply failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	setHxTrigger(w, map[string]any{triggerRefreshSuggestionBadge: "1"})
	w.WriteHeader(http.StatusOK)
}

func (s *server) handlePromptSuggestionDismiss(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	if id == 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
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
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg}) //nolint:errchkjson // HTTP write error is unrecoverable
}
