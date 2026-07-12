// md-serve is a tiny static file server that additionally renders Markdown
// files (.md, .markdown) as GitHub-styled HTML using goldmark + the
// github-markdown-css stylesheet (bundled via //go:embed).
package main

import (
	"bytes"
	"embed"
	"errors"
	"flag"
	"fmt"
	"html"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	gmhtml "github.com/yuin/goldmark/renderer/html"
)

// Populated via -ldflags at build time (see scripts/build-platforms.sh).
var (
	version = "dev"
	commit  = "unknown"
)

//go:embed assets/github-markdown.css assets/github-markdown-light.css assets/github-markdown-dark.css
var assetsFS embed.FS

// Path prefix used to serve the embedded CSS files. Chosen to be unlikely
// to collide with anything a user might have on disk.
const assetsPrefix = "/_md-serve-assets/"

// Endpoint used by injected live-reload JS to poll for source-file changes.
const livereloadPath = "/_md-serve-livereload"

// maxHighlightBytes caps how big a source file we'll syntax-highlight.
// Above this, we fall back to byte-for-byte serving via http.ServeFile so
// the browser handles the download instead of us building a giant DOM.
const maxHighlightBytes = 1 << 20 // 1 MiB

// chromaFormatter renders chroma tokens to HTML using class names (not
// inline styles) so the syntax theme can swap with prefers-color-scheme.
// Line numbers go in a separate <td> column with linkable anchors so
// URLs like /main.go#L42 jump to the right line.
var chromaFormatter = chromahtml.New(
	chromahtml.WithClasses(true),
	chromahtml.WithLineNumbers(true),
	chromahtml.LineNumbersInTable(true),
	chromahtml.WithLinkableLineNumbers(true, "L"),
)

// chromaLightStyle / chromaDarkStyle are the syntax themes for light and
// dark mode respectively. Both use chroma's built-in github themes so the
// colours line up with the rest of the GitHub-styled chrome.
var (
	chromaLightStyle = pickChromaStyle("github")
	chromaDarkStyle  = pickChromaStyle("github-dark")
)

func pickChromaStyle(name string) *chroma.Style {
	if s := styles.Get(name); s != nil {
		return s
	}
	return styles.Fallback
}

// chromaCSSAuto / chromaCSSLight / chromaCSSDark are the three syntax
// stylesheets inlined into a page, one chosen per resolved theme (see
// themeFor). Each rule is scoped under .markdown-body so chroma's background
// beats github-markdown.css's `.markdown-body pre` rule, which would otherwise
// mask chroma's per-mode colours.
//
//   - auto:  the "github" (light) stylesheet, then "github-dark" wrapped in
//     @media (prefers-color-scheme: dark), so code blocks track the OS mode.
//   - light: only the light stylesheet — the theme is pinned, so no media query.
//   - dark:  only the dark stylesheet, applied unconditionally.
var (
	chromaCSSLight = template.CSS(scopedChromaStyle(chromaLightStyle))
	chromaCSSDark  = template.CSS(scopedChromaStyle(chromaDarkStyle))
	chromaCSSAuto  = template.CSS(string(chromaCSSLight) +
		"\n@media (prefers-color-scheme: dark) {\n" + string(chromaCSSDark) + "}\n")
)

// scopedChromaStyle renders one chroma style to CSS and scopes it under
// .markdown-body. Returns "" if chroma fails to emit (never observed for the
// built-in github styles, but we don't want a broken page if it does).
func scopedChromaStyle(style *chroma.Style) string {
	var buf bytes.Buffer
	if err := chromaFormatter.WriteCSS(&buf, style); err != nil {
		return ""
	}
	return scopeChromaCSS(buf.String())
}

// scopeChromaCSS prefixes every chroma class selector with .markdown-body
// so the rules outweigh github-markdown.css's `.markdown-body pre` styling.
// Without this the GitHub stylesheet's pre background would override
// chroma's, which is what we need to differ between light and dark mode.
func scopeChromaCSS(css string) string {
	return strings.ReplaceAll(css, ".chroma", ".markdown-body .chroma")
}

// Resolved theme modes. themeAuto follows the browser's prefers-color-scheme
// (the default, and md-serve's only behavior when -theme-cookie is unset);
// themeLight / themeDark pin the theme, chosen from a request cookie.
const (
	themeAuto  = "auto"
	themeLight = "light"
	themeDark  = "dark"
)

