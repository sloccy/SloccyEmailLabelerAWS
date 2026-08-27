package main

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sloccy/ollamail-aws/db"
	"github.com/sloccy/ollamail-aws/llm"
)

func TestDict(t *testing.T) {
	t.Run("even pairs", func(t *testing.T) {
		m := dict("a", 1, "b", "hello")
		if m["a"] != 1 {
			t.Errorf("a = %v", m["a"])
		}
		if m["b"] != "hello" {
			t.Errorf("b = %v", m["b"])
		}
	})

	t.Run("odd pairs: last key dropped", func(t *testing.T) {
		m := dict("x", 10, "y")
		if m["x"] != 10 {
			t.Errorf("x = %v", m["x"])
		}
		if _, ok := m["y"]; ok {
			t.Error("dangling key y should not be in map")
		}
	})

	t.Run("empty input", func(t *testing.T) {
		m := dict()
		if len(m) != 0 {
			t.Errorf("expected empty map, got len=%d", len(m))
		}
	})

	t.Run("non-string key is silently skipped", func(t *testing.T) {
		m := dict(42, "val")
		if len(m) != 1 {
			t.Errorf("expected 1 entry with empty-string key, got len=%d", len(m))
		}
	})
}

func TestParseTS(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantOK  bool
		wantUTC string // empty means don't check value
	}{
		{name: "valid timestamp", input: "2024-03-15 09:30:00", wantOK: true, wantUTC: "2024-03-15 09:30:00"},
		{name: "empty string", input: "", wantOK: false},
		{name: "wrong format", input: "15/03/2024", wantOK: false},
		{name: "partial", input: "2024-03-15", wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseTS(tc.input)
			if ok != tc.wantOK {
				t.Fatalf("parseTS(%q) ok = %v, want %v", tc.input, ok, tc.wantOK)
			}
			if ok && tc.wantUTC != "" {
				// Round-trip: format back in UTC for comparison.
				asUTC := got.UTC().Format(tsLayout)
				if asUTC != tc.wantUTC {
					t.Errorf("parsed UTC = %q, want %q", asUTC, tc.wantUTC)
				}
			}
		})
	}
}

func TestFmtdate(t *testing.T) {
	// Use a fixed time in UTC so the test is timezone-independent via Local() round-trip.
	// parseTS returns t.Local(), so we construct the input in the local zone.
	loc := time.Local
	ts := time.Date(2024, 3, 15, 9, 30, 0, 0, time.UTC).In(loc)
	input := ts.Format(tsLayout)

	got := fmtdate(input)
	if got == "--" {
		t.Fatal("fmtdate returned -- for valid input")
	}
	// The format is "2 Jan, 15:04" — just check it's non-empty and contains "Mar".
	if !strings.Contains(got, "Mar") {
		t.Errorf("fmtdate(%q) = %q, want it to contain 'Mar'", input, got)
	}

	t.Run("invalid returns --", func(t *testing.T) {
		if got := fmtdate(""); got != "--" {
			t.Errorf("fmtdate('') = %q, want '--'", got)
		}
		if got := fmtdate("bad"); got != "--" {
			t.Errorf("fmtdate('bad') = %q, want '--'", got)
		}
	})
}

func TestFmtdateStacked(t *testing.T) {
	loc := time.Local
	ts := time.Date(2024, 6, 1, 14, 5, 0, 0, time.UTC).In(loc)
	input := ts.Format(tsLayout)

	// parseTS reads the layout as UTC and converts to local, so compare against what it
	// actually yields rather than against ts — the point here is the split, not the zone.
	want, ok := parseTS(input)
	if !ok {
		t.Fatalf("parseTS(%q) failed", input)
	}
	got := fmtdateStacked(input)
	if !got.OK {
		t.Fatalf("fmtdateStacked(%q).OK = false, want true", input)
	}
	if got.Date != want.Format("2 Jan") {
		t.Errorf("Date = %q, want %q", got.Date, want.Format("2 Jan"))
	}
	if got.Time != want.Format("15:04") {
		t.Errorf("Time = %q, want %q", got.Time, want.Format("15:04"))
	}

	t.Run("invalid input is not OK", func(t *testing.T) {
		if got := fmtdateStacked(""); got.OK {
			t.Errorf("fmtdateStacked(\"\").OK = true, want false")
		}
	})
}

