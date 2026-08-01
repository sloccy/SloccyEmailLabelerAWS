package gmail

import (
	"html"
	"strings"
)

// extractText strips HTML tags from src and returns plain text.
// It skips content inside <script> and <style> elements.
func extractText(src string) string {
	srcLower := strings.ToLower(src)
	var sb strings.Builder
	inTag := false
	inScript := false
	inStyle := false
	i := 0

	for i < len(src) {
		if !inTag && !inScript && !inStyle {
			if src[i] == '<' {
				inTag = true
				// Peek at tag name using pre-lowercased copy
				peek := srcLower[i:min(i+8, len(src))]
				if strings.HasPrefix(peek, "<script") {
					inScript = true
				} else if strings.HasPrefix(peek, "<style") {
					inStyle = true
				}
				i++
				continue
			}
			sb.WriteByte(src[i])
			i++
			continue
		}

		if inScript {
			if idx := strings.Index(srcLower[i:], "</script>"); idx >= 0 {
				i += idx + len("</script>")
				inScript = false
				inTag = false
			} else {
				i = len(src)
			}
			continue
		}

		if inStyle {
			if idx := strings.Index(srcLower[i:], "</style>"); idx >= 0 {
				i += idx + len("</style>")
				inStyle = false
				inTag = false
			} else {
				i = len(src)
			}
			continue
		}

		switch src[i] {
		case '"', '\'':
			// Skip to the matching close quote so a '>' inside an
			// attribute value (e.g. alt="a > b") is not mistaken for
			// the end of the tag.
			q := src[i]
			i++
			for i < len(src) && src[i] != q {
				i++
			}
		case '>':
			inTag = false
			sb.WriteByte('\n')
		}
		i++
	}

	// Decode HTML entities, then strip invisible/zero-width characters.
	cleaned := cleanInvisibles(html.UnescapeString(sb.String()))

	// Collapse whitespace lines (some may be empty after invisible-char removal).
	lines := strings.Split(cleaned, "\n")
	var out []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, "\n")
}

// cleanInvisibles removes zero-width, formatting, and BOM code points that
// marketing emails commonly inject as tracking padding or preheader spacers.
func cleanInvisibles(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 0x20: // C0 controls (except \t above)
			return -1
		case r == 0x00AD: // SOFT HYPHEN
			return -1
		case r == 0x034F: // COMBINING GRAPHEME JOINER
			return -1
		case r == 0x200B || r == 0x200C || r == 0x200D: // ZWSP, ZWNJ, ZWJ
			return -1
		case r == 0x2060: // WORD JOINER
			return -1
		case r == 0xFEFF: // BOM / ZERO WIDTH NO-BREAK SPACE
			return -1
		case r >= 0x202A && r <= 0x202E: // bidi embedding/override controls
			return -1
		case r >= 0x2066 && r <= 0x2069: // bidi isolate controls
			return -1
		case r == 0xFFFD: // Unicode replacement character — signals decode error
			return -1
		default:
			return r
		}
	}, s)
}

// Truncate returns s truncated to maxChars bytes.
func Truncate(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	return s[:maxChars]
}

// CollapseExcerpt joins s's whitespace-separated fields with single spaces and truncates to
// maxRunes — turns a raw (possibly multi-line, multi-space) email body into a compact,
// single-line excerpt safe to store and display. Not a substitute for Truncate: Truncate is
// a byte-length cutoff with no whitespace collapsing and no UTF-8-rune safety (fine for a
// log preview, risky for content meant to be stored and re-rendered — a byte cutoff can
// split a multi-byte rune mid-character). Used for db.PromptExample.BodyExcerpt by both the
// recategorize handlers and the classify path (processor), which is why this lives here
// rather than in either of those packages — both already import gmail, and this avoids a
// main<->processor import cycle that duplicating it in each would otherwise require.
func CollapseExcerpt(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > maxRunes {
		r = r[:maxRunes]
	}
	return string(r)
}
