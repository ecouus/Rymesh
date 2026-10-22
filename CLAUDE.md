# agent-console — project rules

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
  the node itself, is the limit;
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
