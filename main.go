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
	"strings"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	gmhtml "github.com/yuin/goldmark/renderer/html"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
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

// Endpoint used by injected -live JS to poll for source-file changes.
const livereloadPath = "/_md-serve-livereload"

// maxHighlightBytes caps how big a source file we'll syntax-highlight.
// Above this, we fall back to byte-for-byte serving via http.ServeFile so
// the browser handles the download instead of us building a giant DOM.
const maxHighlightBytes = 1 << 20 // 1 MiB

// chromaFormatter renders chroma tokens to HTML with inline styles, line
// numbers in a separate <td> column, and linkable anchors so URLs like
// /main.go#L42 jump to the right line.
var chromaFormatter = chromahtml.New(
	chromahtml.WithClasses(false),
	chromahtml.WithLineNumbers(true),
	chromahtml.LineNumbersInTable(true),
	chromahtml.WithLinkableLineNumbers(true, "L"),
)

// chromaStyle is the colour theme for both markdown fenced blocks (via
// goldmark-highlighting) and standalone source files. Picked to match
// the rest of the GitHub-styled chrome.
var chromaStyle = func() *chroma.Style {
	s := styles.Get("github")
	if s == nil {
		s = styles.Fallback
	}
	return s
}()

var pageTpl = template.Must(template.New("page").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<link rel="stylesheet" href="{{.AssetsPrefix}}github-markdown.css">
<style>
  body { box-sizing: border-box; min-width: 200px; max-width: 980px; margin: 0 auto; padding: 45px; }
  @media (max-width: 767px) { body { padding: 15px; } }
  /* Directory listing: full-width table that wraps the name column and
     keeps the Size/Modified columns on a single line, so it stays
     readable on mobile and matches the document width on desktop. */
  .markdown-body table.md-serve-listing { display: table; width: 100%; }
  .markdown-body table.md-serve-listing td:first-child { word-break: break-word; }
  .markdown-body table.md-serve-listing th:nth-child(n+2),
  .markdown-body table.md-serve-listing td:nth-child(n+2) { white-space: nowrap; }
  .markdown-body p.md-serve-readme-source { margin: 16px 0 8px 0; font-size: 13px; color: #57606a; }
</style>
</head>
<body>
<article class="markdown-body">
{{.Body}}
</article>
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
	Title        string
	Body         template.HTML
	AssetsPrefix string
	LiveReload   bool
}

func main() {
	// Default addr: honor $PORT if set (convenient for swe-swe sessions,
	// Heroku-style deploys, etc.), otherwise :8080.
	defaultAddr := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		defaultAddr = ":" + p
	}
	var (
		addr    = flag.String("addr", defaultAddr, "address to listen on (defaults to :$PORT if set, else :8080)")
		dir     = flag.String("dir", ".", "directory to serve")
		live    = flag.Bool("live", false, "inject a small JS poller that auto-reloads pages when their source file changes (dev only)")
		showVer = flag.Bool("version", false, "print version and exit")
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "md-serve — serve static files, render .md/.markdown as GitHub-styled HTML\n\n")
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [flags]\n\nFlags:\n", os.Args[0])
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
				highlighting.WithFormatOptions(chromahtml.WithClasses(false)),
			),
		),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		goldmark.WithRendererOptions(gmhtml.WithUnsafe()),
	)

	h := &fileHandler{root: root, md: md, live: *live}
	mux := http.NewServeMux()
	mux.Handle(assetsPrefix, http.StripPrefix(assetsPrefix, http.FileServerFS(mustSub(assetsFS, "assets"))))
	if *live {
		mux.HandleFunc(livereloadPath, h.livereload)
	}
	mux.Handle("/", h)

	srv := &http.Server{Addr: *addr, Handler: logMiddleware(mux)}
	liveSuffix := ""
	if *live {
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
	root string
	md   goldmark.Markdown
	live bool
}

