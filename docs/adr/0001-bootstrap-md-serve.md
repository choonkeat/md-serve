# ADR 0001 — Bootstrap md-serve: purpose and tech stack

- **Status:** Accepted
- **Date:** 2026-04-11

## Context

We wanted a tiny CLI that can be pointed at any directory and will:

1. Serve static files (images, HTML, data) as a normal file server.
2. Render `.md` / `.markdown` files as HTML in **GitHub's visual style**, so a
   README previews the same way it does on github.com.
3. Be trivial to run ad hoc — ideally a one-liner with no install step, no
   runtime dependencies on the user's machine, and no config file.

This capability is useful for:

- Previewing READMEs/handbooks/specs while authoring them.
- Sharing a folder of mixed static assets + markdown docs over HTTP for a
  demo without setting up a "real" site.
- Quick documentation previews inside containerized dev environments (e.g.
  swe-swe sessions), where we want to bind to a `$PORT` the host already
  wired up.

### Options considered before building

We surveyed the existing ecosystem first:

| Tool | Language | GitHub-style? | Dependency cost | Notes |
|---|---|---|---|---|
| **grip** | Python | ✅ Pixel-identical (uses github.com API) | `pipx install grip` + needs network, API-rate-limited without a token | Best visual fidelity but depends on GitHub's API at render time. |
| **markserv** | Node | ~GitHub-ish (markdown-it + CSS) | ~29 direct deps incl. `handlebars`, `less`, `snyk`, `bluebird`, `is-online`, 8 markdown-it plugins; 500+ transitive packages | Great feature set (live reload) but heavyweight install. |
| **MkDocs + Material** | Python | Themed, not GitHub-style | `pipx install mkdocs-material` + config file + build step | Static-site generator; overkill for "point at folder and serve". |
| **Docsify** | Node | Themed | Node + CDN runtime | Requires client-side rendering + config. |
| **mdBook** | Rust | Themed | Single binary | Book-structured, requires `book.toml`. |
| **Caddy v2 + markdown handler** | Go | Custom | Caddy binary | Markdown module was dropped in v2 core; requires custom build. |
| **`github-markdown-css` + `python -m http.server`** | — | ✅ | Two pieces, no renderer | Serves raw markdown; browser doesn't render it. |

None of them fit all three of our requirements simultaneously: pixel-close to
GitHub, offline-capable, and a single zero-dep binary invokable via `npx`.
`grip` needs network + API tokens; `markserv` is heavy; static-site generators
need config and a build step.

## Decision

Build a tiny Go CLI — **`md-serve`** — and distribute it as a single static
binary fronted by an `npx` shim. Tech stack:

### Language: Go (stdlib + exactly one external dep)

**Why Go:**
- Produces a single static binary per OS/arch. No runtime on the user's
  machine (no Python, no Node, no libc if built with `CGO_ENABLED=0`).
- `net/http` + `http.FileServer` + `embed.FS` cover static files, path
  routing, and asset bundling out of the box — no framework needed.
- Cross-compilation is first-class: `GOOS=darwin GOARCH=arm64 go build …`
  works from any host. This is critical for shipping 6 platform binaries
  from CI (or a laptop).
- Binary is ~9 MB per platform, dwarfed by `node_modules` for an equivalent
  Node implementation.

**Why not Python:** `grip` already exists and we'd have the same "needs a
Python runtime" friction as `grip` itself.

**Why not Node:** we'd inherit a transitive dependency blast radius (see
markserv above) and the install wouldn't be noticeably smaller than a Go
binary.

**Why not Rust:** Go's `net/http` is enough; Rust buys nothing here and
lengthens build times.

### Markdown renderer: `github.com/yuin/goldmark`

Go's stdlib does not include a markdown parser, so we must take one
dependency. Options:

- **goldmark** — pure Go, CommonMark compliant, ships GFM extension (tables,
  strikethrough, task lists, autolinks). Used by Hugo and most modern Go
  projects. Actively maintained. Zero non-stdlib transitive deps.
- **gomarkdown/markdown** — pure Go, looser spec compliance than goldmark.
- **russross/blackfriday** — legacy; author recommends migrating to
  goldmark.

We picked **goldmark** for: CommonMark compliance, GFM out of the box, clean
extension API, and de-facto-standard status in the Go ecosystem. It is the
only external Go dependency we pull in, and it itself has no non-stdlib
transitive deps — so `go.sum` stays small and auditable.

### Styling: bundled `sindresorhus/github-markdown-css`

- Actual CSS extracted from github.com by the same upstream author who ships
  `github-markdown-css` as an npm package. MIT-licensed.
- We **embed** the CSS files (`github-markdown.css`, `github-markdown-light.css`,
  `github-markdown-dark.css`) into the binary via `//go:embed` and serve
  them under `/_md-serve-assets/`. No network round-trip at render time.
