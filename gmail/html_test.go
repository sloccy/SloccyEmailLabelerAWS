package gmail

import "testing"

func TestCleanInvisibles(t *testing.T) {
	const wantFoobar = "foobar"
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"clean text unchanged", "Hello, world!", "Hello, world!"},
		{"null byte stripped", "foo\x00bar", wantFoobar},
		{"C0 controls stripped", "a\x01\x02\x03b", "ab"},
		{"newline preserved", "line1\nline2", "line1\nline2"},
		{"tab preserved", "col1\tcol2", "col1\tcol2"},
		{"zero-width space stripped", "foo\u200bbar", wantFoobar},
		{"BOM stripped", "\uFEFFhello", "hello"},
		{"soft hyphen stripped", "hyp\u00adhen", "hyphen"},
		{"bidi override stripped", "a\u202eb", "ab"},
		{"valid UTF-8 preserved", "café naïve 日本語 🎉", "café naïve 日本語 🎉"},
		{"replacement char stripped", "foo\uFFFDbar", wantFoobar},
		{"empty string", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cleanInvisibles(tc.input)
			if got != tc.want {
				t.Errorf("cleanInvisibles(%q) = %q; want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestExtractText(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			"basic tag stripping",
			"<p>Hello <b>world</b></p>",
			"Hello\nworld",
		},
		{
			"script block removed",
			"<p>text</p><script>alert('x')</script><p>after</p>",
			"text\nafter",
		},
		{
			"style block removed",
			"<style>.x{color:red}</style><p>body</p>",
			"body",
		},
		{
			"html entities decoded",
			"<p>AT&amp;T &lt;rocks&gt;</p>",
			"AT&T <rocks>",
		},
		{
			"nbsp decoded",
			"<p>hello&nbsp;world</p>",
			"hello\u00a0world",
		},
		{
			"empty lines collapsed",
			"<p>a</p>\n\n<p>b</p>",
			"a\nb",
		},
		{
			"gt in double-quoted attribute not leaked",
			`<img alt="a > b" src="x.png">after`,
			"after",
		},
		{
			"gt in single-quoted attribute not leaked",
			`<div data-x='val>ue'>text</div>`,
			"text",
		},
		{
			"img with gt in alt between real content",
			`<p class="x">before</p><img alt=">" border="0"><p>after</p>`,
			"before\nafter",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractText(tc.input)
			if got != tc.want {
				t.Errorf("extractText(%q)\n  got  %q\n  want %q", tc.input, got, tc.want)
			}
		})
	}
}