func (h *fileHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	urlPath := path.Clean("/" + r.URL.Path)
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
//  1. If a markdown index is present (index.md / README.md / readme.md /
//     index.markdown), render a GitHub-style combined page with the
//     directory listing at the top and the rendered README below.
//  2. Else if an index.html is present, serve it raw via http.ServeFile
//     so the user's own HTML is delivered byte-for-byte.
//  3. Else, serve the auto-generated directory listing.
func (h *fileHandler) serveDir(w http.ResponseWriter, r *http.Request, fsPath, urlPath string) {
	// 1. Markdown index → combined page
	for _, name := range []string{"index.md", "README.md", "readme.md", "index.markdown"} {
		p := filepath.Join(fsPath, name)
		if s, err := os.Stat(p); err == nil && !s.IsDir() {
			h.serveCombinedDir(w, r, fsPath, urlPath, p)
			return
		}
	}
	// 2. index.html → raw pass-through
	indexHTML := filepath.Join(fsPath, "index.html")
	if s, err := os.Stat(indexHTML); err == nil && !s.IsDir() {
		http.ServeFile(w, r, indexHTML)
		return
	}
	// 3. Generated listing
	h.serveDirIndex(w, r, fsPath, urlPath)
}

func (h *fileHandler) serveFile(w http.ResponseWriter, r *http.Request, fsPath, urlPath string) {
	ext := strings.ToLower(filepath.Ext(fsPath))
	if ext == ".md" || ext == ".markdown" {
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
		data := pageData{
			Title:        title,
			Body:         template.HTML(buf.String()),
			AssetsPrefix: assetsPrefix,
			LiveReload:   h.live,
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := pageTpl.Execute(w, data); err != nil {
			log.Printf("md-serve: template: %v", err)
		}
		return
	}
	// Source-code highlighting branch: opt-out via ?raw=1. We only render
	// when the file is small enough to be cheap, looks like text, and
	// chroma can find a lexer for it (by filename, extension, or content).
	if r.URL.Query().Get("raw") == "" {
		if info, err := os.Stat(fsPath); err == nil && info.Size() <= maxHighlightBytes {
			if src, err := os.ReadFile(fsPath); err == nil && isTextLike(src) {
				if lexer := pickLexer(filepath.Base(fsPath), src); lexer != nil {
					h.serveHighlighted(w, r, fsPath, urlPath, src, lexer)
					return
				}
			}
		}
	}
	http.ServeFile(w, r, fsPath)
}

// listingHTML builds an HTML <table> of the directory contents as a
// markdown-body fragment with Name / Size / Modified columns. Includes a
// parent "../" link when urlPath is not the root. Dotfiles are skipped.
// Directories are sorted before files; within each group, alphabetically.
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
		if strings.HasPrefix(name, ".") {
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
	data := pageData{
		Title:        "Index of " + urlPath,
		Body:         template.HTML(body),
		AssetsPrefix: assetsPrefix,
		LiveReload:   h.live,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = pageTpl.Execute(w, data)
}

// serveCombinedDir renders a directory as a single page containing the
// file listing on top and the rendered README below, styled after
// github.com's repo-home layout.
func (h *fileHandler) serveCombinedDir(w http.ResponseWriter, r *http.Request, fsPath, urlPath, readmePath string) {
	listing, err := h.listingHTML(fsPath, urlPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
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
	// Listing block labelled with the current path, then a horizontal rule,
	// then a small label naming the README file we picked, then its
	// rendered content. The label is a link to the file itself so the user
	// can open it on its own page.
	readmeName := filepath.Base(readmePath)
	body := fmt.Sprintf(
		`<h2 id="md-serve-dir-listing" style="margin-top:0">Files in %s</h2>%s<hr><p class="md-serve-readme-source"><a href="%s">%s</a></p>%s`,
		html.EscapeString(urlPath),
		string(listing),
		html.EscapeString(readmeName),
		html.EscapeString(readmeName),
		readme.String(),
	)
	data := pageData{
		Title:        readmeName + " — " + urlPath,
		Body:         template.HTML(body),
		AssetsPrefix: assetsPrefix,
		LiveReload:   h.live,
	}
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
	if err := chromaFormatter.Format(&code, chromaStyle, iterator); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	name := filepath.Base(fsPath)
	body := fmt.Sprintf(
		`<p class="md-serve-readme-source"><a href="%s?raw=1">%s</a> · <a href="?raw=1">raw</a></p>%s`,
		html.EscapeString(name),
		html.EscapeString(name),
		code.String(),
	)
	data := pageData{
		Title:        name + " — " + urlPath,
		Body:         template.HTML(body),
		AssetsPrefix: assetsPrefix,
		LiveReload:   h.live,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = pageTpl.Execute(w, data)
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
// at ?path=<url-path>, so the injected -live JS can poll and reload when
// the value changes. For directories the result is the max mtime across
// the dir itself and its immediate non-dotfile entries — that's enough
// to catch both content edits to the rendered README and add/remove of
// files in the listing.
func (h *fileHandler) livereload(w http.ResponseWriter, r *http.Request) {
	urlPath := r.URL.Query().Get("path")
	if urlPath == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}
	cleaned := path.Clean("/" + urlPath)
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
				if strings.HasPrefix(e.Name(), ".") {
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
