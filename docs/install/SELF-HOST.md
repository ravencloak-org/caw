# Self-host caw — operator install guide

This is the operator-side companion to `CLAUDE.md` / `CURSOR.md` / `CODEX-CLI.md`
(which cover the agent-side MCP wiring). If you run the hub yourself instead of
using `caw.ravencloak.org`, start here.

There are two GitHub App registration paths:

1. **Manifest flow** (recommended, default). The hub generates the manifest,
   GitHub creates the App, and the hub auto-persists `client_id`,
   `client_secret`, `webhook_secret`, and the App private key. Setup URL and
   "Request user authorization (OAuth) during installation" come pre-flipped.
2. **Hand-registered** App. You created the GitHub App yourself via
   *Settings → Developer settings → GitHub Apps → New GitHub App*. You own the
   settings page and are responsible for flipping the right toggles.

Most ops gaps in production traced back to the hand-registered path. PR #53
landed the self-service install callback that mints Watcher tokens on the spot,
but a hand-registered App with the wrong toggles will land users on a bare-text
400 from the callback. Phase 0 of the Auth v2 plan made those error pages
actionable; this doc keeps them from firing in the first place.

---

## 1. Bring up the hub

```sh
git clone https://github.com/ravencloak-org/caw.git
cd caw
cp .env.example .env   # fill in CAW_GH_WEBHOOK_SECRET + OpenObserve creds
docker compose up -d
```

Hub listens on `:8080`. Put it behind a reverse proxy with TLS — the install
callback page sets `Cache-Control: no-store` and `Referrer-Policy: no-referrer`
to keep tokens out of intermediate caches, and those guarantees only hold on
HTTPS.

The two env vars that gate the rest of this doc:

- `CAW_BASE_URL` — the publicly reachable URL of your hub, e.g.
  `https://caw.example.com`. Required for the manifest flow and the install
  callback. Empty → both routes are disabled with a startup log line.
- `CAW_BOOTSTRAP_TOKEN` — an operator secret you generate once
  (`openssl rand -hex 32`). Required for the manifest flow only. Without it
  the hub logs `warning: CAW_BASE_URL set but CAW_BOOTSTRAP_TOKEN empty;
  GitHub App manifest flow disabled` and serves a 404 on `/github/app/manifest`.

Add both to your compose env (alongside the secrets you set in step 1) and
restart:

```sh
echo "CAW_BASE_URL=https://caw.example.com" >> .env
echo "CAW_BOOTSTRAP_TOKEN=$(openssl rand -hex 32)" >> .env
docker compose up -d hub
```

---

## 2. Register the GitHub App — path A: manifest flow (recommended)

Visit `https://caw.example.com/github/app/manifest?token=<your CAW_BOOTSTRAP_TOKEN>`.

You'll see a one-button form that POSTs a pre-encoded GitHub App manifest to
`github.com/organizations/<your-org>/settings/apps/new` (or
`github.com/settings/apps/new` for a personal account). The hub-generated
manifest sets these for you, so you don't have to:

| Setting                                                | Value                                       |
| ------------------------------------------------------ | ------------------------------------------- |
| Name                                                   | `Caw`                                       |
| Webhook URL                                            | `https://caw.example.com/webhooks/github`   |
| Setup URL                                              | `https://caw.example.com/github/app/install/callback` |
| **Request user authorization (OAuth) during installation** | ✓ (`request_oauth_on_install: true`)   |
| Redirect on update                                     | off (`setup_on_update: false`)              |
| Permissions: Pull requests                             | Write                                       |
| Permissions: Checks                                    | Read                                        |
| Permissions: Contents                                  | Write (for orphan-rebase force-push, ADR-0002) |
| Subscribed events                                      | check_suite, pull_request, pull_request_review, pull_request_review_comment, issue_comment, installation, installation_repositories |

GitHub redirects back to `/github/app/callback?code=…&state=…`. The hub
exchanges the code for the App's permanent credentials and persists
`client_id`, `client_secret`, `webhook_secret`, and the RSA private key PEM
into `caw.db`'s `app_credentials` table. The bootstrap token is now spent —
the manifest route refuses to re-register over an existing App unless the
operator sets `ALLOW_REBOOTSTRAP=1`.

You can now skip to step 3.

---

## 2. Register the GitHub App — path B: hand-registered (legacy)

Skip this section if you used the manifest flow. This is for operators who
already have a hand-rolled GitHub App (the production `caw-ravencloak` App is
the canonical example) or who prefer to register manually.

In *Settings → Developer settings → GitHub Apps → New GitHub App*, flip the
following — **the bolded two are the ones whose absence produces the
404-bare-text-callback failure mode that Phase 0 of Auth v2 made visible**:

- **Setup URL** = `https://caw.example.com/github/app/install/callback`.
  *(Image alt-text: a text input labeled "Setup URL (optional)" containing the
  URL above, with the "Redirect on update" checkbox below it.)*
  Without this, GitHub never redirects newly-installed users anywhere — they
  land on a generic "Installed" page on github.com with no token, no
  `CAW_WATCHER_TOKEN` value, and no clue what to do next.