// chromeDarkRules are the dark-mode overrides for md-serve's own chrome — the
// page background, breadcrumb / source labels, and width widget — i.e. the
// bits github-markdown.css doesn't theme for us. They're emitted either inside
// an @media (prefers-color-scheme: dark) block (auto theme) or unconditionally
// (dark theme pinned by cookie); see themeFor.
const chromeDarkRules = `
    /* Match the body to the .markdown-body dark background so the 45px
       padding around the article doesn't show a tonal seam against the
       browser's UA-default dark canvas. */
    body { background: #0d1117; }
    /* The README/source label between the listing and the rendered file
       is the only piece of chrome whose color isn't already dark-mode
       aware via github-markdown.css. #57606a is invisible on #0d1117. */
    .markdown-body p.md-serve-readme-source { color: #8b949e; }
    .markdown-body p.md-serve-breadcrumb { border-bottom-color: #30363d; }
    .markdown-body p.md-serve-breadcrumb .md-serve-breadcrumb-sep { color: #8b949e; }
    .md-serve-width-ctrl > summary,
    .md-serve-width-panel { background: #161b22; border-color: #30363d; color: #8b949e; }
    .md-serve-width-panel button { background: #21262d; border-color: #30363d; color: #c9d1d9; }
    .md-serve-width-panel button:hover { background: #30363d; }
`

var (
	chromeDarkAuto   = template.CSS("@media (prefers-color-scheme: dark) {\n" + chromeDarkRules + "  }\n")
	chromeDarkForced = template.CSS(chromeDarkRules)
)

// themeAssets bundles the per-theme pieces the page template interpolates.
type themeAssets struct {
	MarkdownCSS   string       // github-markdown stylesheet filename to link
	ChromaCSS     template.CSS // syntax-highlight stylesheet
	ChromeDarkCSS template.CSS // dark overrides for md-serve's own chrome
	ColorScheme   string       // value for the CSS `color-scheme:` property
}

// themeFor maps a resolved theme mode to the stylesheets and color-scheme a
// page should use. Any mode other than light/dark is treated as auto.
func themeFor(mode string) themeAssets {
	switch mode {
	case themeLight:
		return themeAssets{"github-markdown-light.css", chromaCSSLight, "", "light"}
	case themeDark:
		return themeAssets{"github-markdown-dark.css", chromaCSSDark, chromeDarkForced, "dark"}
	default:
		return themeAssets{"github-markdown.css", chromaCSSAuto, chromeDarkAuto, "light dark"}
	}
}

