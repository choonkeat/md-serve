# md-serve

Tiny static file server that renders Markdown (`.md`, `.markdown`) as
GitHub-styled HTML. Single Go binary, distributed via npm with native
binaries for Linux / macOS / Windows on x64 and arm64.

## Install

```sh
npm install -g md-serve
# or one-shot:
npx md-serve
```

## Usage

```sh
md-serve                  # serve $PWD on :$PORT (or :8080), live-reload on
md-serve -dir ./docs      # serve a specific directory
md-serve -addr :3000      # bind to a specific address
md-serve -no-live         # disable the live-reload poller
md-serve -version
```

## Behavior

- `.md` / `.markdown` files render as HTML using
  [goldmark](https://github.com/yuin/goldmark) with GFM, styled with
  [github-markdown-css](https://github.com/sindresorhus/github-markdown-css).
  Fenced code blocks are syntax-highlighted via
  [chroma](https://github.com/alecthomas/chroma) (200+ languages).
- Directories show a generated listing with **Name / Size / Modified**
  columns. If `index.md` / `README.md` / `readme.md` / `index.markdown`
  is present, it's rendered below the listing GitHub-style. If only
  `index.html` is present, it's served raw.
- Source files (`.go`, `.py`, `.json`, `.yaml`, `.toml`, `Dockerfile`,
  `Makefile`, ...) render as syntax-highlighted HTML with linkable line
  numbers (`/main.go#L42`). Append `?raw=1` to any URL to bypass and
  fetch the byte-for-byte original. Files larger than 1 MiB or that
  look binary skip highlighting and stream raw.
- Everything else is served byte-for-byte.
- Dotfiles are hidden from listings.
- Path traversal is blocked: requests are rejected if they resolve
  outside the served root.
- Live-reload is on by default: rendered pages poll a tiny endpoint
  once a second and reload themselves when the underlying file's mtime
  changes. Pass `-no-live` to disable (e.g. for production-style
  serving where you don't want the extra requests).

## Develop

```sh
make build              # cross-compile all 6 platform binaries → npm-platforms/
make test               # go vet
make publish-dry        # rehearse the npm publish
make publish            # ship to npm
make bump VERSION=x.y.z # sync version across package.json + optionalDependencies
```

The npm package layout: a thin shim (`bin/md-serve.js`) selects the
right `@choonkeat/md-serve-<platform>-<arch>` optionalDependency at
runtime, falling back to a locally built binary in `npm-platforms/`
during development.

## License

MIT
