# Rymesh — project rules

## Core principle (non-negotiable)

This program is **only a remote control**: a page that shows what the agents
do and passes the user's instructions to them. **Every model request is sent
by the official CLI (`claude`, `codex`) running on the machine the user
selected, with that machine's own login.**

It must never become a model-API reverse proxy. Concretely, no change may:

- call Anthropic / OpenAI (or any model provider) APIs from the console, hub,
  web page, Mac app or Android app — not even "just" for a model list, usage,
  titles or summaries;
- forward, relay or re-expose model API traffic, or let a client send model
  requests through another machine's credentials;
- read, copy or transmit CLI credentials (`~/.claude`, `~/.codex/auth.json`,
  API-key environment variables) — checking only whether a login exists, on
  the node itself, is the limit. This binds the program, not the user: what
  the user opens by hand in the terminal or the file browser is theirs to
  see and download, as over plain SSH, so those features do not single out
  credential files. The program itself still never reads, uploads or uses
  them;
- add features meant for letting other people use someone's account
  (multi-user sharing of one node's CLI login is account sharing).

What is allowed: starting the CLI on the node and speaking its local stdio
protocol (`claude -p --input-format stream-json`, `codex app-server`);
relaying **this console's own API** between the user's own devices (hub →
node over SSH or a private network); asking the CLI itself for information
(e.g. `codex app-server` `model/list`, Claude's own slash-command replies).

When a feature needs data from the provider, get it through the CLI on the
node. If a request cannot be done that way, say so instead of working around
the rule. `TestNoDirectModelAPI` (principle_test.go) fails the build if
provider API hosts or credential variables appear in the source.

## Working notes

- Build: `make build` (CLI), `make dist` (all platforms), `make macos`,
  `make android`. Tests: `make test` (`go vet` + `go test -race`).
- Real-CLI end-to-end checks run against an isolated instance (own config,
  data dir and port); clean up the sessions they create in `~/.claude` and
  `~/.codex` afterwards.
- After every change, once `make test` passes, update every client so none
  runs a stale build: `make macos` and replace `/Applications/Rymesh.app`
  with `macos/dist.noindex/Rymesh.app` (quit and reopen the app), and
  `make android` then `adb install -r
  android/app/build/outputs/apk/release/app-release.apk` on the connected phone
  (relaunch `dev.rymesh.app`). Then upgrade the remote server nodes too, the
  same way the app does it: the Mac console's `POST /api/devices/{id}/deploy`
  uploads the new Linux build over SSH and runs `rymesh node setup`, which
  upgrades in place and keeps the node's token. Confirm it took: the node's
  `/api/health` `build` must equal the first 12 hex digits of the SHA-256 of
  the matching `dist/rymesh_linux_<arch>` (the `version` string alone does not
  change between uncommitted builds).
