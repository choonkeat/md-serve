# md-serve

Tiny static file server that renders Markdown (`.md`, `.markdown`) as
GitHub-styled HTML. Single Go binary, distributed via npm with native
binaries for Linux / macOS / Windows on x64 and arm64.

## Go to file

Every page carries a top bar: the current path on the left (each ancestor
segment is a link), a **🔍 Go to file** button on the right. It opens a
fuzzy file finder over everything under the served directory, the way
`t` does on GitHub and `⌘P` does in VS Code.

- Open it with the button, or press `t`, `/`, `⌘K` / `Ctrl-K`, or
  `⌘P` / `Ctrl-P`. Keys are ignored while you're typing in a field.
- Type any subsequence of the path: `smg` finds `src/main.go`,
  `pkgjson` finds `package.json`. A contiguous match ranks above a
  scattered one, a tight match above a loose one, a short path above a
  long one.
- `↑` `↓` to move, `↵` to open, `⌘↵` / `Ctrl-↵` for a new tab, `esc` to
  close. On touch, rows are full-width tap targets showing the filename
  above its directory, with a **Cancel** button instead of `esc`.
- Results link exactly where the directory listing would: `?pretty=1`
  for source files, trailing `/` for directories.

The tree is walked per keystroke (debounced) rather than indexed at
startup, so a file you just created is findable immediately. The walk
stops after 500 matches or 20,000 entries — a truncated list says
`first 50` in its footer. `.git` is skipped; other dotfiles follow
`-hide-dotfiles`.

The endpoint behind it is plain JSON, if you want it directly:

```sh
curl 'http://localhost:8080/_md-serve-search?q=pkgjson'
```

## Protip: query strings

A few query strings change how a URL is served — handy when the default
rendering isn't what you want:

| Append to a URL | What you get |
| --- | --- |
| `?listing=1` | The bare file listing for a directory — even when an `index.html` or `README.md` would otherwise take over. This is what the **Home** breadcrumb links to. |
| `?pretty=0` | The **raw source** of a file that normally renders — e.g. `README.md?pretty=0` or `index.html?pretty=0` returns the bytes as `text/plain` instead of the rendered page. |
| `?pretty=1` | A **syntax-highlighted** view of a source file, with linkable line numbers (e.g. `main.go?pretty=1#L42`). |

## Install

```sh
npm install -g @choonkeat/md-serve
# or one-shot:
npx @choonkeat/md-serve
```

(The published name is scoped — npm rejected the unscoped `md-serve`
as too similar to an existing package. The installed binary is still
called `md-serve`.)

## Usage

```sh
md-serve                  # serve $PWD on :$PORT (or :8080), live-reload on
md-serve -dir ./docs      # serve a specific directory
md-serve -addr :3000      # bind to a specific address
md-serve -no-live         # disable the live-reload poller
md-serve -hide-dotfiles   # hide dotfiles (omit from listings, 404 direct requests)
md-serve -theme-cookie swe-swe-theme  # pin light/dark from a request cookie
md-serve -version
```

## Behavior

- `.md` / `.markdown` files render as HTML using
  [goldmark](https://github.com/yuin/goldmark) with GFM, styled with
  [github-markdown-css](https://github.com/sindresorhus/github-markdown-css).
  Fenced code blocks are syntax-highlighted via
  [chroma](https://github.com/alecthomas/chroma) (200+ languages).
- Directories: if `index.html` is present it's served raw (matches
  nginx / Apache / Caddy / GitHub Pages, so md-serve can host real
  static apps). Otherwise, if `index.md` / `README.md` / `readme.md` /
  `index.markdown` is present, it's rendered GitHub-style under a
  `Home / <file>` breadcrumb — the **Home** crumb (a link to
  `?listing=1`) drops you to the bare file listing. Otherwise a plain
  generated listing with **Name / Size / Modified** columns. Appending
  `?listing=1` to any directory URL forces that listing, bypassing
  `index.html` too.
- Everything else is served byte-for-byte. That means `.js`, `.css`,
  `.wasm`, `.json`, images, fonts, and the rest all reach the browser
  with their normal MIME types — ES module scripts load, fetch() works,
  the static-app use case Just Works.
- Source files (`.go`, `.py`, `.yaml`, `.toml`, `Dockerfile`,
  `Makefile`, ...) can be viewed as syntax-highlighted HTML with
  linkable line numbers (`/main.go#L42`) by appending `?pretty=1` to
  the URL. Directory listings already link source files this way, so
  clicking from a listing lands on the highlighted view while direct
  URLs / `curl` / `<script src>` get the raw bytes. Files larger than
  1 MiB, files that look binary, or files chroma can't lex stay raw
  even with `?pretty=1`.
- Pretty rendering also gates on the `Accept` header: a client whose
  Accept doesn't include `text/html` (e.g. `curl -H 'Accept:
  application/json'`, `fetch()` with an explicit Accept) gets raw
  bytes regardless of `?pretty=1` or the extension's default. So an
  API consumer asking for `/README.md` with `Accept: application/json`
  gets the source, not an HTML wrapper.
- Theme follows the browser's `prefers-color-scheme` (GitHub light /
  dark) out of the box. Pass `-theme-cookie NAME` to instead pin the
  theme from a request cookie whose value is `light` or `dark` — e.g.
  `-theme-cookie swe-swe-theme` so md-serve matches a host UI that
  already sets that cookie. No cookie (or an unrecognized value) falls
  back to following the browser.
- Dotfiles are served and shown in listings by default. Pass
  `-hide-dotfiles` to omit them from listings and return `404` for
  direct requests to any dotfile (or path under a dotfile directory).
- Path traversal is blocked: requests are rejected if they resolve
  outside the served root.
- Live-reload is on by default: rendered pages poll a tiny endpoint
  once a second and reload themselves when the underlying file's mtime
  changes. Pass `-no-live` to disable (e.g. for production-style
  serving where you don't want the extra requests).

## Develop

```sh
make build              # cross-compile all 6 platform binaries → npm-platforms/, then link
make link               # just the link step: put this checkout's md-serve on PATH
make test               # go vet
make publish-dry        # rehearse the npm publish
make publish            # ship to npm
make bump VERSION=x.y.z # sync version across package.json + optionalDependencies
```

The npm package layout: a thin shim (`bin/md-serve.js`) selects the
right `@choonkeat/md-serve-<platform>-<arch>` optionalDependency at
runtime, falling back to a locally built binary in `npm-platforms/`
during development.

That fallback is what makes `make build` enough to run your own changes:
`npm link` symlinks the shim onto your PATH, and the shim prefers
`npm-platforms/` over the published package, so `md-serve` means "this
working tree". `make build` prints the resolved path and version banner
so you can see which build you're about to run. One thing it can't
reach: `npx @choonkeat/md-serve` resolves from the npx cache and keeps
running the last *published* release regardless.

## License

MIT
