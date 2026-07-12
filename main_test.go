package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	gmhtml "github.com/yuin/goldmark/renderer/html"
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

// ----------------------------------------------------------------------------
// Combined directory pages: a collapsible file list over the rendered README,
// with the bare file listing also reachable at ?listing=1.
// ----------------------------------------------------------------------------

// A directory with a markdown index renders the README under a collapsible
// <details> file list whose summary counts the visible files/folders and names
// the rendered file; the listing table is embedded inline (revealed on expand).
func TestCombinedDirShowsCollapsibleFileList(t *testing.T) {
	h := newTestHandler(t, map[string]string{
		"README.md":   "# Project\n\nrendered readme body.\n",
		"other.txt":   "hello\n",
		"sub/keep.md": "# Sub\n",
	})
	body := roundTrip(t, h, "/", "text/html").Body.String()

	for _, want := range []string{
		`<details class="md-serve-files">`,
		"2 files · 1 folder",               // README.md + other.txt; sub/
		"README.md",                        // filename label on the right
		`<table class="md-serve-listing">`, // listing embedded inline
		"other.txt",                        // ...and it lists the siblings
		"rendered readme body.",            // the README is rendered inline
	} {
		if !strings.Contains(body, want) {
			t.Errorf("combined page missing %q\n%s", want, truncate(body, 500))
		}
	}
	// At the root there's no directory name, so the summary omits the "in …"
	// clause rather than showing "in /". (Match the clause markup, not the bare
	// class name, which the stylesheet defines.)
	if strings.Contains(body, `in <span class="md-serve-files-dir">`) {
		t.Errorf("combined page at root should omit the dir clause\n%s", truncate(body, 500))
	}
}

