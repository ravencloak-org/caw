# ADR-0010 — Self-service Watcher token issuance

| Field   | Value                                                |
|---------|------------------------------------------------------|
| Status  | Accepted                                             |
| Date    | 2026-06-18                                           |
| Extends | ADR-0003 (Hub-minted installation token for SSE auth) |

## Context

ADR-0003 established the Watcher token shape: a 32-byte random value minted by
the Hub, stored only as a SHA-256 hash, presented by the Watcher as
`Authorization: Bearer …` against the SSE and pending routes. The token is
scoped to a GitHub App installation.

ADR-0003 did **not** specify an issuance channel. The implementation shipped a
single channel: the `hub mint-token <installation_id> [org]` CLI subcommand,
run on the host where the Hub's SQLite DB lives. That works for the operator,
and it is still the right tool for automation. It fails for everyone else:

- A user who installs the public Hub's GitHub App from `github.com/apps/…` has
  nowhere to obtain a token. `README.md` Self-host step 3 and
  `docs/install/*.md`'s "Get a Hub token" section both punt to
  *"ask Jobin until self-service ships."* That is the wall the present ADR
  removes.
- A self-hoster who wants to onboard a non-admin teammate must either SSH the
  teammate into the Hub host, or paste a token over a side channel. Neither is
  a real product story.

The blocker is structural: the token mint function was only ever exposed via
`os.Args` parsing in `cmd/hub/main.go`. The HTTP surface had no path to it
that was both authenticated (so we know *who* is asking) and safe to display a
plaintext token through (so we don't leak it via Referer, browser history, or
server logs).

## Decision

We add a **self-service issuance channel** that piggybacks on GitHub's App
install Setup URL plus user-to-server OAuth, so the user proves they own the
installation before we mint:

```
GitHub  ──user clicks Install──▶  https://github.com/apps/caw/installations/new
                                                │
                                  user picks repos, completes OAuth consent
                                                │
                                                ▼
Hub    ◀──GitHub redirect──  /github/app/install/callback
                              ?installation_id=N&setup_action=install&code=C
       │
       │ 1. POST github.com/login/oauth/access_token  (code → user token)
       │ 2. GET  api.github.com/user/installations    (user owns N?)
       │ 3. mintFn(N, account_login)                  (raw token + hash to DB)
       ▼
Browser ◀────────  one-time HTML page rendering the raw token  ────────
```

The handler lives in `internal/hub/install_callback.go`. The same `mintFn`
the CLI uses is reused; the token shape and storage are unchanged from
ADR-0003.

### Why OAuth user-to-server (not bootstrap secret)

The manifest flow (`/github/app/manifest`, `/github/app/callback` — ADR's
predecessors in `internal/hub/manifest_handler.go`) is gated by
`CAW_BOOTSTRAP_TOKEN` — an operator-held secret. That is correct for
provisioning the App credentials themselves, where there is only one
authorized actor and the bootstrap token is a one-shot. It is wrong for token
issuance, where the authorized actor is *the user who installs the App* —
typed any time, by many different people.

`GET /user/installations` returns only the installations the OAuth-presenting
user has admin access to. The Hub asserts that `installation_id` from the
GitHub redirect is in that list before minting. This makes the user's GitHub
session the source of authority — exactly what the user already proved by
completing the install flow.

### Why one-time display (not server-stored plaintext)

GitHub's own personal access tokens use the same pattern: the raw token is
displayed exactly once at creation time; refresh loses it; lost tokens can
only be replaced, not recovered. We adopt the same shape:

- The Hub stores only the SHA-256 hash (existing ADR-0003 invariant).
- The raw token is in the response body, never persisted, never logged.
- Refresh fails because GitHub OAuth codes are single-use — the second
  exchange of the same `code` returns `bad_verification_code`.
- "Lost your token?" path: re-install the App, or `hub mint-token` if you
  self-host.

### Response headers

The response sets `Cache-Control: no-store`, `Referrer-Policy: no-referrer`,
`X-Content-Type-Options: nosniff`, and a Content-Security-Policy locking the
page down to inline style/script (required for the embedded one-page
template) plus `default-src 'self'`. No external assets, no analytics, no
fonts from CDNs.

### Manifest changes

`NewManifestHandler` now emits a manifest with:

- `setup_url` → `${CAW_BASE_URL}/github/app/install/callback`
- `setup_on_update` → `false`
- `request_oauth_on_install` → `true`

Apps minted from this point forward auto-redirect to the install callback and
include the OAuth `code` automatically. Apps that predate this ADR need the
operator to set the Setup URL and the OAuth-on-install checkbox manually in
the App's GitHub settings.

## Alternatives considered

| Alternative                                            | Why not                                                                |
|--------------------------------------------------------|------------------------------------------------------------------------|
| Separate authenticated REST route `POST /tokens`       | Still needs a bootstrap secret per user — same UX wall as today        |
| GitHub Action that mints tokens                        | Couples token lifecycle to a CI run; awkward for non-CI Watchers       |
| SaaS-only web dashboard                                | Contradicts ADR-0001 (portable single-binary self-host)                |
| Keep status quo, write better docs about `mint-token`  | Doesn't solve the public-Hub case; "ask Jobin" remains the only path   |

## Consequences

- One new HTTP route: `GET /github/app/install/callback`. Unauthenticated by
  design (GitHub's redirect is the authn signal); the OAuth exchange is the
  authz signal.
- No new external dependency. OAuth client_id/secret already live in the Hub
  DB from the manifest flow.
- Same token shape as ADR-0003. SSE auth, hash storage, and verification path
  unchanged.
- Existing Apps need a one-time Setup URL update in GitHub settings. New Apps
  get it from the manifest.
- The `hub mint-token` CLI stays — both for automation and as the recovery
  channel when the install flow can't be repeated easily.

## Follow-ups (out of scope for this ADR)

- A token-rotation route (re-OAuth, mint replacement, optionally revoke prior).
- Per-repo token scoping (today's tokens are installation-wide).
- A "lost token?" recovery flow that doesn't require re-installing the App.