func TestFmtretention(t *testing.T) {
	tests := []struct {
		days int64
		want string
	}{
		{1, "1 day"},
		{2, "2 days"},
		{30, "30 days"},
		{365, "1 year"},
		{730, "2 years"},
		{364, "364 days"},
		{366, "366 days"}, // not divisible by 365
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			got := fmtretention(tc.days)
			if got != tc.want {
				t.Errorf("fmtretention(%d) = %q, want %q", tc.days, got, tc.want)
			}
		})
	}
}

// ============================================================
// Prompt suggestion fragments — render smoke tests
// ============================================================
//
// html/template fails at *execution* time, not parse time, when a data shape doesn't
// match what a template expects ({{.Foo}} on a struct with no Foo field, {{range .Items}}
// against a bare slice, etc.) — exactly the kind of regression the suggestions-list
// restructuring (suggestionsListView wrapping []suggestionView) and the detail view's new
// live-trace branch could introduce silently. These render every status branch of both
// fragments against loadTemplates()' real parsed set, so a shape mismatch fails a test
// instead of a 500 in production.

func mustLoadTemplates(t *testing.T) *template.Template {
	t.Helper()
	tmpl, err := loadTemplates()
	if err != nil {
		t.Fatalf("loadTemplates: %v", err)
	}
	return tmpl
}

func TestPromptSuggestionsListTemplate_Renders(t *testing.T) {
	tmpl := mustLoadTemplates(t)

	cases := []struct {
		name string
		view suggestionsListView
	}{
		{"empty", suggestionsListView{PollEvery: "60s"}},
		{"generating", suggestionsListView{PollEvery: "5s", Items: []suggestionView{
			{ID: 1, PromptName: "Newsletters", TriggerKind: "false_negative", Status: "generating", EmailSubject: "Weekly digest", EmailSender: "a@example.com"},
		}}},
		{"pending", suggestionsListView{PollEvery: "60s", Items: []suggestionView{
			{ID: 2, PromptName: "Receipts", TriggerKind: "false_positive", Status: "pending", EmailSubject: "Order #1", EmailSender: "b@example.com"},
		}}},
		{"failed", suggestionsListView{PollEvery: "60s", Items: []suggestionView{
			{ID: 3, PromptName: "Spam", TriggerKind: "false_negative", Status: "failed", EmailSubject: "", EmailSender: "c@example.com"},
		}}},
		{"dismissed falls through to the else branch", suggestionsListView{PollEvery: "60s", Items: []suggestionView{
			{ID: 4, PromptName: "Spam", TriggerKind: "false_negative", Status: "dismissed"},
		}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := tmpl.ExecuteTemplate(io.Discard, "prompt_suggestions_list.html", c.view); err != nil {
				t.Errorf("execute: %v", err)
			}
		})
	}
}

func TestPromptSuggestionDetailTemplate_Renders(t *testing.T) {
	tmpl := mustLoadTemplates(t)

	base := suggestionView{
		ID: 1, PromptName: "Newsletters", TriggerKind: "false_negative",
		OriginalInstructions: "matches newsletters", EmailSubject: "Weekly digest", EmailSender: "a@example.com",
	}

	cases := []struct {
		name string
		view suggestionView
	}{
		{"generating: renders the live trace pane", func() suggestionView { v := base; v.Status = "generating"; return v }()},
		{"pending: renders the regenerate form", func() suggestionView {
			v := base
			v.Status = "pending"
			v.SuggestedInstructions = "Match promotional newsletters."
			return v
		}()},
		{"pending with replay results", func() suggestionView {
			v := base
			v.Status = "pending"
			v.SuggestedInstructions = "Match promotional newsletters."
			v.ReplayTotal, v.ReplayPassed, v.ReplayBaseline = 10, 8, 6
			v.ReplayFailures = []llm.ReplayFailure{{Verdict: "false_positive", Sender: "x@example.com", Subject: "s", Got: true}}
			return v
		}()},
		{"pending with example groups", func() suggestionView {
			v := base
			v.Status = "pending"
			v.SuggestedInstructions = "Match promotional newsletters."
			v.ExampleGroups = []exampleGroup{{Verdict: "false_negative", Label: "Missed it", Examples: []db.PromptExample{{Sender: "a@example.com", Subject: "s"}}}}
			return v
		}()},
		{"pending with a multi-round trajectory", func() suggestionView {
			v := base
			v.Status = "pending"
			v.SuggestedInstructions = "Match promotional newsletters."
			v.ReplayTotal, v.ReplayPassed = 10, 9
			v.BestRound = 2
			v.Rounds = []db.SuggestionRoundSummary{
				{N: 1, Candidate: "Match newsletters.", Passed: 7, Total: 10},
				{N: 2, Candidate: "Match promotional newsletters.", Passed: 9, Total: 10},
			}
			return v
		}()},
		{"failed: renders the error text and retry form", func() suggestionView {
			v := base
			v.Status = "failed"
			v.UserComment = "LLM error: timeout"
			return v
		}()},
		{"dismissed: renders the terminal-status branch", func() suggestionView { v := base; v.Status = "dismissed"; return v }()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := tmpl.ExecuteTemplate(io.Discard, "prompt_suggestion_detail.html", c.view); err != nil {
				t.Errorf("execute: %v", err)
			}
		})
	}
}

