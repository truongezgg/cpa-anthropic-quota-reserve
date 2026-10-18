# Anthropic Quota Reserve Plugin

Reserves a percentage of Anthropic's real 5h/7d subscription quota for specific
auth IDs (e.g. an account borrowed from someone else) so the proxy stops
selecting that account once it gets close to being drained.

It implements:

- `plugin.register`
- `plugin.reconfigure`
- `plugin.shutdown`
- `scheduler.pick`
- `usage.handle`

## How it works

Every Claude response carries Anthropic's own `anthropic-ratelimit-unified-5h-utilization`
and `anthropic-ratelimit-unified-7d-utilization` headers (0.0–1.0 fraction of quota used,
alongside matching `-status` and
`-reset` headers). CLIProxyAPI already forwards these headers unfiltered to
every `UsagePlugin` via `UsageRecord.ResponseHeaders`; this plugin reads them
in `usage.handle`, keyed by `UsageRecord.AuthID`, and keeps the latest snapshot
per account in memory.

In `scheduler.pick`, only auth IDs listed in `accounts` are subject to this
logic — every other candidate is left completely untouched, so your own
unlimited accounts are unaffected. A configured account is excluded from
selection once its tracked 5h or 7d utilization crosses `1 - reserve_percent/100`.
If every candidate ends up excluded (e.g. all configured accounts are over
budget and there's no unconfigured fallback), the default soft mode returns
`handled: false` so the host's normal scheduler still picks something. Set
`hard_block_when_all_reserved: true` to fail the request with HTTP 429 instead,
ensuring a protected account is never selected below its reserve.

The plugin asks the host for candidates across **all** priority tiers
(`scheduler_across_priorities`, supported by hosts built after 2026-09-17), so
a protected account can sit at the top priority without getting stuck: once it
is reserved, requests move to the highest remaining priority tier, and move
back automatically after Anthropic's reset time. While no account is reserved
the plugin does not intervene, so the host's own session affinity keeps prompt
caches warm. While an account is reserved the plugin keeps each client session
(`X-Claude-Code-Session-Id` and similar headers) on one account for up to 12h
and round-robins new sessions within the chosen tier.

This is a **soft budget** based on Anthropic's own self-reported utilization,
not a hard rate limiter — it only reacts after utilization has been observed
at least once (a newly added, never-used account is always eligible until its
first response is recorded).

## Known limitation: unbounded block if `reset` is never captured

Once an account is blocked it receives zero further traffic, so its tracked
state can only self-heal by comparing `now` against the `reset` timestamp
parsed from Anthropic's `-reset` header (see `pickAuth` in `go/main.go`). If
that header is ever missing or fails to parse for a window at the moment the
account trips over threshold, `Reset5h`/`Reset7d` stays `0`, and the account
is blocked **permanently** — including across restarts, since state is
persisted to `state_file`. There is currently no time-based fallback.

Mitigation today: if an account seems stuck, check `state_file` for the
account's entry and its `reset_5h`/`reset_7d` fields; a `0` value with a
high `util_5h`/`util_7d` confirms this case. Manual recovery is to edit or
remove that entry from `state_file` (or delete the file) and restart.

## Configuration

Add the plugin under `plugins.configs`:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    anthropic-quota-reserve:
      enabled: true
      priority: 1
      state_file: "./plugins/anthropic-quota-reserve-state.json"
      flush_interval_seconds: 300
      hard_block_when_all_reserved: true
      accounts:
        - auth_id: "<Auth.ID of the borrowed account>"
          match: exact
          reserve_5h_percent: 20
          reserve_7d_percent: 20
```

Fields:

- `state_file`: local JSON file the plugin periodically flushes tracked
  utilization to, and reloads on startup, so state survives restarts.
- `flush_interval_seconds`: flush period; defaults to `300` (5 minutes) if unset or `<= 0`.
- `hard_block_when_all_reserved`: when `true`, return HTTP 429 instead of
  delegating to the built-in scheduler if every candidate is below reserve.
  Defaults to `false` for backward compatibility.
- `accounts`: list of `auth_id` / `match` / `reserve_5h_percent` /
  `reserve_7d_percent` entries. `match` defaults to `exact`; set it to
  `contains` for a stable email fragment such as `borrowed@example.com` when
  the host adds changing prefixes or suffixes to OAuth auth IDs. Contains
  matching is case-insensitive, exact matching remains case-sensitive, and an
  exact rule wins over a contains rule. Accounts not matched here are never
  touched by this plugin.

## Build & deploy

Production (the Cheesepad CLIProxyAPI) is deployed **only** through the
infrastructure repo's `scripts/cheesepad-cpa-quota-plugin-deploy.sh`. It
refuses a dirty tree or an unpushed commit, builds `git archive <commit>` in
`golang:1.26-bookworm` on the box (the CPA image is Debian bookworm), stamps
the commit into the plugin version (`version=0.1.0+<sha>` in the host's
`plugin registered` log), runs the tests, checks the `.so` still declares
`scheduler_across_priorities`, swaps it atomically, restarts `cli-proxy-api`
and rolls back if the plugin does not re-register with that commit.

Local compile check only:

```bash
cd go && go test ./... && go build -buildmode=c-shared -o /tmp/anthropic-quota-reserve.so . && rm /tmp/anthropic-quota-reserve.so
```
