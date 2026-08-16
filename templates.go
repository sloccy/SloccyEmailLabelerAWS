package main

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"path/filepath"
	"time"
)

//go:embed templates
var templateFS embed.FS

// tmplFuncs returns the template FuncMap used by all templates.
func tmplFuncs() template.FuncMap {
	return template.FuncMap{
		"fmtdate":        fmtdate,
		"fmtdateStacked": fmtdateStacked,
		"fmtretention":   fmtretention,
		"dict":           dict,
		"safeHTML":       safeHTML,
		"asset":          assetURL,
	}
}

// safeHTML marks a string as pre-trusted HTML so html/template passes it through instead
// of escaping it — used to hand a literal icon <svg> into the "empty_state" partial via
// dict, which Go templates cannot do by name since {{template}} needs a literal.
//
// It is only sound while every call passes markup written literally in a template file;
// a value assembled at runtime would make its producer an XSS sink. That is enforced by
// TestSafeHTMLIsOnlyCalledWithLiterals rather than left to this comment.
func safeHTML(s string) template.HTML {
	return template.HTML(s) //nolint:gosec // G203: trusted, hardcoded template markup only
}

// dict creates a map from alternating key/value pairs, used in templates as (dict "Key" val ...).
func dict(pairs ...any) map[string]any {
	m := make(map[string]any, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		key, _ := pairs[i].(string)
		m[key] = pairs[i+1]
	}
	return m
}

const tsLayout = "2006-01-02 15:04:05"

func parseTS(ts string) (time.Time, bool) {
	if ts == "" {
		return time.Time{}, false
	}
	// Handle sql.NullString wrapper — ts may arrive as a struct; callers should pass .String
	t, err := time.Parse(tsLayout, ts)
	if err != nil {
		return time.Time{}, false
	}
	return t.Local(), true
}

func fmtdate(ts string) string {
	t, ok := parseTS(ts)
	if !ok {
		return "--"
	}
	return t.Format("2 Jan, 15:04")
}

// stackedDate is the two-line form of a timestamp, returned as data rather than as
// pre-built markup so the template emits the <br>/<span> and html/template escapes the
// values. Building the HTML here meant handing back a template.HTML that bypassed
// escaping — safe for a formatted time, but a trust boundary with no reason to exist.
type stackedDate struct {
	Date string
	Time string
	OK   bool
}

func fmtdateStacked(ts string) stackedDate {
	t, ok := parseTS(ts)
	if !ok {
		return stackedDate{}
	}
	return stackedDate{Date: t.Format("2 Jan"), Time: t.Format("15:04"), OK: true}
}

func fmtretention(days int64) string {
	if days >= 365 && days%365 == 0 {
		y := days / 365
		if y == 1 {
			return "1 year"
		}
		return fmt.Sprintf("%d years", y)
	}
	if days == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", days)
}

// loadTemplates parses all embedded templates.
func loadTemplates() (*template.Template, error) {
	t := template.New("").Funcs(tmplFuncs())

	err := fs.WalkDir(templateFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, readErr := templateFS.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", path, readErr)
		}
		if _, parseErr := t.New(filepath.Base(path)).Parse(string(data)); parseErr != nil {
			return fmt.Errorf("parse %s: %w", path, parseErr)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("load templates: %w", err)
	}

	return t, nil
}
