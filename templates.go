package main

import (
	"embed"
	"encoding/json"
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
		"fmtinterval":    fmtinterval,
		"fmtretention":   fmtretention,
		"toJSON":         toJSON,
		"safeHTML":       func(s string) template.HTML { return template.HTML(s) }, //nolint:gosec // G203: used with trusted internal strings only
		"printf":         fmt.Sprintf,
		"dict":           dict,
		"not":            func(b bool) bool { return !b },
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

func fmtinterval(secs int) string {
	switch {
	case secs >= 3600:
		return fmt.Sprintf("%dh", secs/3600)
	case secs >= 60:
		return fmt.Sprintf("%dm", secs/60)
	default:
		return fmt.Sprintf("%ds", secs)
	}
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

func toJSON(v any) template.JS {
	b, err := json.Marshal(v)
	if err != nil {
		return template.JS("null")
	}
	return template.JS(b) //nolint:gosec // G203: JSON-encoded from internal data
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