- **Request user authorization (OAuth) during installation** = ✓ (checked).
  *(Image alt-text: a labeled checkbox under the "Identifying and authorizing
  users" section, with the helper text "If checked, users will be prompted to
  authorize your GitHub App during installation".)*
  Without this, GitHub redirects to the Setup URL but omits the `code` query
  parameter, and the hub renders a 400 page with code `missing_oauth_code`
  telling the user (and you) exactly which checkbox is wrong.
- Redirect on update = off (matches the manifest default). Optional. When on,
  reconfiguring the installation (e.g. adding a repo) re-fires the Setup URL
  with `setup_action=update`; the hub renders a soft-redirect page pointing
  at `/me/tokens` (Phase 4 of Auth v2 wires the actual route) instead of
  re-minting a token.
- Webhook URL = `https://caw.example.com/webhooks/github`.
- Webhook secret = the value of `CAW_GH_WEBHOOK_SECRET` from your `.env`.
- Permissions: pull_requests=Write, checks=Read, contents=Write.
- Events: check_suite, pull_request, pull_request_review,
  pull_request_review_comment, issue_comment, installation,
  installation_repositories.

After GitHub generates the App, *Generate a private key* and download the PEM
file. On the App's general page, copy the App ID, Client ID, and *Generate a
new client secret*. Wire all five into the hub via env:

```sh
echo "CAW_APP_ID=123456" >> .env
echo "CAW_APP_PRIVATE_KEY_PATH=/data/caw-app.pem" >> .env
echo "CAW_APP_CLIENT_ID=Iv1.xxxxxxxxxxxxxxxx" >> .env
echo "CAW_APP_CLIENT_SECRET=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" >> .env
# Webhook secret already in .env as CAW_GH_WEBHOOK_SECRET.
docker cp ./caw-app.pem $(docker compose ps -q hub):/data/caw-app.pem
docker compose up -d hub
```

The install callback's `credsFn` checks env first, store second, so this
co-exists fine with a future manifest-flow registration.

---

## 3. Install the App on a repo

Send your developers (or yourself) to
`https://github.com/apps/<your-app-slug>/installations/new`. They pick the org
and the repos to install on, GitHub redirects to your Setup URL, and the hub:

1. Validates `installation_id`, `setup_action=install`, and `code` are all
   present (or renders an actionable error page — see "When the install
   callback fails" below).
2. Exchanges the OAuth `code` for a user-to-server token.
3. Calls `GET /user/installations` to confirm the authenticated user is in
   fact an admin of `installation_id`.
4. Calls `MintFn(installation_id, org)` — same code path as
   `hub mint-token` — and renders the token in a one-shot HTML page with
   copy-to-clipboard plus pre-baked Claude Desktop / Cursor / Codex CLI
   snippets.

`Cache-Control: no-store`, `Referrer-Policy: no-referrer`, an aggressive CSP,
and `X-Content-Type-Options: nosniff` all hold for that page.

---

## 4. When the install callback fails

Every failure path renders `install_error.html` (Phase 0 of Auth v2): an
HTML page with a short "What happened" explanation, 1-3 actionable "What to
do" bullets, a *Restart login* button (which lands on
`<hub>/auth/start-help` — Phase 3 of Auth v2 wires the actual handler;
until then the button 404s but the on-page bullets carry the real guidance),
and a stable error code rendered as `<code>missing_oauth_code</code>`-style
badge in the sub-header. Match the code to its fix:

| Code                          | Status | What to flip                                                                |
| ----------------------------- | ------ | --------------------------------------------------------------------------- |
| `missing_installation_id`     | 400    | GitHub redirected without `installation_id`. Usually a stale browser tab.   |
| `missing_oauth_code`          | 400    | Flip *Request user authorization (OAuth) during installation* on the App.   |
| `unsupported_setup_action`    | 400    | Hand-crafted URL; not a real flow.                                          |
| `no_credentials`              | 424    | Set `CAW_APP_CLIENT_ID` + `CAW_APP_CLIENT_SECRET` or run the manifest flow. |
| `creds_lookup_failed`         | 500    | DB read error. Check hub logs.                                              |
| `oauth_exchange_failed`       | 502    | Code expired or reused. Restart from the agent.                             |
| `installations_lookup_failed` | 502    | Transient GitHub outage. Retry in 30s.                                      |
| `not_an_admin`                | 403    | User isn't an admin of this installation. Sign in as the right account.    |
| `mint_failed`                 | 500    | DB write error. Check hub logs.                                             |

The `setup_action=update` redirect — fired only when *Redirect on update* is
on for a hand-registered App and the installation is reconfigured — renders a
200 soft-redirect page pointing at `/me/tokens`. That route ships in Phase 4
of Auth v2; until then the button 404s and the page tells self-hosters to
rotate via `hub mint-token`.

---

## 5. Smoke test

The hub-side analogue of the agent-side smoke test in `CLAUDE.md`:

```sh
# 1. Confirm the install callback is reachable.
curl -fsS "https://caw.example.com/github/app/install/callback?installation_id=0&setup_action=install" \
  | grep -F 'Request user authorization (OAuth)' \
  && echo "OK: install callback renders the actionable error page."

# 2. Confirm the manifest route is gated.
curl -fsS -o /dev/null -w '%{http_code}\n' \
  "https://caw.example.com/github/app/manifest" \
  | grep -q '^4' \
  && echo "OK: manifest route refuses unauthenticated callers."
```

If both print `OK`, the hub is install-ready. Hand a developer the install
link from step 3 and watch a token land in their agent.

---

## 6. First end-user login (post-Auth-v2 cutover)

With Phase 5 shipped, end-users no longer paste tokens. The flow on a fresh
self-host, from the operator running `docker compose up` through to a
developer's first `subscribe_pr`, is:

```sh
# Operator side — one-shot, then leave the hub running.
git clone https://github.com/ravencloak-org/caw.git
cd caw
export CAW_BASE_URL=https://caw.example.com
export CAW_BOOTSTRAP_TOKEN=$(openssl rand -hex 32)
docker compose up -d
make hooks                                  # set core.hooksPath = .githooks
# Browser-only manifest registration; bootstrap token never leaves the
# terminal. The hub auto-persists App credentials.
open "https://caw.example.com/github/app/manifest?token=$CAW_BOOTSTRAP_TOKEN"
# Confirm the App settings page has these toggles flipped (the manifest
# pre-flips them, but check after re-registration):
#   ✓ Active webhook
#   ✓ Request user authorization (OAuth) during installation
#   Setup URL = https://caw.example.com/github/app/install/callback
```

```sh
# Developer side — install caw-watcher per docs/install/{CLAUDE,CURSOR,CODEX-CLI}.md
# then in the agent:
login()
# Browser opens to https://caw.example.com/auth/u/<sid>. Authorize the App,
# pick installation(s). The hub mints user-bound tokens and delivers them
# to ~/.config/caw/credentials.json over a one-shot loopback POST.

# Verify isolation: a co-worker who is a collaborator on `private-thing` but
# NOT on `secret-thing` (both in the same installation) gets:
subscribe_pr("ravencloak-org", "private-thing", 1)   # 200 + SSE
subscribe_pr("ravencloak-org", "secret-thing",  1)   # 404
```

### Operator break-glass commands

```sh
# Revoke a single token by id (Phase 4):
docker compose exec hub /hub revoke-token <token_id>

# Cutover migration (Phase 5): revoke every active legacy (NULL
# github_user_id) token row. Idempotent — re-running on a clean DB prints
# "Revoked 0 legacy tokens".
docker compose exec hub /hub migrate-tokens --dry-run   # preview first
docker compose exec hub /hub migrate-tokens             # then commit

# Last-resort: extend the migration window by one more release. With this
# env set, RequireRepoAccess still bypasses legacy tokens with a
# `Deprecation: legacy-token` header. Remove it once all watchers re-login.
docker compose run -e CAW_ALLOW_LEGACY_TOKENS=1 -d hub
```

## 7. Breaking change — Auth v2 Phase 5 cutover

Phase 5 flips `RequireRepoAccess` from "bypass legacy tokens with a
`Deprecation` header" to "reject legacy tokens with `400 user-bound token
required; run \`login\` from your agent`" on `/sse/...` and `/leases/...`.

Any v0.1.x watcher still presenting a `hub mint-token`-issued token will
start failing on its next subscribe. The migration window is gated by the
`CAW_ALLOW_LEGACY_TOKENS=1` env flag:

1. **Before deploying the cutover release:** set `CAW_ALLOW_LEGACY_TOKENS=1`
   on the hub. Watchers continue to work; the hub logs every legacy hit so
   you can quantify the migration tail.
2. **Run the migration:** `docker compose exec hub /hub migrate-tokens
   --dry-run` to preview, then drop `--dry-run` to revoke. Each affected
   token's `installation_id` + `org` is printed for audit.
3. **Tell your developers to re-login:** they invoke the `login` MCP tool
   from inside their agent (see `MCP-LOGIN.md`). It works one device at a
   time; old devices keep their (now revoked) credentials but get the same
   400 directing them to log in.
4. **Unset `CAW_ALLOW_LEGACY_TOKENS`** once the legacy tail is gone. The
   hub starts rejecting any remaining legacy tokens with the same 400 the
   middleware uses for every other un-bound token.

The schema additions from Phase 1 (the nullable user / device / expiry
columns on `tokens`) stay backward-compatible — rollback is just unsetting
`CAW_ALLOW_LEGACY_TOKENS` (or rolling the hub image back one tag).

---

## See also

- [ADR-0003](../adr/0003-sse-auth-via-hub-minted-installation-token.md) — token shape and per-installation auth model.
- [ADR-0010](../adr/0010-self-service-watcher-tokens.md) — the install
  callback design that this doc operationalizes.
- `CLAUDE.md`, `CURSOR.md`, `CODEX-CLI.md` — agent-side wiring.