- Rendered markdown bodies are wrapped in `<article class="markdown-body">`
  as the stylesheet expects.

**Why not write our own CSS?** Reproducing GitHub's look by hand is a moving
target, and the authoritative CSS already exists under a permissive license.
Refreshing it is a single curl against upstream.

**Why not fetch from a CDN at runtime?** Defeats the "works offline, one
binary, no network assumptions" goal.

### Distribution: `npx` shim + per-platform optional dependencies

The single most important ergonomics decision is **how users invoke it**.
We picked the same pattern as the `agent-chat` project in this org:

```
npx md-serve            # zero-install, fetches only the matching platform binary
```

Mechanics:

1. The primary package `md-serve` (unscoped for `npx md-serve` ergonomics) is
   a ~3 kB package whose only content is a small Node shim at
   `bin/md-serve.js`. No runtime deps.
2. The shim detects `process.platform` + `process.arch`, then `require.resolve`s
   a platform-specific package like `@choonkeat/md-serve-linux-x64`, and
   spawns the native binary contained within. (Originally `spawnSync`;
   later switched to async `spawn` with signal forwarding so that
   SIGTERM/SIGINT propagate to the Go child instead of orphaning it.)
3. Those six platform packages (`darwin-{x64,arm64}`, `linux-{x64,arm64}`,
   `win32-{x64,arm64}`) are listed as `optionalDependencies` on the primary
   package. npm's `os`/`cpu` package-json fields ensure a given user only
   downloads the one binary matching their machine — the other five are
   skipped automatically.
4. Result: a user on `darwin-arm64` running `npx md-serve` downloads ~9 MB
   total and spawns a native Go binary. No Node code runs except a dozen
   lines of shim.

**Why npx at all, when Go alone is enough?**
- `npx md-serve .` is the lowest-friction one-liner we can offer. No
  `go install`, no PATH setup, no Homebrew tap, no Rust toolchain. npm is
  already installed anywhere Node is, which is most dev machines.
- Platform-specific optional dependencies are a well-trodden npm pattern
  (used by esbuild, turbo, swc, rollup, biome, etc.), not a hack.
- Users who prefer `go install github.com/choonkeat/md-serve@latest` can
  still do that — the Go source is the source of truth.

**Why unscoped primary package?** Users type the name. `npx md-serve` is
easier than `npx @choonkeat/md-serve`. The platform packages remain scoped
because nobody types them by hand.

### Default `$PORT` binding

The server's `-addr` flag defaults to `:$PORT` if the environment variable
is set, otherwise `:8080`. Rationale: swe-swe sessions, Heroku, Fly, Railway,
and most containerized dev environments assign a port via `$PORT`. Honoring
it by default means `md-serve -dir .` Just Works in those environments
without flag plumbing.

## Consequences

### Positive
- **One-liner invocation** via `npx md-serve`, no prior install.
- **Offline-capable**: after the first fetch, no network calls at render time.
- **Auditable dependency surface**: exactly one Go dep (`goldmark`), one npm
  shim (no deps), six per-platform packages with zero deps each.
- **Fast**: Go binary, no JS startup, no Python cold start.
- **GitHub-accurate styling** without relying on github.com's API.
- **Portable across dev environments** via `$PORT` default.

### Negative / Accepted tradeoffs
- We now **own a small codebase** instead of using a third-party tool. We
  must keep `goldmark` and `github-markdown-css` up to date ourselves.
- **~9 MB binary per platform** is larger than a Python script. Acceptable
  in exchange for zero runtime deps.
- **Six platform packages to publish** per release. Mitigated by the
  `make build-platforms` / `make publish` pipeline and version-lockstep
  guard in `scripts/publish.sh`.
- ~~**No live-reload** (markserv has this).~~ Live reload landed in v0.1
  (see commit `b781c1f`); it is on by default and uses an injected JS
  poller, not WebSocket. Pass `-no-live` to disable.
- ~~**No syntax highlighting** inside fenced code blocks.~~ Chroma syntax
  highlighting landed in v0.1 (see commit `8456629`). Fenced blocks and
  standalone source files are both highlighted using the `github` /
  `github-dark` chroma themes with `prefers-color-scheme` media queries
  (see ADR 0003).

### Follow-ups / explicit non-goals for v0.1
- ~~Syntax highlighting (chroma) — deferred.~~ Shipped in v0.1 (`8456629`).
- ~~Live reload — deferred.~~ Shipped in v0.1 (`b781c1f`).
- Dark mode — shipped in v0.2 (see ADR 0003).
- Authentication / access control — explicit non-goal; this is a dev tool.
- Multi-page navigation, search, sidebar TOCs — use MkDocs/mdBook if you
  need those.
- Custom themes — explicit non-goal; GitHub style is the whole point.
