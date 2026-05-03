package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	gmhtml "github.com/yuin/goldmark/renderer/html"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
)

// ----------------------------------------------------------------------------
// acceptsHTML — unit tests
// ----------------------------------------------------------------------------

func TestAcceptsHTML(t *testing.T) {
	cases := []struct {
		name     string
		setValue bool // false → don't set the header at all (distinct from empty string)
		accept   string
		want     bool
	}{
		// "Anything" — clients that don't constrain Accept get HTML.
		{"missing header", false, "", true},
		{"empty header", true, "", true},
		{"*/*", true, "*/*", true},
		{"text/*", true, "text/*", true},
		{"text/html", true, "text/html", true},
		{"application/xhtml+xml", true, "application/xhtml+xml", true},

		// Real-world browser Accept (Chromium, Firefox).
		{"browser typical", true,
			"text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8",
			true},

		// Accept lists with text/html somewhere in them.
		{"text/html with q-param", true, "text/html;q=0.9", true},
		{"json then text/html", true, "application/json, text/html", true},

		// Case-insensitive media type matching.
		{"upper case text/html", true, "TEXT/HTML", true},
		{"mixed case wildcard", true, "Text/*", true},

		// Explicit non-HTML — these are the requests we want to gate raw.
		{"json only", true, "application/json", false},
		{"text/plain only", true, "text/plain", false},
		{"json + octet", true, "application/json, application/octet-stream", false},
		{"image/png", true, "image/png", false},

		// Whitespace tolerance.
		{"padded list", true, "  application/json ,  text/html  ", true},
		{"padded non-html", true, "  application/json , application/xml ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.Header.Del("Accept")
			if tc.setValue {
				r.Header.Set("Accept", tc.accept)
			}
			if got := acceptsHTML(r); got != tc.want {
				t.Errorf("acceptsHTML(%q, set=%v) = %v, want %v",
					tc.accept, tc.setValue, got, tc.want)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// Pretty × Accept matrix — integration tests via httptest
// ----------------------------------------------------------------------------

func newTestHandler(t *testing.T, files map[string]string) *fileHandler {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	md := goldmark.New(
		goldmark.WithExtensions(
			extension.GFM,
			highlighting.NewHighlighting(
				highlighting.WithStyle("github"),
				highlighting.WithFormatOptions(chromahtml.WithClasses(true)),
			),
		),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		goldmark.WithRendererOptions(gmhtml.WithUnsafe()),
	)
	return &fileHandler{root: root, md: md, live: false}
}

// pretty=true marker: the wrapped HTML page contains <article class="markdown-body">.
const renderedMarker = `<article class="markdown-body">`

// roundTrip drives a single request through the handler and returns the
// recorder. Accept is omitted entirely when accept == "" (use "<empty>" to
// send an explicit empty Accept header — not exercised here since the
// distinction is covered in TestAcceptsHTML).
func roundTrip(t *testing.T, h *fileHandler, target, accept string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", target, nil)
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestServeFileMatrix(t *testing.T) {
	h := newTestHandler(t, map[string]string{
		"test.md":   "# Hello\n\nThis is *markdown*.\n",
		"test.html": "<h1>Static page</h1>\n",
		"test.go":   "package main\n\nfunc main() { println(\"hi\") }\n",
	})

	type expect struct {
		status     int
		ctPrefix   string // Content-Type prefix the response must match
		bodyHas    string // substring that must appear in the response body
		bodyNotHas string // substring that must NOT appear (optional)
	}

	cases := []struct {
		name   string
		target string
		accept string
		want   expect
	}{
		// ---- .md (prettyByDefault=true) -------------------------------------
		{
			name:   "md/default/accept-star — pretty HTML",
			target: "/test.md",
			accept: "*/*",
			want:   expect{200, "text/html", renderedMarker, ""},
		},
		{
			name:   "md/default/accept-html — pretty HTML",
			target: "/test.md",
			accept: "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
			want:   expect{200, "text/html", renderedMarker, ""},
		},
		{
			name:   "md/default/accept-json — Accept gate forces raw text/plain",
			target: "/test.md",
			accept: "application/json",
			want:   expect{200, "text/plain", "# Hello", renderedMarker},
		},
		{
			name:   "md/?pretty=0/accept-star — explicit raw wins",
			target: "/test.md?pretty=0",
			accept: "*/*",
			want:   expect{200, "text/plain", "# Hello", renderedMarker},
		},
		{
			name:   "md/?pretty=1/accept-html — explicit pretty + html allowed",
			target: "/test.md?pretty=1",
			accept: "text/html",
			want:   expect{200, "text/html", renderedMarker, ""},
		},
		{
			name:   "md/?pretty=1/accept-json — Accept overrides explicit pretty=1",
			target: "/test.md?pretty=1",
			accept: "application/json",
			want:   expect{200, "text/plain", "# Hello", renderedMarker},
		},

		// ---- .html (prettyByDefault=true, browser-rendered) ------------------
		{
			name:   "html/default/accept-star — text/html passthrough",
			target: "/test.html",
			accept: "*/*",
			want:   expect{200, "text/html", "<h1>Static page</h1>", ""},
		},
		{
			name:   "html/default/accept-json — Accept gate forces raw text/plain",
			target: "/test.html",
			accept: "application/json",
			want:   expect{200, "text/plain", "<h1>Static page</h1>", ""},
		},
		{
			name:   "html/?pretty=0/accept-star — explicit raw text/plain",
			target: "/test.html?pretty=0",
			accept: "*/*",
			want:   expect{200, "text/plain", "<h1>Static page</h1>", ""},
		},
		{
			name:   "html/?pretty=1/accept-json — Accept overrides explicit pretty=1",
			target: "/test.html?pretty=1",
			accept: "application/json",
			want:   expect{200, "text/plain", "<h1>Static page</h1>", ""},
		},

		// ---- .go (prettyByDefault=false, chroma highlighter) -----------------
		{
			name:   "go/default/accept-star — raw bytes (chroma not invoked)",
			target: "/test.go",
			accept: "*/*",
			want:   expect{200, "text/", "package main", renderedMarker},
		},
		{
			name:   "go/default/accept-json — raw (default already raw, Accept moot)",
			target: "/test.go",
			accept: "application/json",
			want:   expect{200, "text/", "package main", renderedMarker},
		},
		{
			name:   "go/?pretty=1/accept-star — chroma-highlighted HTML",
			target: "/test.go?pretty=1",
			accept: "*/*",
			want:   expect{200, "text/html", renderedMarker, ""},
		},
		{
			name:   "go/?pretty=1/accept-html — chroma-highlighted HTML",
			target: "/test.go?pretty=1",
			accept: "text/html",
			want:   expect{200, "text/html", renderedMarker, ""},
		},
		{
			name:   "go/?pretty=1/accept-json — Accept overrides, raw bytes",
			target: "/test.go?pretty=1",
			accept: "application/json",
			want:   expect{200, "text/", "package main", renderedMarker},
		},
		{
			name:   "go/?pretty=0/accept-html — explicit raw beats default-on-no-default",
			target: "/test.go?pretty=0",
			accept: "text/html",
			want:   expect{200, "text/", "package main", renderedMarker},
		},

		// ---- Invalid ?pretty values fall back to extension default -----------
		{
			name:   "md/?pretty=garbage/accept-star — unparseable falls back to default (pretty)",
			target: "/test.md?pretty=garbage",
			accept: "*/*",
			want:   expect{200, "text/html", renderedMarker, ""},
		},
		{
			name:   "go/?pretty=garbage/accept-star — unparseable falls back to default (raw)",
			target: "/test.go?pretty=garbage",
			accept: "*/*",
			want:   expect{200, "text/", "package main", renderedMarker},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := roundTrip(t, h, tc.target, tc.accept)
			if w.Code != tc.want.status {
				t.Fatalf("status = %d, want %d (body=%q)", w.Code, tc.want.status, w.Body.String())
			}
			gotCT := w.Header().Get("Content-Type")
			if !strings.HasPrefix(gotCT, tc.want.ctPrefix) {
				t.Errorf("Content-Type = %q, want prefix %q", gotCT, tc.want.ctPrefix)
			}
			body := w.Body.String()
			if tc.want.bodyHas != "" && !strings.Contains(body, tc.want.bodyHas) {
				t.Errorf("body missing %q\nbody (first 300 chars): %s",
					tc.want.bodyHas, truncate(body, 300))
			}
			if tc.want.bodyNotHas != "" && strings.Contains(body, tc.want.bodyNotHas) {
				t.Errorf("body unexpectedly contained %q\nbody (first 300 chars): %s",
					tc.want.bodyNotHas, truncate(body, 300))
			}
		})
	}
}

// nosniff is set on raw responses for prettyByDefault=true extensions to
// keep browsers from re-sniffing markdown/html source as HTML when served
// as text/plain. Worth a regression guard.
func TestRawForPrettyByDefaultSetsNosniff(t *testing.T) {
	h := newTestHandler(t, map[string]string{
		"a.md":   "# x\n",
		"a.html": "<p>x</p>",
	})
	for _, path := range []string{"/a.md", "/a.html"} {
		t.Run(path, func(t *testing.T) {
			w := roundTrip(t, h, path+"?pretty=0", "*/*")
			if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
			}
			// Same result via the Accept gate — it's the same code path
			// but the test pins both entry points.
			w2 := roundTrip(t, h, path, "application/json")
			if got := w2.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("via Accept gate: X-Content-Type-Options = %q, want %q", got, "nosniff")
			}
		})
	}
}

// Sanity: chroma raw passthrough (no pretty) should NOT add nosniff — the
// file's real MIME type is the right answer for source files.
func TestChromaRawDoesNotForceNosniff(t *testing.T) {
	h := newTestHandler(t, map[string]string{"x.go": "package main\n"})
	w := roundTrip(t, h, "/x.go", "*/*")
	if got := w.Header().Get("X-Content-Type-Options"); got == "nosniff" {
		t.Errorf("unexpectedly forced nosniff on raw .go (header was %q)", got)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
