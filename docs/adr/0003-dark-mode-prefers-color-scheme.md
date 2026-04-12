# ADR 0003 — Dark mode via `prefers-color-scheme`

- **Status:** Accepted
- **Date:** 2026-04-12
- **Builds on:** ADR 0001 (chroma syntax highlighting).

## Context

md-serve already shipped with `github-markdown-css`, which includes both
light and dark mode styles gated on `prefers-color-scheme`. But two pieces
were frozen in light mode:

1. **Syntax highlighting (chroma).** The chroma formatter was configured
   with `WithClasses(false)` — it emitted inline `style` attributes baked
   to the `github` (light) palette. No amount of CSS media queries can
   override inline styles, so code blocks stayed light even when the
   browser was in dark mode.

2. **Page chrome.** The `<body>` background, the README/source label
   between the directory listing and rendered content, and the
   page-width widget all used hard-coded light-mode colours. In dark
   mode the body was white, the label text was invisible against a dark
   article, and the width widget looked jarring.

The net effect: users with `prefers-color-scheme: dark` saw a
half-dark page — the prose and tables respected dark mode (thanks to
`github-markdown.css`), but code blocks were blinding white rectangles
and the surrounding page chrome was wrong.

## Decision

Switch chroma to class-based output and generate a dual-theme CSS block
at init time so every rendered page respects `prefers-color-scheme`
end-to-end.

### Chroma: inline styles -> CSS classes

```go
var chromaFormatter = chromahtml.New(
    chromahtml.WithClasses(true),   // was: false
    ...
)
```

With `WithClasses(true)`, chroma emits class names (`.kd`, `.s`, `.c`,
etc.) instead of inline `style="color:..."` attributes. The actual
colours are supplied by a `<style>` block that the formatter can
generate via `WriteCSS`.

### Dual-theme CSS generation

At init time, `chromaCSS` is computed once:

1. Generate the `github` (light) theme CSS via
   `chromaFormatter.WriteCSS(&buf, chromaLightStyle)`.
2. Generate the `github-dark` (dark) theme CSS via
   `chromaFormatter.WriteCSS(&buf, chromaDarkStyle)`.
3. Wrap the dark block in `@media (prefers-color-scheme: dark) { ... }`.
4. Scope every rule under `.markdown-body` (rewriting `.chroma` to
   `.markdown-body .chroma`) so chroma's background-color beats
   `github-markdown.css`'s `.markdown-body pre` rule without needing
   `!important`.

The resulting CSS is injected as `<style>{{.ChromaCSS}}</style>` in the
page template, before the main `<style>` block.

### Why scope under `.markdown-body`?

`github-markdown.css` sets a background on `.markdown-body pre`. Without
scoping, chroma's `.chroma` selector has equal specificity and loses in
some browsers (source order depends on which `<style>` block comes
first). `.markdown-body .chroma` always wins without `!important`.

### Page chrome fixes

- `:root { color-scheme: light dark; }` — tells the browser the page
  supports both schemes, so scrollbars, form controls, and other
  UA-rendered chrome adapt.
- `body { background: #0d1117; }` in the dark media query — matches
  `github-markdown.css`'s dark article background so the 45px body
  padding doesn't create a tonal seam.
- `.markdown-body p.md-serve-readme-source { color: #8b949e; }` in the
  dark media query — the "README" / "source" label was `#57606a`, which
  is invisible on a dark background.

### Goldmark fenced-block alignment

The goldmark-highlighting extension was also switched to
`WithClasses(true)` so that fenced code blocks inside markdown files
use the same class-based approach and pick up the dual-theme CSS.

## Consequences

### Positive

- **Full dark mode support.** Every element on the page — prose, tables,
  code blocks, line numbers, directory listings, page-width widget, body
  background — now tracks `prefers-color-scheme` without any user action.
- **Zero runtime cost.** The CSS is computed once at startup and inlined
  into every page. No JavaScript, no cookie, no toggle button.
- **No new dependencies.** Uses chroma's built-in `github` and
  `github-dark` styles, which are already bundled in the binary.

### Negative / Accepted tradeoffs

- **Slightly larger HTML.** Each page now carries ~4 KB of chroma CSS
  (light + dark rules). Acceptable for a dev tool; the CSS compresses
  well with gzip.
- **No manual toggle.** The page follows `prefers-color-scheme` only.
  Users who want light-mode code on a dark-mode OS (or vice versa) must
  change their OS/browser setting. A JS toggle is an explicit non-goal —
  it adds complexity and state for a dev tool where OS preference is the
  right default.
- **`chromaFormatter.Format` still takes a single style.** The light
  style is passed as the "base" style to `Format()`, but since
  `WithClasses(true)` is set, the style argument only matters for
  fallback tokens not covered by the CSS. In practice all tokens are
  covered, so this is a no-op distinction.

### Non-goals

- Manual light/dark toggle button.
- Per-file or per-directory theme overrides.
- User-selectable chroma themes (always `github` / `github-dark`).