// TestPromptExamplesListTemplate_Renders exercises the read-only example expansion on a
// prompt card. Beyond "does it execute", it asserts the two things the template has to get
// right for the panel to be readable at all: the .examples-content class on the root (the
// card's <details> targets it by class, and the swap replaces the element that carries it,
// so losing it would break a re-open), and the "showing the newest N" note that only appears
// when the handler saw more rows than it displays.
func TestPromptExamplesListTemplate_Renders(t *testing.T) {
	tmpl := mustLoadTemplates(t)

	example := db.PromptExample{
		CreatedAt: "2026-08-01 09:12:03", Sender: "a@example.com", Subject: "Weekly digest",
		BodyExcerpt: "line one\nline two", Note: "not a newsletter", Missed: true,
	}

	cases := []struct {
		name         string
		view         promptExamplesView
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:         "empty corpus explains itself",
			view:         promptExamplesView{ID: 1},
			wantContains: []string{"examples-content", "No examples recorded yet"},
			wantAbsent:   []string{"Showing the newest"},
		},
		{
			name: "populated group renders the example and whether it was missed",
			view: promptExamplesView{ID: 1, Groups: []exampleGroup{
				{Verdict: db.VerdictConfirmedNegative, Label: verdictLabels[db.VerdictConfirmedNegative], Examples: []db.PromptExample{example}},
			}},
			wantContains: []string{"examples-content", "Weekly digest", "a@example.com", "not a newsletter", "missed"},
			wantAbsent:   []string{"Showing the newest"},
		},
		{
			name: "a full group says there are more",
			view: promptExamplesView{ID: 1, Groups: []exampleGroup{
				{Verdict: db.VerdictConfirmedPositive, Label: verdictLabels[db.VerdictConfirmedPositive], Examples: []db.PromptExample{example}, More: true},
			}},
			wantContains: []string{"Showing the newest"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := tmpl.ExecuteTemplate(&buf, "prompt_examples_list.html", c.view); err != nil {
				t.Fatalf("execute: %v", err)
			}
			got := buf.String()
			for _, want := range c.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("output missing %q:\n%s", want, got)
				}
			}
			for _, absent := range c.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("output unexpectedly contains %q:\n%s", absent, got)
				}
			}
		})
	}
}

// safeHTMLCallRe matches a safeHTML invocation and captures the first character of its
// argument, which is enough to tell a template-file literal from anything evaluated.
var safeHTMLCallRe = regexp.MustCompile(`\(safeHTML\s+(\S)`)

// safeHTML hands a string to html/template as pre-trusted markup, bypassing escaping. It
// is only sound while every call passes markup written literally in a template file: the
// moment one passes a value assembled at runtime, whatever produced that value becomes an
// XSS sink. That invariant lived in a comment; this test makes it fail the build instead.
func TestSafeHTMLIsOnlyCalledWithLiterals(t *testing.T) {
	var bad []string
	err := fs.WalkDir(templateFS, "templates", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		b, readErr := templateFS.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		for i, line := range strings.Split(string(b), "\n") {
			for _, m := range safeHTMLCallRe.FindAllStringSubmatch(line, -1) {
				if c := m[1]; c != "`" && c != `"` {
					bad = append(bad, fmt.Sprintf("%s:%d: safeHTML applied to a non-literal", p, i+1))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk templates: %v", err)
	}
	if len(bad) > 0 {
		t.Errorf("safeHTML must only wrap literal markup:\n  %s", strings.Join(bad, "\n  "))
	}
}
