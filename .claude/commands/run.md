---
description: Run md-serve on the current working directory
argument-hint: [extra md-serve flags]
allowed-tools: Bash
---

Start the locally-built `md-serve` HTTP server on the current working directory and leave it running in the background.

Steps:

1. Capture the current directory: it is the value of `$PWD` when this command is invoked. Do **not** `cd` anywhere — serve from wherever the user ran `/run`.
2. Launch the server in the background using the Bash tool with `run_in_background: true`. Use the project's node shim so this works whether or not `md-serve` is on `$PATH`:

   ```
   node /repos/static-files-and-markdown-server/workspace/bin/md-serve.js -dir "$PWD" $ARGUMENTS
   ```

   The shim auto-resolves to the locally built binary in `npm-platforms/<plat>-<arch>/bin/md-serve` (built by `make build`), falling back to the npm-installed `@choonkeat/md-serve-<plat>-<arch>` package.

3. Briefly read the background process output to grab the listen address (default `:8080`, or `:$PORT` if set, or whatever `-addr` the user passed in `$ARGUMENTS`).

4. Report back to the user with: the directory being served, the URL (e.g. `http://localhost:8080`), and the background shell ID so they can stop it later with `KillShell`. Keep the report to 3 lines.

If the build artifacts are missing (the shim errors with "Could not find package"), tell the user to run `make build` first instead of trying to fix it yourself.
