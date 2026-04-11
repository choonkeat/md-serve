---
description: Rebuild md-serve and (re)start it on the current working directory
argument-hint: [extra md-serve flags]
allowed-tools: Bash, KillShell
---

Rebuild `md-serve` from source and (re)launch it on the current working directory, killing any prior instance first. Always rebuild — the user wants every `/run` to reflect the latest source.

Steps:

1. **Capture the working directory.** It is `$PWD` at invocation time. Do **not** `cd` anywhere — serve from wherever the user ran `/run`.

2. **Find the project root** so `make` runs in the right place even if `$PWD` is a subdirectory of the repo:

   ```sh
   REPO_ROOT=$(git -C "$PWD" rev-parse --show-toplevel)
   ```

3. **Free the listen port.** Whatever process is currently bound to `:$PORT` (default `:8080` if `PORT` is unset) gets killed — that's what `/run` is about to bind to. Surgical by port, not by process name, so it won't touch unrelated `md-serve` instances on other ports.

   ```sh
   PIDS=$(lsof -ti tcp:"${PORT:-8080}" 2>/dev/null)
   if [ -n "$PIDS" ]; then kill $PIDS 2>/dev/null || true; sleep 0.2; fi
   ```

   (`lsof -t` prints PIDs only; the empty-string guard works on both BSD and GNU `xargs`/`kill`. The brief sleep gives the kernel a moment to release the port before we re-bind.)

   If you also still hold a background shell ID from an earlier `/run` in this same Claude session, call `KillShell` on it too so you don't leave orphaned task records around.

4. **Rebuild.** Run `make -C "$REPO_ROOT" build` in the foreground. This cross-compiles all 6 platform binaries via `scripts/build-platforms.sh` and refreshes the embedded `-X main.commit=` ldflag with the current `git rev-parse --short HEAD`. If the build fails, surface the error to the user and stop — do not attempt to launch a stale binary.

5. **Launch in the background** via the node shim, forwarding any extra flags and enabling `-live` so the user gets auto-reload while editing. Use the Bash tool with `run_in_background: true`:

   ```sh
   node "$REPO_ROOT/bin/md-serve.js" -dir "$PWD" -live $ARGUMENTS
   ```

   (`$ARGUMENTS` is forwarded *after* `-live`, so the user can override with `-live=false` if they need to.)

   The shim auto-resolves to the freshly built binary in `npm-platforms/<plat>-<arch>/bin/md-serve`, falling back to the npm-installed `@choonkeat/md-serve-<plat>-<arch>` package.

6. **Read the background output** once to grab the listen address line (e.g. `serving … on http://:3000`). The default address is `:$PORT` if set, else `:8080`, unless the user passed `-addr` in `$ARGUMENTS`.

7. **Report back** in ~4 lines: the directory being served, the URL (e.g. `http://localhost:3000`), the version banner (`md-serve <ver> (<commit>)`), and the new background shell ID so the user can stop it later with `KillShell`.
