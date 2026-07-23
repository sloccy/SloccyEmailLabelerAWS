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
	}
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

func fmtdateStacked(ts string) template.HTML {
	t, ok := parseTS(ts)
	if !ok {
		return template.HTML("--")
	}
	date := t.Format("2 Jan")
	timeStr := t.Format("15:04")
	return template.HTML(date + `<br><span class="text-muted">` + timeStr + `</span>`) //nolint:gosec // G203: formatted from parsed time, no user input
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
