# CLAUDE.md

Out-of-process CLIProxyAPI plugin (`anthropic-quota-reserve`) that reserves a
percentage of Anthropic's real 5h/7d subscription quota for specific auth IDs.

## Build & deploy

**Never build or copy a `.so` to production by hand.** On 2026-10-16 a build cut
from a stale checkout silently dropped `scheduler_across_priorities`, and every
Claude request 429'd once the borrowed top-tier account hit its reserve. Deploy
only via the infrastructure repo's `scripts/cheesepad-cpa-quota-plugin-deploy.sh`
(clean + pushed commit only, builds `git archive` in `golang:1.26-bookworm`,
stamps `version=0.1.0+<sha>`, verifies, auto-rollback). Commit AND push before
deploying; `git fetch` first — this repo has been pushed from more than one place.

Verify compile only (no output kept):

```bash
cd go && go test ./... && go build -buildmode=c-shared -o /tmp/anthropic-quota-reserve.so . && rm /tmp/anthropic-quota-reserve.so
```

`TestRegistrationOptsIntoAcrossPriorities` guards the flag — don't delete it.

## Architecture

Single file, `go/main.go`. C-ABI shim (`cliproxy_plugin_init`, `cliproxyPluginCall`
method dispatch) around two capabilities:

- `usage.handle` (`handleUsage`) — parses `anthropic-ratelimit-unified-{5h,7d}-{utilization,status,reset}`
  response headers per `UsageRecord.AuthID`, keeps latest snapshot in the
  in-memory `state` map.
- `scheduler.pick` (`pickAuth`) — for auth IDs listed in config only, excludes
  a candidate once tracked utilization crosses `1 - reserve_percent/100`,
  unless its Anthropic-reported `reset` timestamp has passed. Registers
  `scheduler_across_priorities`, so candidates span every priority tier (host
  must include commit `b715526a`, 2026-09-17; older hosts ignore the flag and
  only offer the top tier, which brings back the stuck-account problem). When
  no candidate is blocked it returns `Handled:false` so host priority, strategy
  and session affinity apply untouched. When something is blocked it picks from
  the highest remaining priority value (larger = preferred, same as the host),
  keeps each session (by session header) on the account it chose, and
  round-robins new sessions. If every candidate ends up excluded, soft mode
  returns `Handled:false`; with `hard_block_when_all_reserved: true`, it returns
  HTTP 429 so the host cannot fall back to a protected account.

State is flushed to `state_file` on a ticker (`flush_interval_seconds`) and on
`plugin.shutdown`, and reloaded on `plugin.register`/`plugin.reconfigure` —
this is what makes tracked utilization (and thus blocks) survive restarts.

`Auth.ID` (the config's `auth_id`, matched 1:1 against both `UsageRecord.AuthID`
and `SchedulerAuthCandidate.ID` in the host) is populated differently by auth
source: OAuth JSON credential files use their filename
(e.g. `some-account@gmail.com.json`), config-declared API-key auths use a
synthesized 12-hex-char ID. Always confirm the real value via the management
API auth-listing endpoint or by reading `state_file` after the account has
made at least one request — do not assume the ID format.

See README.md "Known limitation" section for the current unbounded-block edge
case (`reset` never captured → permanent block, no time-based fallback yet).

## Conventions

- Keep this as a single-file plugin; don't split into a package layout unless
  it grows a second capability that needs real isolation.
- No comments explaining *what* code does — only *why*, and only when
  non-obvious (see existing comments in `pickAuth`/`handleUsage` for the bar).
- Never commit real `auth_id`, API keys, or account emails into README/config
  examples — placeholders only (e.g. `<Auth.ID of the borrowed account>`).
- This repo tracks `github.com/router-for-me/CLIProxyAPI/v7` as a Go module
  dependency (see `go/go.mod`) for `sdk/pluginabi` / `sdk/pluginapi` — bump it
  when the host's plugin ABI changes, and rebuild/retest against the new
  version before deploying.