var pageTpl = template.Must(template.New("page").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<link rel="stylesheet" href="{{.AssetsPrefix}}{{.MarkdownCSS}}">
<style>{{.ChromaCSS}}</style>
<style>
  /* Advertise which scheme(s) we support so scrollbars, form controls, and
     other UA-rendered chrome adapt. "light dark" when the theme follows the
     OS; a single value when a cookie has pinned the theme. */
  :root { color-scheme: {{.ColorScheme}}; }
  body { box-sizing: border-box; min-width: 200px; max-width: var(--md-max-width, 980px); margin: 0 auto; padding: 45px; }
  @media (max-width: 767px) { body { padding: 15px; } }
  /* Directory listing: full-width table that wraps the name column and
     keeps the Size/Modified columns on a single line, so it stays
     readable on mobile and matches the document width on desktop. */
  .markdown-body table.md-serve-listing { display: table; width: 100%; }
  .markdown-body table.md-serve-listing td:first-child { word-break: break-word; }
  .markdown-body table.md-serve-listing th:nth-child(n+2),
  .markdown-body table.md-serve-listing td:nth-child(n+2) { white-space: nowrap; }
  .markdown-body p.md-serve-readme-source { margin: 16px 0 8px 0; font-size: 13px; color: #57606a; }
  /* Breadcrumb above a rendered README ("Home / README.md"). Sits a touch
     larger than the source label and gains a bottom border so it reads as a
     header strip separating navigation from the document. */
  .markdown-body p.md-serve-breadcrumb { margin: 0 0 20px 0; padding-bottom: 12px;
    font-size: 14px; border-bottom: 1px solid #d0d7de; }
  .markdown-body p.md-serve-breadcrumb .md-serve-breadcrumb-sep { color: #57606a; margin: 0 2px; }
  /* Reader-controlled page width widget. Subtle until hovered, bottom-right
     fixed, never affects document layout. Persists choice in localStorage so
     it survives reloads and applies before paint via the head script below. */
  .md-serve-width-ctrl { position: fixed; bottom: 12px; right: 12px; z-index: 9999;
    font: 12px -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
    opacity: 0.35; transition: opacity 0.15s ease; }
  .md-serve-width-ctrl:hover, .md-serve-width-ctrl[open] { opacity: 1; }
  .md-serve-width-ctrl > summary { list-style: none; cursor: pointer;
    width: 28px; height: 28px; line-height: 26px; text-align: center;
    background: #ffffff; border: 1px solid #d0d7de; border-radius: 6px;
    color: #57606a; box-shadow: 0 1px 3px rgba(27,31,36,0.08); user-select: none; }
  .md-serve-width-ctrl > summary::-webkit-details-marker { display: none; }
  .md-serve-width-ctrl[open] > summary { border-bottom-right-radius: 0; border-bottom-left-radius: 0; }
  .md-serve-width-panel { position: absolute; right: 0; bottom: 28px;
    background: #ffffff; border: 1px solid #d0d7de; border-radius: 6px 6px 0 6px;
    box-shadow: 0 1px 3px rgba(27,31,36,0.08); padding: 10px 12px;
    display: flex; flex-direction: column; gap: 8px; min-width: 220px; color: #57606a; }
  .md-serve-width-panel label { display: flex; justify-content: space-between; align-items: center; gap: 8px; }
  .md-serve-width-panel input[type=range] { flex: 1; }
  .md-serve-width-panel .md-serve-width-row { display: flex; gap: 6px; }
  .md-serve-width-panel button { flex: 1; background: #f6f8fa; border: 1px solid #d0d7de;
    border-radius: 4px; padding: 4px 8px; cursor: pointer; font: inherit; color: #24292f; }
  .md-serve-width-panel button:hover { background: #eef1f4; }
  {{.ChromeDarkCSS}}
</style>
<script>
/* Apply stored width before <body> paints, to avoid a flash at the default. */
(function(){
  try {
    var w = localStorage.getItem('md-serve-max-width');
    if (w) document.documentElement.style.setProperty('--md-max-width', w);
  } catch(e) {}
})();
</script>
</head>
<body>
<article class="markdown-body">
{{.Body}}
</article>
<details class="md-serve-width-ctrl" aria-label="Page width">
  <summary title="Page width">&#8596;</summary>
  <div class="md-serve-width-panel">
    <label>Width <span data-md-width-label>980px</span></label>
    <input type="range" min="480" max="1800" step="20" value="980" data-md-width-slider>
    <div class="md-serve-width-row">
      <button type="button" data-md-width-action="full">Full</button>
      <button type="button" data-md-width-action="reset">Reset</button>
    </div>
  </div>
</details>
<script>
(function(){
  var KEY = 'md-serve-max-width';
  var DEFAULT_PX = 980;
  var slider = document.querySelector('[data-md-width-slider]');
  var label  = document.querySelector('[data-md-width-label]');
  function setVar(v){ document.documentElement.style.setProperty('--md-max-width', v); }
  function clearVar(){ document.documentElement.style.removeProperty('--md-max-width'); }
  function syncFromStored(){
    var stored = null;
    try { stored = localStorage.getItem(KEY); } catch(e){}
    if (stored === 'none') {
      label.textContent = 'full';
      slider.value = slider.max;
    } else if (stored && /^\d+px$/.test(stored)) {
      var n = parseInt(stored, 10);
      slider.value = String(n);
      label.textContent = n + 'px';
    } else {
      slider.value = String(DEFAULT_PX);
      label.textContent = DEFAULT_PX + 'px (default)';
    }
  }
  slider.addEventListener('input', function(){
    var v = slider.value + 'px';
    setVar(v);
    label.textContent = slider.value + 'px';
    try { localStorage.setItem(KEY, v); } catch(e){}
  });
  document.querySelector('[data-md-width-action="full"]').addEventListener('click', function(){
    setVar('none');
    label.textContent = 'full';
    slider.value = slider.max;
    try { localStorage.setItem(KEY, 'none'); } catch(e){}
  });
  document.querySelector('[data-md-width-action="reset"]').addEventListener('click', function(){
    clearVar();
    try { localStorage.removeItem(KEY); } catch(e){}
    slider.value = String(DEFAULT_PX);
    label.textContent = DEFAULT_PX + 'px (default)';
  });
  syncFromStored();
})();
</script>
{{if .LiveReload}}<script>
(function(){
  var last=null;
  setInterval(function(){
    fetch('/_md-serve-livereload?path='+encodeURIComponent(location.pathname),{cache:'no-store'})
      .then(function(r){return r.ok?r.text():null;})
      .then(function(t){
        if(t==null) return;
        if(last==null){last=t;return;}
        if(t!==last) location.reload();
      })
      .catch(function(){});
  }, 1000);
})();
</script>{{end}}
</body>
</html>
`))

type pageData struct {
	Title         string
	Body          template.HTML
	AssetsPrefix  string
	MarkdownCSS   string
	ChromaCSS     template.CSS
	ChromeDarkCSS template.CSS
	ColorScheme   string
	LiveReload    bool
}

func main() {
	// Default addr: honor $PORT if set (convenient for swe-swe sessions,
	// Heroku-style deploys, etc.), otherwise :8080.
	defaultAddr := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		defaultAddr = ":" + p
	}
	var (
		addr      = flag.String("addr", defaultAddr, "address to listen on (defaults to :$PORT if set, else :8080)")
		dir       = flag.String("dir", ".", "directory to serve")
		noLive    = flag.Bool("no-live", false, "disable the auto-reload JS poller (live-reload is on by default)")
		hideDots  = flag.Bool("hide-dotfiles", false, "hide dotfiles: omit them from listings and 404 direct requests (shown by default)")
		themeCkie = flag.String("theme-cookie", "", "cookie name that pins light/dark theme (e.g. swe-swe-theme); empty = follow the browser's prefers-color-scheme")
		showVer   = flag.Bool("version", false, "print version and exit")
	)
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "md-serve — serve static files, render .md/.markdown as GitHub-styled HTML\n\n")
		fmt.Fprintf(out, "Usage: %s [flags]\n\n", os.Args[0])
		fmt.Fprintf(out, "Behavior:\n")
		fmt.Fprintf(out, "  • .md / .markdown render as GitHub-styled HTML (GFM, syntax-highlighted code blocks).\n")
		fmt.Fprintf(out, "  • Directories: index.html wins; otherwise index.md / README.md is rendered\n")
		fmt.Fprintf(out, "    under a 'Home / <file>' breadcrumb; otherwise an auto-generated listing.\n")
		fmt.Fprintf(out, "    Append ?listing=1 to any directory URL to force the bare file listing\n")
		fmt.Fprintf(out, "    (this is what the 'Home' breadcrumb links to; it also bypasses index.html).\n")
		fmt.Fprintf(out, "  • Everything else is served byte-for-byte (.js, .css, .wasm, .json, images,\n")
		fmt.Fprintf(out, "    fonts, ...) so ES module scripts and static apps Just Work.\n")
		fmt.Fprintf(out, "  • Append ?pretty=1 to a source-file URL for a syntax-highlighted view with\n")
		fmt.Fprintf(out, "    linkable line numbers (e.g. /main.go?pretty=1#L42). Directory listings\n")
		fmt.Fprintf(out, "    already link source files this way; direct URLs / curl / <script src>\n")
		fmt.Fprintf(out, "    get raw bytes.\n")
		fmt.Fprintf(out, "  • Append ?pretty=0 to a markdown or .html URL to fetch the raw source\n")
		fmt.Fprintf(out, "    instead of the rendered page (Content-Type: text/plain).\n")
		fmt.Fprintf(out, "  • Clients whose Accept header doesn't include text/html (curl -H, fetch\n")
		fmt.Fprintf(out, "    with Accept: application/json, ...) get raw bytes regardless of pretty.\n")
		fmt.Fprintf(out, "  • Theme follows the browser's prefers-color-scheme (github light/dark).\n")
		fmt.Fprintf(out, "    Pass -theme-cookie NAME to instead pin the theme from a request cookie\n")
		fmt.Fprintf(out, "    (value light/dark), e.g. -theme-cookie swe-swe-theme to match a host UI.\n")
		fmt.Fprintf(out, "  • Live-reload polls every second; pass -no-live to disable.\n")
		fmt.Fprintf(out, "  • Dotfiles are served and listed by default; pass -hide-dotfiles to\n")
		fmt.Fprintf(out, "    omit them from listings and 404 direct requests. Path traversal\n")
		fmt.Fprintf(out, "    outside -dir is always rejected.\n\n")
		fmt.Fprintf(out, "Flags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVer {
		fmt.Printf("md-serve %s (%s)\n", version, commit)
		return
	}

	root, err := filepath.Abs(*dir)
	if err != nil {
		log.Fatalf("md-serve: resolve dir: %v", err)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		log.Fatalf("md-serve: -dir %q is not a directory", root)
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

	live := !*noLive
	h := &fileHandler{root: root, md: md, live: live, hideDotfiles: *hideDots, themeCookie: *themeCkie}
	mux := http.NewServeMux()
	mux.Handle(assetsPrefix, http.StripPrefix(assetsPrefix, http.FileServerFS(mustSub(assetsFS, "assets"))))
	if live {
		mux.HandleFunc(livereloadPath, h.livereload)
	}
	mux.Handle("/", h)

	srv := &http.Server{Addr: *addr, Handler: logMiddleware(mux)}
	liveSuffix := ""
	if live {
		liveSuffix = " [live-reload]"
	}
	log.Printf("md-serve %s (%s) serving %s on http://%s%s", version, commit, root, *addr, liveSuffix)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("md-serve: listen: %v", err)
	}
}

func mustSub(f embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

type fileHandler struct {
	root         string
	md           goldmark.Markdown
	live         bool
	hideDotfiles bool   // when true, dotfiles are 404'd and omitted from listings
	themeCookie  string // when non-empty, this request cookie pins light/dark; else follow prefers-color-scheme
}

// themeMode resolves the theme for a request. With -theme-cookie unset it's
// always themeAuto (follow the browser). Otherwise a cookie of that name whose
// value is "light" or "dark" pins the theme; anything else falls back to auto.
func (h *fileHandler) themeMode(r *http.Request) string {
	if h.themeCookie == "" {
		return themeAuto
	}
	if c, err := r.Cookie(h.themeCookie); err == nil {
		switch c.Value {
		case themeLight, themeDark:
			return c.Value
		}
	}
	return themeAuto
}

// newPageData builds a pageData with the fields common to every rendered page
// (assets, resolved-theme stylesheets, live-reload) filled in, so each render
// path only has to supply the title and body.
func (h *fileHandler) newPageData(r *http.Request, title string, body template.HTML) pageData {
	ta := themeFor(h.themeMode(r))
	return pageData{
		Title:         title,
		Body:          body,
		AssetsPrefix:  assetsPrefix,
		MarkdownCSS:   ta.MarkdownCSS,
		ChromaCSS:     ta.ChromaCSS,
		ChromeDarkCSS: ta.ChromeDarkCSS,
		ColorScheme:   ta.ColorScheme,
		LiveReload:    h.live,
	}
}

// hasHiddenSegment reports whether any component of the cleaned URL path is a
// dotfile (begins with "."). Used to gate access when -hide-dotfiles is set.
// urlPath is already path.Clean'd and starts with "/", so "." and ".." have
// been resolved away and the empty leading/trailing segments don't match.
func hasHiddenSegment(urlPath string) bool {
	for _, seg := range strings.Split(urlPath, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

func (h *fileHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	urlPath := path.Clean("/" + r.URL.Path)
	// When -hide-dotfiles is set, a request for any dotfile path (or a path
	// under a dotfile directory) is treated as if it doesn't exist, so we
	// don't leak bytes the listing already omits.
	if h.hideDotfiles && hasHiddenSegment(urlPath) {
		http.NotFound(w, r)
		return
	}
	fsPath, ok := safeJoin(h.root, urlPath)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	info, err := os.Stat(fsPath)
	if errors.Is(err, fs.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if info.IsDir() {
		// Ensure trailing slash so relative links resolve correctly.
		if !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return
		}
		h.serveDir(w, r, fsPath, urlPath)
		return
	}
	h.serveFile(w, r, fsPath, urlPath)
}

// serveDir implements the directory-handling rules:
//
//  1. If an index.html is present, serve it raw via http.ServeFile so
//     the user's own HTML is delivered byte-for-byte. This matches
//     nginx, Apache, Caddy, GitHub Pages, and every CDN — index.html
//     wins at directory roots so md-serve can host real static apps.
//  2. Else if a markdown index is present (index.md / README.md /
//     readme.md / index.markdown), render a GitHub-style combined page
//     with the directory listing at the top and the rendered README
//     below.
//  3. Else, serve the auto-generated directory listing.
//
// The ?listing=1 escape hatch forces rule 3 unconditionally: it's what the
// "Home" breadcrumb on a combined page links to, so the reader can always get
// back to the bare file listing — even in a directory whose index.html would
// otherwise pass through and hide it entirely.
func (h *fileHandler) serveDir(w http.ResponseWriter, r *http.Request, fsPath, urlPath string) {
	if b, err := strconv.ParseBool(r.URL.Query().Get("listing")); err == nil && b {
		h.serveDirIndex(w, r, fsPath, urlPath)
		return
	}
	// 1. index.html → raw pass-through (matches every other static host)
	indexHTML := filepath.Join(fsPath, "index.html")
	if s, err := os.Stat(indexHTML); err == nil && !s.IsDir() {
		http.ServeFile(w, r, indexHTML)
		return
	}
	// 2. Markdown index → combined page
	for _, name := range []string{"index.md", "README.md", "readme.md", "index.markdown"} {
		p := filepath.Join(fsPath, name)
		if s, err := os.Stat(p); err == nil && !s.IsDir() {
			h.serveCombinedDir(w, r, fsPath, urlPath, p)
			return
		}
	}
	// 3. Generated listing
	h.serveDirIndex(w, r, fsPath, urlPath)
}

// prettyEntry describes how a file extension behaves with respect to the
// `?pretty=` query string: which renderer to invoke when pretty mode is on,
// and whether pretty mode is the default for that extension. Extensions not
// listed in prettyRegistry implicitly use chroma as their pretty renderer
// and default to raw.
type prettyEntry struct {
	render          func(*fileHandler, http.ResponseWriter, *http.Request, string, string)
	prettyByDefault bool
}

// prettyRegistry is the single source of truth for per-extension behavior.
// Markdown is rendered server-side (goldmark); .html is rendered by the
// browser (http.ServeFile sends Content-Type: text/html). Both default to
// pretty=true. Everything else is implicitly { chroma, prettyByDefault: false }.
var prettyRegistry = map[string]prettyEntry{
	".md":       {(*fileHandler).renderMarkdownPage, true},
	".markdown": {(*fileHandler).renderMarkdownPage, true},
	".html":     {(*fileHandler).serveBrowserHTML, true},
	".htm":      {(*fileHandler).serveBrowserHTML, true},
}

func (h *fileHandler) serveFile(w http.ResponseWriter, r *http.Request, fsPath, urlPath string) {
	ext := strings.ToLower(filepath.Ext(fsPath))
	entry, registered := prettyRegistry[ext]
	if !registered {
		entry = prettyEntry{render: (*fileHandler).tryChromaHighlight, prettyByDefault: false}
	}

	pretty := entry.prettyByDefault
	if v := r.URL.Query().Get("pretty"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			pretty = b
		}
	}
	// A client whose Accept header doesn't include text/html (curl -H,
	// fetch with Accept: application/json, an SDK that wants the bytes)
	// shouldn't get the HTML viewer wrapper even when pretty would
	// otherwise apply — wrapping their response in <html> is just noise
	// they'd then have to strip. Treat as ?pretty=0.
	if pretty && !acceptsHTML(r) {
		pretty = false
	}

	if pretty {
		entry.render(h, w, r, fsPath, urlPath)
		return
	}

	// Raw mode. For extensions whose default is pretty (markdown, html), force
	// text/plain so the browser shows the source instead of auto-rendering it,
	// and use http.ServeContent rather than http.ServeFile because the latter
	// 301-redirects /foo/index.html → /foo/ (canonical-URL behavior baked into
	// the stdlib) and would drop the headers we set.
	if entry.prettyByDefault {
		f, err := os.Open(fsPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, fsPath, info.ModTime(), f)
		return
	}
	http.ServeFile(w, r, fsPath)
}

// renderMarkdownPage is the pretty renderer for .md / .markdown: convert the
// file with goldmark and wrap it in the GitHub-styled page template.
func (h *fileHandler) renderMarkdownPage(w http.ResponseWriter, r *http.Request, fsPath, urlPath string) {
	src, err := os.ReadFile(fsPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := h.md.Convert(src, &buf); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	title := filepath.Base(fsPath)
	data := h.newPageData(r, title, template.HTML(buf.String()))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTpl.Execute(w, data); err != nil {
		log.Printf("md-serve: template: %v", err)
	}
}

// serveBrowserHTML is the pretty renderer for .html / .htm: hand the file to
// http.ServeFile so it goes out with Content-Type: text/html and the browser
// renders it. (The "rendering" happens client-side, but it's still pretty mode
// from md-serve's perspective — `?pretty=0` is what gets you the source.)
func (h *fileHandler) serveBrowserHTML(w http.ResponseWriter, r *http.Request, fsPath, urlPath string) {
	http.ServeFile(w, r, fsPath)
}

// tryChromaHighlight is the pretty renderer for everything not in the registry.
// We only highlight when the file is small enough to be cheap, looks like text,
// and chroma can find a lexer for it (by filename, extension, or content);
// otherwise we fall through to the raw bytes via http.ServeFile so binaries
// and unknown formats don't get mangled into a fake "code" page.
func (h *fileHandler) tryChromaHighlight(w http.ResponseWriter, r *http.Request, fsPath, urlPath string) {
	if info, err := os.Stat(fsPath); err == nil && info.Size() <= maxHighlightBytes {
		if src, err := os.ReadFile(fsPath); err == nil && isTextLike(src) {
			if lexer := pickLexer(filepath.Base(fsPath), src); lexer != nil {
				h.serveHighlighted(w, r, fsPath, urlPath, src, lexer)
				return
			}
		}
	}
	http.ServeFile(w, r, fsPath)
}

// listingHTML builds an HTML <table> of the directory contents as a
// markdown-body fragment with Name / Size / Modified columns. Includes a
// parent "../" link when urlPath is not the root. Dotfiles are included
// unless -hide-dotfiles is set. Directories are sorted before files; within
// each group, alphabetically.
func (h *fileHandler) listingHTML(fsPath, urlPath string) (template.HTML, error) {
	entries, err := os.ReadDir(fsPath)
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool {
		ai, aj := entries[i].IsDir(), entries[j].IsDir()
		if ai != aj {
			return ai
		}
		return entries[i].Name() < entries[j].Name()
	})
	var b strings.Builder
	b.WriteString(`<table class="md-serve-listing">
<thead><tr><th>Name</th><th style="text-align:right">Size</th><th>Modified</th></tr></thead>
<tbody>
`)
	if urlPath != "/" {
		b.WriteString(`<tr><td><a href="../">../</a></td><td></td><td></td></tr>` + "\n")
	}
	for _, e := range entries {
		name := e.Name()
		if h.hideDotfiles && strings.HasPrefix(name, ".") {
			continue
		}
		display := name
		link := name
		size := ""
		modified := ""
		if info, err := e.Info(); err == nil {
			modified = info.ModTime().Local().Format("2006-01-02 15:04")
			if !e.IsDir() {
				size = humanSize(info.Size())
			}
		}
		if e.IsDir() {
			display += "/"
			link += "/"
		} else if shouldPrettyLink(name) {
			// Source file we'd highlight: link with ?pretty=1 so a click
			// from the listing lands on the highlighted view, while
			// direct URLs / <script src> still get raw bytes.
			link += "?pretty=1"
		}
		fmt.Fprintf(&b,
			`<tr><td><a href="%s">%s</a></td><td style="text-align:right">%s</td><td>%s</td></tr>`+"\n",
			html.EscapeString(link),
			html.EscapeString(display),
			html.EscapeString(size),
			html.EscapeString(modified),
		)
	}
	b.WriteString("</tbody></table>\n")
	return template.HTML(b.String()), nil
}

// humanSize formats a byte count using binary (KiB/MiB/...) units, with
// no decimals for plain bytes and one decimal otherwise.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func (h *fileHandler) serveDirIndex(w http.ResponseWriter, r *http.Request, fsPath, urlPath string) {
	listing, err := h.listingHTML(fsPath, urlPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body := fmt.Sprintf(`<h1 id="index-of">Index of %s</h1>%s`,
		html.EscapeString(urlPath), string(listing))
	data := h.newPageData(r, "Index of "+urlPath, template.HTML(body))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = pageTpl.Execute(w, data)
}

// serveCombinedDir renders a directory as a single page: a breadcrumb across
// the top, then the rendered README below, styled after github.com's repo-home
// layout. The full file listing isn't shown inline — the "Home" crumb links to
// it (?listing=1) so the README leads and the listing is one click away.
func (h *fileHandler) serveCombinedDir(w http.ResponseWriter, r *http.Request, fsPath, urlPath, readmePath string) {
	src, err := os.ReadFile(readmePath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var readme bytes.Buffer
	if err := h.md.Convert(src, &readme); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Breadcrumb: "Home / README.md", then the rendered README. "Home" points
	// at ?listing=1 (the bare file listing for this directory); the filename
	// crumb links to the README on its own page.
	readmeName := filepath.Base(readmePath)
	body := fmt.Sprintf(
		`<p class="md-serve-breadcrumb"><a href="?listing=1">Home</a> <span class="md-serve-breadcrumb-sep">/</span> <a href="%s">%s</a></p>%s`,
		html.EscapeString(readmeName),
		html.EscapeString(readmeName),
		readme.String(),
	)
	data := h.newPageData(r, readmeName+" — "+urlPath, template.HTML(body))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = pageTpl.Execute(w, data)
}

// serveHighlighted renders a source file as a syntax-highlighted HTML
// page using chroma. The surrounding chrome (filename label, "raw" link)
// matches the README label used by combined dir pages.
func (h *fileHandler) serveHighlighted(w http.ResponseWriter, r *http.Request, fsPath, urlPath string, src []byte, lexer chroma.Lexer) {
	iterator, err := chroma.Coalesce(lexer).Tokenise(nil, string(src))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var code bytes.Buffer
	if err := chromaFormatter.Format(&code, chromaLightStyle, iterator); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	name := filepath.Base(fsPath)
	// Use the absolute URL path for the raw link so it resolves the same
	// way regardless of the document's base URL (avoids the trap where a
	// relative href would resolve against a parent path in some browsers
	// / redirect chains). Raw is the default for source files now, so the
	// link is just the bare URL with no query string.
	rawHref := html.EscapeString(urlPath)
	body := fmt.Sprintf(
		`<p class="md-serve-readme-source"><a href="%s">%s</a> · <a href="%s">raw</a></p>%s`,
		rawHref,
		html.EscapeString(name),
		rawHref,
		code.String(),
	)
	data := h.newPageData(r, name+" — "+urlPath, template.HTML(body))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = pageTpl.Execute(w, data)
}

// shouldPrettyLink reports whether a directory-listing entry for the
// given filename should carry a `?pretty=1` query string. We say yes
// when chroma can identify a lexer by filename — that's a cheap proxy
// for "this is source code we'd render as a highlighted view" and it
// avoids reading every file in the directory just to build a listing.
// Markdown files have their own rendering path; .html/.htm are served
// raw as the static-server's primary payload, so neither category gets
// `?pretty=1` even if a lexer exists for them.
func shouldPrettyLink(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown", ".html", ".htm":
		return false
	}
	return lexers.Match(name) != nil
}

// pickLexer chooses a chroma lexer for a file. Tries filename match
// (handles Makefile, Dockerfile, go.mod, etc.) first, then content
// analysis as a fallback. Returns nil if neither yields a real match —
// the caller should fall back to byte-for-byte serving in that case
// rather than highlight as plain text, since most "unknown" hits in a
// docs tree are random binary or proprietary formats we shouldn't dress
// up as code.
func pickLexer(filename string, content []byte) chroma.Lexer {
	if l := lexers.Match(filename); l != nil {
		return l
	}
	if l := lexers.Analyse(string(content)); l != nil {
		return l
	}
	return nil
}

// acceptsHTML reports whether the request's Accept header indicates the
// client is willing to render HTML. Missing/empty Accept means "anything"
// per RFC 9110, so we say yes. Otherwise we say yes only if one of the
// listed media ranges covers text/html: text/html, text/*, */*, or
// application/xhtml+xml. q-values aren't parsed — q=0 explicit rejection
// is rare enough in practice that the simpler check is worth the trade.
func acceptsHTML(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if accept == "" {
		return true
	}
	for _, part := range strings.Split(accept, ",") {
		mt := strings.TrimSpace(part)
		if i := strings.Index(mt, ";"); i >= 0 {
			mt = strings.TrimSpace(mt[:i])
		}
		switch strings.ToLower(mt) {
		case "text/html", "application/xhtml+xml", "text/*", "*/*":
			return true
		}
	}
	return false
}

// isTextLike does a cheap binary-vs-text sniff: scan the first 512 bytes
// for a NUL byte. Far from perfect (UTF-16, some binary formats without
// NULs early on, etc.) but catches the common "don't try to highlight a
// PNG" case with no false positives on real source code.
func isTextLike(content []byte) bool {
	n := len(content)
	if n > 512 {
		n = 512
	}
	for i := 0; i < n; i++ {
		if content[i] == 0 {
			return false
		}
	}
	return true
}

// livereload returns the source-file mtime (as Unix nanos) for the page
// at ?path=<url-path>, so the injected live-reload JS can poll and reload
// when the value changes. For directories the result is the max mtime across
// the dir itself and its immediate entries (dotfiles included unless
// -hide-dotfiles is set) — that's enough to catch both content edits to the
// rendered README and add/remove of files in the listing.
func (h *fileHandler) livereload(w http.ResponseWriter, r *http.Request) {
	urlPath := r.URL.Query().Get("path")
	if urlPath == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}
	cleaned := path.Clean("/" + urlPath)
	if h.hideDotfiles && hasHiddenSegment(cleaned) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	fsPath, ok := safeJoin(h.root, cleaned)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	info, err := os.Stat(fsPath)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	mtime := info.ModTime().UnixNano()
	if info.IsDir() {
		if entries, err := os.ReadDir(fsPath); err == nil {
			for _, e := range entries {
				if h.hideDotfiles && strings.HasPrefix(e.Name(), ".") {
					continue
				}
				if i, err := e.Info(); err == nil {
					if t := i.ModTime().UnixNano(); t > mtime {
						mtime = t
					}
				}
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "%d", mtime)
}

// safeJoin joins root with urlPath (already cleaned, starts with "/") and
// ensures the result stays inside root. Returns false if not.
func safeJoin(root, urlPath string) (string, bool) {
	p := filepath.Join(root, filepath.FromSlash(urlPath))
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	if abs != rootAbs && !strings.HasPrefix(abs, rootAbs+string(filepath.Separator)) {
		return "", false
	}
	return abs, true
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
