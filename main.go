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

	"github.com/yuin/goldmark"
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
</style>
</head>
<body>
<article class="markdown-body">
{{.Body}}
</article>
</body>
</html>
`))

type pageData struct {
	Title        string
	Body         template.HTML
	AssetsPrefix string
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
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		goldmark.WithRendererOptions(gmhtml.WithUnsafe()),
	)

	mux := http.NewServeMux()
	mux.Handle(assetsPrefix, http.StripPrefix(assetsPrefix, http.FileServerFS(mustSub(assetsFS, "assets"))))
	mux.Handle("/", &fileHandler{root: root, md: md})

	srv := &http.Server{Addr: *addr, Handler: logMiddleware(mux)}
	log.Printf("md-serve %s (%s) serving %s on http://%s", version, commit, root, *addr)
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
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := pageTpl.Execute(w, data); err != nil {
			log.Printf("md-serve: template: %v", err)
		}
		return
	}
	http.ServeFile(w, r, fsPath)
}

// listingHTML builds a rendered <ul> of the directory contents as a
// markdown-body HTML fragment. Includes a parent "../" link when urlPath
// is not the root. Dotfiles are skipped. Directories are sorted before
// files; within each group, alphabetically.
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
	var md strings.Builder
	if urlPath != "/" {
		md.WriteString("- [../](../)\n")
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		display := name
		link := name
		if e.IsDir() {
			display += "/"
			link += "/"
		}
		fmt.Fprintf(&md, "- [%s](%s)\n", display, link)
	}
	var buf bytes.Buffer
	if err := h.md.Convert([]byte(md.String()), &buf); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil
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
	// then the rendered README.
	body := fmt.Sprintf(
		`<h2 id="md-serve-dir-listing" style="margin-top:0">Files in %s</h2>%s<hr>%s`,
		html.EscapeString(urlPath),
		string(listing),
		readme.String(),
	)
	data := pageData{
		Title:        filepath.Base(readmePath) + " — " + urlPath,
		Body:         template.HTML(body),
		AssetsPrefix: assetsPrefix,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = pageTpl.Execute(w, data)
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
