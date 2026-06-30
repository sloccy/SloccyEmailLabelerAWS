package gmail

import (
	"context"
	"encoding/base64"
	"testing"
)

func b64(s string) string {
	return base64.URLEncoding.EncodeToString([]byte(s))
}

func part(mime, body string) apiMessagePart {
	return apiMessagePart{
		MimeType: mime,
		Body:     &apiMessagePartBody{Data: b64(body)},
	}
}

func multipart(children ...apiMessagePart) apiMessagePart {
	return apiMessagePart{
		MimeType: "multipart/alternative",
		Parts:    children,
	}
}

func TestExtractBodyRecursive(t *testing.T) {
	ctx := context.Background()

	cssDump := `.ExternalClass p { line-height: 100% } @font-face { font-family: Poppins; src: url(x.woff2); } @media only screen and (max-width:500px) { .mobile { width:414px!important; } } body { margin:0; padding:0; }`
	adCopy := "<p>You could win 2026 Stanley Cup tickets!</p><p>Enter now at greatclips.com</p>"
	adCopyStripped := "You could win 2026 Stanley Cup tickets!\nEnter now at greatclips.com"

	tests := []struct {
		name string
		root apiMessagePart
		want string
	}{
		{
			name: "html only",
			root: part("text/html", adCopy),
			want: adCopyStripped,
		},
		{
			name: "plain only",
			root: part("text/plain", "Just some plain text"),
			want: "Just some plain text",
		},
		{
			name: "css-dump plain beats real html — html wins",
			root: multipart(
				part("text/plain", cssDump),
				part("text/html", adCopy),
			),
			want: adCopyStripped,
		},
		{
			name: "real plain + empty html stub — plain wins",
			root: multipart(
				part("text/plain", "Win tickets!"),
				part("text/html", "<a>x</a>"), // strips to < 40 chars
			),
			want: "Win tickets!",
		},
		{
			name: "both empty — empty returned",
			root: multipart(
				part("text/plain", ""),
				part("text/html", ""),
			),
			want: "",
		},
		{
			name: "plain only in multipart",
			root: multipart(
				part("text/plain", "Plain only here"),
			),
			want: "Plain only here",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractBodyRecursive(ctx, nil, "msgID", &tc.root, 10000)
			if got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}
