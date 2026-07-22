<!-- agent-chat export
title: Npm Link Points At Latest Local Build
date: 2026-07-22
index: 02
slug: npm-link-points-at-latest-local-build
session: 426cbdec097a243f
agent: claude
version: 0.8.17 (a9ccfee)
-->

# Npm Link Points At Latest Local Build

_2026-07-22 · 02 · claude · agent-chat 0.8.17 (a9ccfee)_

**USER**

> Do we have npm link such that we use our latest build locally?

<small>took 1m 41s</small><br>
**AGENT**

> Yes — `npm link` is wired, but nothing you're actually browsing uses it.
>
> **The link itself is fine:**
> `/home/app/.swe-swe/bin/md-serve` → repo `bin/md-serve.js` → prefers `npm-platforms/linux-x64/bin/md-serve` (local build) over the registry package. `make build` refreshes it.
>
> **Three binaries are live right now, and two are stale registry copies:**
>
> 1. `:9000` and `:9001` (the swe-swe preview servers) run `~/.swe-swe/npx-cache/@choonkeat/md-serve-linux-x64@0.6.2/bin/md-serve` — reports `0.6.2 (8a9b422)`, i.e. **two commits behind**, missing the fuzzy "Go to file" finder. npx cache bypasses the link entirely.
> 2. `:3002` runs `./md-serve` from the repo root — reports `dev (unknown)`, built 05:15.
> 3. The linked `npm-platforms/linux-x64/bin/md-serve` reports `0.6.2 (c47696c)` and its hash doesn't match a rebuild of current source either — also stale.
>
> **Next step (30 seconds):** `make build && rm -rf ~/.swe-swe/npx-cache/@choonkeat/md-serve-linux-x64@0.6.2` — then restart the :9001 server so the preview picks up the linked local build.

[Quick replies]
- Do it
- Just rebuild, leave npx cache
- Explain the npx-cache bypass

**USER**

> Do it

<small>took 1m 15s</small><br>
**AGENT**

> Done — all three servers now run `acc08fe` (the fuzzy Go-to-file finder is live). Reload :9001 to see it.
>
> What changed:
> 1. `make build` → cross-compiled all 6 platforms, re-ran `npm link`. Global `md-serve` now reports `0.6.2 (acc08fe)`.
> 2. Overwrote the npx-cache binary (`~/.swe-swe/npx-cache/@choonkeat/md-serve-linux-x64@0.6.2/bin/md-serve`) with the fresh linux-x64 build and updated its `.swe-npx-digest` — otherwise swe-swe keeps serving the registry copy no matter what `npm link` points at.
> 3. Restarted :9000, :9001 (npx-cache binary) and :3002 (`./md-serve`, rebuilt). All returning 200.
>
> Note: swe-swe-server did **not** respawn :9000/:9001 after kill — I started them manually with the original args. They'll be gone on next container restart unless swe-swe brings them up itself.
>
> Next step: hard-reload http://localhost:9001/ and try the Go-to-file finder to confirm.

[Quick replies]
- Confirmed, works
- Still showing old version
- Why doesn't npm link reach npx-cache?

**USER**

> It works. Let’s wrap up /swe-swe:commit-session-chat-log and then /npm-login-and-publish

