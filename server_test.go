package main

import (
	"encoding/hex"
	"net/url"
	"strings"
	"testing"

	"github.com/sloccy/ollamail-aws/db"
	"github.com/sloccy/ollamail-aws/llm"
)

func TestBuildAccountMap(t *testing.T) {
	rows := []db.ListAccountsSafeRow{
		{ID: 1, Email: "a@example.com"},
		{ID: 2, Email: "b@example.com"},
	}
	m := buildAccountMap(rows)
	if m[1] != "a@example.com" {
		t.Errorf("id 1 = %q", m[1])
	}
	if m[2] != "b@example.com" {
		t.Errorf("id 2 = %q", m[2])
	}
	if len(m) != 2 {
		t.Errorf("len = %d, want 2", len(m))
	}
}

func TestBuildAccountMap_Empty(t *testing.T) {
	m := buildAccountMap(nil)
	if len(m) != 0 {
		t.Errorf("empty input should give empty map, got len=%d", len(m))
	}
}

func TestToAccountViews(t *testing.T) {
	lastScan := "2024-01-02 00:00:00"
	rows := []db.ListAccountsSafeRow{
		{ID: 1, Email: "x@example.com", Active: 1, AddedAt: "2024-01-01 00:00:00", LastScanAt: &lastScan},
		{ID: 2, Email: "y@example.com", Active: 0, AddedAt: "2024-02-01 00:00:00", LastScanAt: nil},
	}
	views := toAccountViews(rows)
	if len(views) != 2 {
		t.Fatalf("len = %d, want 2", len(views))
	}

	v0 := views[0]
	if v0.ID != 1 || v0.Email != "x@example.com" {
		t.Errorf("views[0] ID/Email wrong: %+v", v0)
	}
	if !v0.Active {
		t.Error("views[0].Active should be true (Active=1)")
	}
	if v0.LastScanAt != "2024-01-02 00:00:00" {
		t.Errorf("views[0].LastScanAt = %q", v0.LastScanAt)
	}

	v1 := views[1]
	if v1.Active {
		t.Error("views[1].Active should be false (Active=0)")
	}
	if v1.LastScanAt != "" {
		t.Errorf("views[1].LastScanAt should be empty for nil *string, got %q", v1.LastScanAt)
	}
}

func TestDbPromptToView(t *testing.T) {
	accountMap := map[int64]string{5: "owner@example.com"}

	accountID := int64(5)
	p := db.Prompt{
		ID:             10,
		Name:           "Test Prompt",
		Instructions:   "some instructions",
		LabelName:      "newsletters",
		Active:         1,
		CreatedAt:      "2024-01-01 00:00:00",
		ActionArchive:  1,
		ActionSpam:     0,
		ActionTrash:    1,
		ActionMarkRead: 0,
		StopProcessing: 1,
		AccountID:      &accountID,
	}

	pv := dbPromptToView(p, accountMap)

	if pv.ID != 10 {
		t.Errorf("ID = %d", pv.ID)
	}
	if pv.Name != "Test Prompt" {
		t.Errorf("Name = %q", pv.Name)
	}
	if !pv.Active {
		t.Error("Active should be true")
	}
	if !pv.ActionArchive {
		t.Error("ActionArchive should be true")
	}
	if pv.ActionSpam {
		t.Error("ActionSpam should be false")
	}
	if !pv.ActionTrash {
		t.Error("ActionTrash should be true")
	}
	if pv.ActionMarkRead {
		t.Error("ActionMarkRead should be false")
	}
	if !pv.StopProcessing {
		t.Error("StopProcessing should be true")
	}
	if pv.AccountID != 5 {
		t.Errorf("AccountID = %d", pv.AccountID)
	}
	if pv.AccountEmail != "owner@example.com" {
		t.Errorf("AccountEmail = %q", pv.AccountEmail)
	}
}

func TestDbPromptToView_NoAccount(t *testing.T) {
	p := db.Prompt{
		ID:        1,
		AccountID: nil,
	}
	pv := dbPromptToView(p, map[int64]string{})
	if pv.AccountID != 0 {
		t.Errorf("AccountID should be 0 for nil AccountID, got %d", pv.AccountID)
	}
	if pv.AccountEmail != "" {
		t.Errorf("AccountEmail should be empty, got %q", pv.AccountEmail)
	}
}

func TestBoolToInt(t *testing.T) {
	if boolToInt(true) != 1 {
		t.Error("boolToInt(true) should be 1")
	}
	if boolToInt(false) != 0 {
		t.Error("boolToInt(false) should be 0")
	}
}

