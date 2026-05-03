# ADR 0002 — Raw-by-default for source files; `?pretty=1` opts in to highlighting

- **Status:** Accepted
- **Date:** 2026-04-11
- **Supersedes:** v0.1 source-file behavior described in ADR 0001 follow-ups.
- **Implemented in:** [`5a7df17`](../../commit/5a7df17), building on [`8a1b43b`](../../commit/8a1b43b).

## Context

ADR 0001 listed syntax highlighting as a deferred follow-up for v0.1. It
landed shortly after, with this default: any file md-serve recognized as
source code (via `chroma`'s lexer registry) was rendered as a
syntax-highlighted HTML viewer. `?raw=1` was the opt-out for fetching the
byte-for-byte original.

That default made sense when md-serve was framed as a *source browser* —
a thing you point at a checkout to read code with linkable line numbers.
It made md-serve **unusable** the moment someone tried to use it as a
plain static-asset host.

### The reproduction that forced the decision

A sister project (`form-is-type`) tried to swap `python3 -m http.server`
for md-serve to serve a `./www/` tree containing a small SPA: a few
HTML pages plus ES module bundles (`engine.js`, `datatype.js`, etc.).

```html
<script type="module" src="datatype.js"></script>
```

Chromium issues this request and applies a *strict MIME type check* per
the HTML spec: a module script MUST be served with a JavaScript MIME
type (`text/javascript`, `application/javascript`, etc.). md-serve
served `datatype.js` as:

```
HTTP/1.1 200 OK
Content-Type: text/html; charset=utf-8

<!DOCTYPE html><html…><title>datatype.js — /datatype.js</title>…
```

…and Chromium killed every module script with:

> Failed to load module script: Expected a JavaScript-or-Wasm module
> script but the server responded with a MIME type of "text/html".
> Strict MIME type checking is enforced for module scripts per HTML spec.

Every demo app in the tree (`datatype-builder`, `case-editor`,
`view-builder`, `cases`, `seed`, `analytics`, ...) lost its JavaScript
and rendered as a static HTML shell with no interactivity. Same class
of failure would hit `.css` consumed via `<link>` (whose MIME check is
weaker but still wrong), `.wasm` instantiation (strict-MIME), and
anything `fetch()`ed and parsed as JSON (works but the response shows
up as HTML in DevTools, which is misleading at best).

A second, smaller blocker surfaced at the same time: at directory
roots, md-serve served `www/README.md` for `GET /` instead of
`www/index.html`. Every other static host on earth (nginx, Apache,
Caddy, GitHub Pages, every CDN) prefers `index.html` at directory
roots. md-serve was the outlier.

`8a1b43b` patched the most obvious symptom by serving `.html` / `.htm`
raw — but the design problem (source files are dressed up as HTML
viewers by default) was untouched, and the index.html-vs-README.md
ordering was untouched.

## Decision

Invert the default. Source files now serve **raw** via `http.ServeFile`,
with their normal MIME types. The highlighted-HTML viewer is opt-in via
a `?pretty=1` query string. At the same time, `index.html` wins over
markdown index files at directory roots.

Concretely:

1. **Raw-by-default for source files.** `serveFile` no longer
   auto-highlights. The `.md` / `.markdown` rendering branch is
   unchanged (those files are the whole point of md-serve and are
   *always* rendered to HTML). The `.html` / `.htm` raw passthrough
   from `8a1b43b` is unchanged. Everything else now goes straight to
   `http.ServeFile`.

2. **`?pretty=1` opts in to highlighting.** When the query string is
   present and the file is small enough (≤1 MiB), looks like text, and
   chroma can find a lexer for it, md-serve renders the same
   syntax-highlighted view it used to render by default. Otherwise it
   falls through to raw — `?pretty=1` on a binary or an unrecognized
   format is harmless.

3. **Directory listings link source files with `?pretty=1`.**
   `listingHTML` checks each entry against `lexers.Match(name)` (a
   filename-only lookup, so it stays O(1) per entry — no file reads at
   listing time). Source files get `href="name.ext?pretty=1"`; markdown,
   HTML, directories, and files chroma can't lex by name get the bare
   href. Net effect: a human browsing the listing still clicks into the
   highlighted view, but `<script src>`, `curl`, `wget`, and direct
   URLs all get the raw bytes.

4. **`index.html` wins over `README.md` at directory roots.**
   `serveDir` now checks `index.html` first; if absent, it falls back
   to the `index.md` / `README.md` / `readme.md` / `index.markdown`
   combined-page rendering; if neither exists, it generates the
   listing. README is still served when navigated to directly
   (`/README.md`).

5. **`?raw=1` is removed.** It was an opt-out for the old default. The
   new default *is* raw, so the parameter is meaningless. Removed
   cleanly with no deprecation alias — there is no installed
   bookmark base worth protecting against, and a no-op param invites
   confusion.

### Why filename-only lexer lookup in the listing

`pickLexer` (used at request time) tries `lexers.Match(name)` first
and falls back to `lexers.Analyse(content)` for files without a
recognizable extension. The listing builder deliberately skips the
content fallback: doing it would mean reading every file in every
directory just to compute hrefs, which is O(N) I/O on every listing
request. The trade-off: a content-detected file in a listing won't
get a `?pretty=1` link, so the user lands on raw and can manually
append `?pretty=1` if they want the highlighted view. Acceptable.

### Why pre-1.0 minor bump (0.1.0 → 0.2.0)

The default behavior change is breaking for anyone who scripted
against `?raw=1` or relied on the old README-wins ordering, and for
anyone whose bookmarks pointed at the auto-highlighted view of a
source file (the URL still works but returns different content).
Pre-1.0 semver convention puts breaking changes in the minor slot.
There is no installed user base large enough to warrant a major bump
or a deprecation cycle.

## Consequences

### Positive

- **md-serve is now a viable static-asset host.** ES module scripts,
  CSS, wasm, JSON-as-data, images, and fonts all reach the browser
  with their normal MIME types. The form-is-type SPA experiment now
  works end-to-end (verified via Playwright: `datatype-builder.html`
  renders its sidebar and "Pick a DataType…" prompt, which are both
  produced by JavaScript at runtime).
- **Honest default**: raw is what the file actually is. The pretty
  view is a presentation layer the human opts into.
- **Matches the rest of the static-host ecosystem.** `index.html`
  precedence and raw-MIME source files are what every nginx / Apache
  / Caddy / GitHub Pages user already expects.
- **No new code paths to maintain at request time.** The
  highlighting code is intact and reused; only the trigger flipped.
- **Listing UX preserved.** Humans browsing still get the highlighted
  view by default, because the listing carries the `?pretty=1`.

### Negative / Accepted tradeoffs

- **Breaking change for `?raw=1` users.** Removed without alias. We
  judge the installed base to be effectively zero given md-serve's
  age and distribution.
- **Stale bookmarks to highlighted source views still work** (the
  URL is the same, just without `?pretty=1`), but they now return
  raw bytes — a behavior change for anyone who bookmarked the
  highlighted view of a code file. They can re-bookmark with
  `?pretty=1`.
- **A directory with both `index.html` and `README.md`** (typical
  for a project root) now serves the index.html at `/`, hiding the
  README from the root URL. The README is still reachable at
  `/README.md`. This is a subjective UX call; it favors "host real
  apps" over "render docs at the root".
- **`mime.TypeByExtension` quirks leak through.** `http.ServeFile`
  uses the system mime database, which on some hosts maps `.mod`
  (Go modules) to `audio/x-mod` and similar oddities. The directory
  listing links these files with `?pretty=1` so the human-browsing
  case is unaffected; a `curl` user gets the wrong header but the
  right bytes. Not worth a whitelist.
- **Listing builder cannot detect content-based source files.**
  Files without a recognizable extension whose language chroma can
  only identify by content sniffing won't get `?pretty=1` in the
  listing. Workaround: append `?pretty=1` manually.

### Non-goals for this change

- **No CLI flag to flip the default back.** The whole point is the
  new default is correct; a flag to undo it is just lock-in to the
  old design.
- **No automatic content negotiation** (e.g. Accept-header sniffing
  to serve HTML to browsers and raw to curl). The query-string
  approach is explicit, predictable, cacheable, and bookmarkable.
- **No deprecation period for `?raw=1`.** Clean removal.
- **No special handling for `index.html` + `README.md`** beyond the
  precedence change. We do not, for example, render the README
  *into* index.html or merge them.

## Follow-ups

- None blocking. The design lands as a single self-contained change.
- If someone hits the "I want both index.html *and* the README rendered
  at /" use case, a `?index=listing` or similar opt-in could be added
  later — but no one has asked for it yet, so we're not building it.

## Update — symmetric `?pretty=` for rendered file types

Original wording above said `.md` / `.markdown` are "*always* rendered to
HTML." That's no longer true: `?pretty=0` now opts out for any extension
whose default is rendered.

The query-string semantics are now uniform across all file types, driven
by a single `prettyRegistry` map of `ext → { renderer, prettyByDefault }`:

- `.md` / `.markdown` / `.html` / `.htm` default to `prettyByDefault=true`.
  `?pretty=0` (or `false`, `no`, `off`) serves the source as
  `Content-Type: text/plain` with `X-Content-Type-Options: nosniff`.
- Everything else defaults to `prettyByDefault=false` and uses chroma as
  the implicit pretty renderer. `?pretty=1` opts in (with the same
  size/text-like/lexer-found viability gates as before).
- The query string is parsed with `strconv.ParseBool`. Invalid or empty
  values fall back to the extension's default rather than being treated
  as truthy. This is a small breaking change for callers who relied on
  the prior `Get("pretty") != ""` check — `?pretty=0` and similar
  falsy-looking values *used* to trigger highlighting. They no longer do.

Raw mode for `prettyByDefault=true` extensions uses `http.ServeContent`
rather than `http.ServeFile` because the latter 301-redirects requests
ending in `/index.html` to the directory form, which would drop the
forced `text/plain` headers and re-enter the directory's `index.html`
rendering branch.

## Update — `Accept` header gates pretty rendering

The original "Non-goals" section above ruled out content negotiation:

> No automatic content negotiation (e.g. Accept-header sniffing to serve
> HTML to browsers and raw to curl). The query-string approach is
> explicit, predictable, cacheable, and bookmarkable.

That stance was correct as the *only* mechanism — but it left a gap once
markdown and `.html` defaulted to pretty rendering. An API consumer
fetching `/README.md` with `Accept: application/json`, or `curl -H
'Accept: text/plain'`, got an HTML wrapper they then had to strip. The
query-string opt-out (`?pretty=0`) requires the caller to know about it
and modify every URL they touch, which doesn't compose with bookmarks
or hand-typed paths.

The Accept header is the standard way clients express what they can
render. Honoring it costs nothing, breaks nothing, and is what every
other content-aware server does:

- If `pretty` would otherwise be true (extension default, or explicit
  `?pretty=1`) **and** the request's `Accept` header doesn't include
  `text/html`, `application/xhtml+xml`, `text/*`, or `*/*`, force
  `pretty=false`. Equivalent to `?pretty=0` for that one request.
- Empty/missing Accept means "anything" (RFC 9110); we treat it as
  HTML-accepting.
- The Accept check applies *after* the query-string check, so it
  overrides explicit `?pretty=1`. Rationale: a client that says it
  can't render HTML probably set the query string by accident (or
  inherited it from a referrer); shipping HTML anyway is worse than
  ignoring the hint.

What stays the same: query-string semantics are unchanged for clients
that *do* accept HTML. Browsers, `curl` with no `-H`, `wget`, and
default `fetch()` all send `Accept: */*` or a list including
`text/html`, so they hit the same code paths as before. The only
behavior shift is for clients explicitly opting out of HTML — and for
those, raw bytes is what they were trying to ask for.

This reverses the "no Accept sniffing" non-goal. The original argument
(query-string is "explicit, predictable, cacheable, bookmarkable") is
still true for the query string itself; the Accept gate is purely
additive for the API/curl-with-Accept use case the query string can't
serve cleanly.

Caching note: responses now vary by Accept for `prettyByDefault=true`
extensions. We don't currently set `Vary: Accept` — the typical
deployment is dev/local serving with no shared cache, and adding the
header would force CDN re-fetches for the common case where Accept
doesn't actually change the body. If md-serve grows a production-cache
deployment story, revisit.