// At a nested directory the summary names the current directory in an "in …"
// clause built from the URL path.
func TestCombinedDirSummaryNamesNestedDir(t *testing.T) {
	h := newTestHandler(t, map[string]string{
		"doc/api/README.md": "# API\n\napi body.\n",
		"doc/api/schema.md": "# Schema\n",
	})
	body := roundTrip(t, h, "/doc/api/", "text/html").Body.String()

	for _, want := range []string{
		`<details class="md-serve-files">`,
		"2 files",
		`in <span class="md-serve-files-dir">doc/api</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("nested combined page missing %q\n%s", want, truncate(body, 500))
		}
	}
}

// ?listing=1 forces the bare file listing, bypassing both the combined README
// page and (crucially) an index.html that would otherwise pass through.
func TestListingQueryForcesFileListing(t *testing.T) {
	h := newTestHandler(t, map[string]string{
		"README.md":  "# Project\n\nrendered readme body.\n",
		"index.html": "<h1>Static home</h1>\n",
		"other.txt":  "hello\n",
	})

	// Without ?listing, index.html wins (matches every static host).
	plain := roundTrip(t, h, "/", "text/html").Body.String()
	if !strings.Contains(plain, "Static home") {
		t.Errorf("expected index.html passthrough at /\n%s", truncate(plain, 500))
	}

	// With ?listing=1, we get the generated listing regardless of index.html.
	listing := roundTrip(t, h, "/?listing=1", "text/html").Body.String()
	for _, want := range []string{`<table class="md-serve-listing">`, "Index of", "other.txt", "README.md"} {
		if !strings.Contains(listing, want) {
			t.Errorf("?listing=1 missing %q\n%s", want, truncate(listing, 500))
		}
	}
	for _, notWant := range []string{"Static home", "rendered readme body.", `<details class="md-serve-files">`} {
		if strings.Contains(listing, notWant) {
			t.Errorf("?listing=1 should not contain %q\n%s", notWant, truncate(listing, 500))
		}
	}
}

// ----------------------------------------------------------------------------
// -theme-cookie: pin light/dark from a request cookie (server-side), so a host
// like swe-swe can make md-serve match its own theme. Unset = follow the
// browser's prefers-color-scheme (unchanged default behavior).
// ----------------------------------------------------------------------------

// roundTripCookie is roundTrip plus a single request cookie.
func roundTripCookie(t *testing.T, h *fileHandler, target, accept, cname, cval string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", target, nil)
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	if cname != "" {
		r.AddCookie(&http.Cookie{Name: cname, Value: cval})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestThemeCookiePinsTheme(t *testing.T) {
	files := map[string]string{"test.md": "# Hi\n\n```go\nvar x = 1\n```\n"}

	// A handler with -theme-cookie set: the cookie now drives the theme.
	pinned := newTestHandler(t, files)
	pinned.themeCookie = "swe-swe-theme"

	// A handler with -theme-cookie unset: cookie is ignored, always auto.
	off := newTestHandler(t, files)

	type check struct {
		has    []string
		notHas []string
	}
	cases := []struct {
		name string
		h    *fileHandler
		cval string // "" means send no cookie
		want check
	}{
		{
			name: "pinned/dark",
			h:    pinned, cval: "dark",
			want: check{
				has:    []string{"github-markdown-dark.css", "color-scheme: dark;"},
				notHas: []string{"github-markdown.css\"", "prefers-color-scheme"},
			},
		},
		{
			name: "pinned/light",
			h:    pinned, cval: "light",
			want: check{
				has:    []string{"github-markdown-light.css", "color-scheme: light;"},
				notHas: []string{"github-markdown.css\"", "prefers-color-scheme", "#0d1117"},
			},
		},
		{
			name: "pinned/unrecognized-value falls back to auto",
			h:    pinned, cval: "chartreuse",
			want: check{
				has:    []string{"github-markdown.css\"", "color-scheme: light dark;", "prefers-color-scheme"},
				notHas: []string{"github-markdown-dark.css"},
			},
		},
		{
			name: "pinned/no-cookie falls back to auto",
			h:    pinned, cval: "",
			want: check{
				has:    []string{"github-markdown.css\"", "color-scheme: light dark;", "prefers-color-scheme"},
				notHas: []string{"github-markdown-dark.css"},
			},
		},
		{
			name: "flag-off/dark-cookie is ignored (auto)",
			h:    off, cval: "dark",
			want: check{
				has:    []string{"github-markdown.css\"", "color-scheme: light dark;", "prefers-color-scheme"},
				notHas: []string{"github-markdown-dark.css"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := roundTripCookie(t, tc.h, "/test.md", "text/html", "swe-swe-theme", tc.cval).Body.String()
			for _, want := range tc.want.has {
				if !strings.Contains(body, want) {
					t.Errorf("missing %q\n%s", want, truncate(body, 400))
				}
			}
			for _, notWant := range tc.want.notHas {
				if strings.Contains(body, notWant) {
					t.Errorf("unexpectedly contains %q\n%s", notWant, truncate(body, 400))
				}
			}
		})
	}
}

// ----------------------------------------------------------------------------
// Hidden files (dotfiles) — served & listed by default, hidden via
// -hide-dotfiles (which also 404s direct requests, not just omits listings).
// ----------------------------------------------------------------------------

func TestHasHiddenSegment(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/", false},
		{"/index.md", false},
		{"/dir/file.txt", false},
		{"/.env", true},
		{"/.git", true},
		{"/.git/config", true},
		{"/dir/.secret", true},
		{"/a/.b/c.txt", true},
		{"/_md-serve-assets/x.css", false}, // underscore prefix is not hidden
	}
	for _, tc := range cases {
		if got := hasHiddenSegment(tc.path); got != tc.want {
			t.Errorf("hasHiddenSegment(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// By default dotfiles are revealed: served byte-for-byte on a direct request
// and included in directory listings.
func TestHiddenFilesRevealedByDefault(t *testing.T) {
	h := newTestHandler(t, map[string]string{
		".env":        "SECRET=1\n",
		".git/config": "[core]\n",
		"visible.txt": "hi\n",
	})

	w := roundTrip(t, h, "/.env", "*/*")
	if w.Code != 200 {
		t.Fatalf("GET /.env status = %d, want 200 (body=%q)", w.Code, truncate(w.Body.String(), 200))
	}
	if body := w.Body.String(); !strings.Contains(body, "SECRET=1") {
		t.Errorf("GET /.env body = %q, want the file content", truncate(body, 200))
	}
	if w := roundTrip(t, h, "/.git/config", "*/*"); w.Code != 200 {
		t.Errorf("GET /.git/config status = %d, want 200", w.Code)
	}

	listing := roundTrip(t, h, "/", "*/*").Body.String()
	for _, want := range []string{".env", ".git", "visible.txt"} {
		if !strings.Contains(listing, want) {
			t.Errorf("listing missing %q\n%s", want, truncate(listing, 500))
		}
	}
}

// With -hide-dotfiles, dotfiles are omitted from listings AND direct requests
// (including nested dotfile directories) 404 instead of leaking bytes.
func TestHiddenFilesHiddenWithFlag(t *testing.T) {
	h := newTestHandler(t, map[string]string{
		".env":        "SECRET=1\n",
		".git/config": "[core]\n",
		"visible.txt": "hi\n",
	})
	h.hideDotfiles = true

	for _, target := range []string{"/.env", "/.git/config", "/.git/"} {
		if w := roundTrip(t, h, target, "*/*"); w.Code != 404 {
			t.Errorf("GET %s status = %d, want 404 (body=%q)", target, w.Code, truncate(w.Body.String(), 200))
		}
	}

	if w := roundTrip(t, h, "/visible.txt", "*/*"); w.Code != 200 {
		t.Errorf("GET /visible.txt status = %d, want 200", w.Code)
	}

	listing := roundTrip(t, h, "/", "*/*").Body.String()
	if strings.Contains(listing, ".env") || strings.Contains(listing, ".git") {
		t.Errorf("listing should not mention dotfiles\n%s", truncate(listing, 500))
	}
	if !strings.Contains(listing, "visible.txt") {
		t.Errorf("listing missing visible.txt\n%s", truncate(listing, 500))
	}

	// The live-reload endpoint must not report on hidden paths either.
	req := httptest.NewRequest("GET", livereloadPath+"?path=/.env", nil)
	rec := httptest.NewRecorder()
	h.livereload(rec, req)
	if rec.Code != 404 {
		t.Errorf("livereload /.env status = %d, want 404 (body=%q)", rec.Code, truncate(rec.Body.String(), 200))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