func TestGenerateToken(t *testing.T) {
	tests := []struct{ n int }{
		{8}, {16}, {32},
	}
	for _, tc := range tests {
		t.Run("", func(t *testing.T) {
			tok := generateToken(tc.n)
			// hex.EncodeToString produces 2 chars per byte.
			wantLen := tc.n * 2
			if len(tok) != wantLen {
				t.Errorf("generateToken(%d) len = %d, want %d", tc.n, len(tok), wantLen)
			}
			// Must be valid hex.
			if _, err := hex.DecodeString(tok); err != nil {
				t.Errorf("generateToken(%d) = %q is not valid hex: %v", tc.n, tok, err)
			}
		})
	}

	t.Run("tokens differ", func(t *testing.T) {
		a := generateToken(16)
		b := generateToken(16)
		if a == b {
			t.Error("two calls to generateToken should not produce identical tokens")
		}
	})

	t.Run("lowercase hex only", func(t *testing.T) {
		tok := generateToken(32)
		if strings.ToLower(tok) != tok {
			t.Errorf("expected lowercase hex, got %q", tok)
		}
	})
}

func TestModelAllowedForTier(t *testing.T) {
	bare := llm.ModelOption{ID: "google.gemma-3-4b-it", Flex: true}                                   // single-datacenter, flex-capable
	usProfile := llm.ModelOption{ID: "us.amazon.nova-pro-v1:0", ProfileRegion: "us"}                  // US-routed, not flex
	globalProfile := llm.ModelOption{ID: "global.anthropic.claude-opus-4-8", ProfileRegion: "global"} // Global-routed, not flex
	euProfile := llm.ModelOption{ID: "eu.some.model", ProfileRegion: "eu", Flex: true}                // EU-routed, flex-capable
	apacNonFlex := llm.ModelOption{ID: "apac.some.model", ProfileRegion: "apac"}                      // APAC-routed, not flex

	cases := []struct {
		name  string
		model llm.ModelOption
		tier  string
		want  bool
	}{
		{"standard: bare model allowed", bare, llm.TierStandard, true},
		{"standard: us profile allowed", usProfile, llm.TierStandard, true},
		{"standard: global profile allowed", globalProfile, llm.TierStandard, true},
		{"standard: eu profile allowed — no geo restriction on standard", euProfile, llm.TierStandard, true},
		{"standard: apac profile allowed — no geo restriction on standard", apacNonFlex, llm.TierStandard, true},
		{"flex: flex-capable bare model allowed regardless of (lack of) profile", bare, llm.TierFlex, true},
		{"flex: non-flex us profile rejected", usProfile, llm.TierFlex, false},
		{"flex: non-flex global profile rejected", globalProfile, llm.TierFlex, false},
		{"flex: flex-capable eu profile allowed — any geo eligible for flex", euProfile, llm.TierFlex, true},
		{"flex: non-flex apac profile rejected", apacNonFlex, llm.TierFlex, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := modelAllowedForTier(c.model, c.tier); got != c.want {
				t.Errorf("modelAllowedForTier(%+v, %q) = %v, want %v", c.model, c.tier, got, c.want)
			}
		})
	}
}

// TestSettingsFormRendersTierControls executes the real settings_form.html fragment with
// settingsTemplateData and checks both models' Standard/Flex controls come out wired the
// way app.js and handleUpdateSettings expect (hidden tier inputs, per-tier selects with
// the inactive one disabled).
func TestSettingsFormRendersTierControls(t *testing.T) {
	tmpl, err := loadTemplates()
	if err != nil {
		t.Fatalf("loadTemplates: %v", err)
	}
	models := []llm.ModelOption{
		{ID: "us.amazon.nova-micro-v1:0", Label: "Nova Micro", ProfileRegion: "us", InputCostPer1M: 0.035, OutputCostPer1M: 0.14, FlexCostPer1M: 0.017, FlexOutputCostPer1M: 0.07, Flex: true},
		{ID: "eu.some.model", Label: "EU Model", ProfileRegion: "eu", InputCostPer1M: 0.5, OutputCostPer1M: 1.5, FlexCostPer1M: 0.25, FlexOutputCostPer1M: 0.75, Flex: true},
		{ID: "anthropic.claude-unpriced", Label: "Unpriced Model", InputCostPer1M: llm.CostUnknown, OutputCostPer1M: llm.CostUnknown, FlexCostPer1M: llm.CostUnknown, FlexOutputCostPer1M: llm.CostUnknown},
	}

	render := func(classifyTier, improveTier string) string {
		var sb strings.Builder
		data := settingsTemplateData("us.amazon.nova-micro-v1:0", "us.amazon.nova-micro-v1:0", classifyTier, improveTier, "", true, models)
		if err := tmpl.ExecuteTemplate(&sb, "settings_form.html", data); err != nil {
			t.Fatalf("ExecuteTemplate: %v", err)
		}
		return sb.String()
	}

	t.Run("both standard", func(t *testing.T) {
		out := render(llm.TierStandard, llm.TierStandard)
		for _, want := range []string{
			`name="classify_tier"`, `name="improve_tier"`,
			`id="classify-tier-toggle"`, `id="improve-tier-toggle"`,
			`id="improve-model-standard"`, `id="improve-model-flex"`,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("rendered form missing %s", want)
			}
		}
		// Standard tier: the flex select is the hidden/disabled one.
		if !strings.Contains(out, `id="improve-model-flex" style="min-width:300px;" disabled`) {
			t.Error("improve flex select should be disabled when improve tier is standard")
		}
		// EU model has no geo restriction anymore: it appears in all four selects (classify
		// standard/flex, improve standard/flex).
		if strings.Count(out, "eu.some.model") != 4 {
			t.Errorf("eu model should appear in all four selects, found %d occurrences", strings.Count(out, "eu.some.model"))
		}
		// Unpriced model appears in the two standard selects (any Converse model is listed
		// there) but not in either flex select (not flex-capable), and renders "n/a" instead of
		// being hidden.
		if strings.Count(out, "anthropic.claude-unpriced") != 2 {
			t.Errorf("unpriced model should appear only in the two standard selects, found %d occurrences", strings.Count(out, "anthropic.claude-unpriced"))
		}
		if !strings.Contains(out, "Unpriced Model — n/a") {
			t.Error("unpriced model should render with an n/a price, not be omitted")
		}
	})

	t.Run("improve flex", func(t *testing.T) {
		out := render(llm.TierStandard, llm.TierFlex)
		if !strings.Contains(out, `id="improve-model-standard" style="min-width:300px;" disabled`) {
			t.Error("improve standard select should be disabled when improve tier is flex")
		}
		if strings.Contains(out, `id="improve-model-flex" style="min-width:300px;" disabled`) {
			t.Error("improve flex select should be enabled when improve tier is flex")
		}
	})
}

// ============================================================
// History pagination (handleHistory helpers)
// ============================================================
//
// handleHistory itself isn't exercised end-to-end here: server.store is a concrete
// *db.Store (not db.StoreIface), so it can't take a db.FakeStore without a much wider
// interface than this change needs — server.go calls ~40 distinct *db.Store methods, and
// GetHistoryFiltered's own pagination correctness is already covered directly against
// db.FakeStore in db/history_page_test.go. What's tested here is the handler-local logic
// that sits around that call: the page-size/ceiling math and the sentinel URL builder.

func TestHistoryPageLimit(t *testing.T) {
	tests := []struct {
		name               string
		pageSize, maxLimit int
		loaded             int64
		wantLimit          int64
		wantCeilingHit     bool
	}{
		{"first page, plenty of room", 50, 500, 0, 50, false},
		{"final partial page clamps to what's left", 50, 500, 480, 20, false},
		{"exactly one row of room left", 50, 500, 499, 1, false},
		{"ceiling already reached", 50, 500, 500, 0, true},
		{"ceiling already exceeded", 50, 500, 600, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			limit, ceilingHit := historyPageLimit(tc.pageSize, tc.maxLimit, tc.loaded)
			if limit != tc.wantLimit || ceilingHit != tc.wantCeilingHit {
				t.Errorf("historyPageLimit(%d, %d, %d) = (%d, %v), want (%d, %v)",
					tc.pageSize, tc.maxLimit, tc.loaded, limit, ceilingHit, tc.wantLimit, tc.wantCeilingHit)
			}
		})
	}
}

func TestHistoryNextURL(t *testing.T) {
	q := url.Values{
		"account_id": {"7"},
		"subject":    {"invoice"},
	}

	got := historyNextURL(q, "2026-08-01 09:12:03#00000000000000000017", 50)

	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("historyNextURL produced an unparseable URL %q: %v", got, err)
	}
	if parsed.Path != "/fragments/history" {
		t.Errorf("path = %q, want /fragments/history", parsed.Path)
	}
	vals := parsed.Query()
	if vals.Get("account_id") != "7" || vals.Get("subject") != "invoice" {
		t.Errorf("existing filters not preserved: %v", vals)
	}
	if vals.Get("cursor") != "2026-08-01 09:12:03#00000000000000000017" {
		t.Errorf("cursor = %q", vals.Get("cursor"))
	}
	if vals.Get("loaded") != "50" {
		t.Errorf("loaded = %q, want 50", vals.Get("loaded"))
	}

	// q itself must be untouched — handleHistory reads filters from it earlier in the
	// same request and must not see cursor/loaded bleed back into that read.
	if _, ok := q["cursor"]; ok {
		t.Error("historyNextURL must not mutate its q argument")
	}
}
