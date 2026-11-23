# mctl-glovo

Glovo Basket MCP server for **https://glovo.mctl.ai/mcp**.

Connect it to an MCP client (Claude, Claude Code, ChatGPT and others), sign
in with MCTL, and ask it to put together a Glovo basket. The basket is sent
to you for approval. Nothing is ordered and nothing is paid: the service only
prepares the basket.

## Status
Bootstrap. The service currently runs inside
[mctl-api](https://github.com/mctlhq/mctl-api) and is being moved here.
Migration phases:

1. mctl-api exposes a resolve and approvals API for this service (dark).
2. This repository gets the MCP server, token verification and the
   mctl-api client.
3. mctl-gitops deploys it without a public route.
4. The glovo.mctl.ai host moves here; rollback is reverting that change.
5. Basket data moves to its own database.
6. The in-process copy is removed from mctl-api.

## glovo-home (owner-only gateway)
`cmd/glovo-home` is a separate, owner-only gateway for running
`glovo.mctl.ai/mcp` from the owner's own machine. It sits in front of the
open-source local Glovo MCP server
[glovo-skill](https://github.com/denya/glovo-skill) and adds token
verification against mctl-api, a principal allowlist, a checkout/payment
tool refusal and an audit log. See
[deploy/mac-mini/README.md](deploy/mac-mini/README.md).

## Account link flow (C2, off by default)
`cmd/mctl-glovo` serves the account-link flow of
[docs/design/token-store.md](docs/design/token-store.md) 4.1 only when
`GLOVO_C2_ENABLED=true`. Off (the default), no `/link` route exists, and no
store, Vault login or database connection is built. Any other value is a
startup error. On, it needs `GLOVO_DATABASE_URL`, `GLOVO_VAULT_ADDR`
(in-cluster only), `GLOVO_EGRESS_PROXY`, an access gate (below), and
optionally `GLOVO_VAULT_ROLE` (default `mctl-glovo`) and
`GLOVO_LINK_TRUSTED_PROXIES` (CIDRs whose `X-Forwarded-For` is believed).

### Read-only by default
Hosted C2 does not change a Glovo basket. There is no switch for it:

- `glovo_add_to_basket`, `glovo_set_quantity` and `glovo_remove_from_basket`
  are not in `tools/list`, and a `tools/call` for one by name is refused
  before the tool lookup, the argument check, the token store and any
  request to Glovo;
- every hosted Glovo client refuses a basket mutation (POST, PUT, PATCH,
  DELETE or any other non-GET method on a `baskets` path) before the network,
  also on a 301/302/303/307/308 redirect hop, on HTTP/1.1 and HTTP/2, and on
  a retry; a redirect off the API origin is refused too;
- reading stays as it is: `glovo_get_basket`, purchase history, order items,
  order stats, `glovo_analyze_order_history`, reorder previews and plans;
- the text the tools serve (descriptions, the shopping guide, a reorder
  plan's `next_step`, the MCP instructions) points at no tool that is not
  listed, and the consent text (`c2-link-v2`) claims only reading.

No checkout, payment or order tool exists, and the client's mutation
allowlist has no such endpoint.

`GLOVO_C2_KEEPALIVE_INTERVAL` turns on the hosted session keepalive. Unset or
`0` (the default) runs none. Otherwise it is a Go duration from `1h` to `24h`
(for example `24h`); anything else stops the boot. Once per interval and
active link the service makes one authenticated basket read through the same
client, refresh lock and audit path as a tool call, so a linked account nobody
calls does not silently lose its refresh chain. It stops under the kill
switch, its decrypts are budgeted apart from the decrypt caps, and a refused
chain is logged as `event=keepalive outcome=needs_relink`. It assumes one
replica for the read schedule (`docs/design/token-store.md` 4.5). Enabling it
in production is a gitops change that needs the owner's go-ahead.

`GLOVO_C2_BASKET_WRITES` is only there to be refused: unset or `false` is
the one working value, and `true` (like any other spelling, `TRUE`, ` true`,
`1`, `yes`) stops the process at startup. Until an approval gate exists
(mctl-api#1011 and #1012), an environment variable must not be able to open
basket writes on the user-facing hosted C2. The seam for that future change
is in code: `c2Config.basketWrites` and `c2Config.basketApprover`, which no
loader sets, and `mcpDeps.BasketApprover`, without which `newMCPRoutes`
refuses to serve the write tools; every write tool call then asks the
approver first (`approvalMiddleware`). The enabling change adds the approval
step and sets those fields in the same reviewed change.

The session-file backend of `glovo-home` (the owner's own machine) is not
covered: it serves the three tools, and `cmd/glovo-home` is a separate
owner-only service. Whether a pilot participant can reach it is a question of
its principal allowlist and of what the `glovo.mctl.ai` route points at; see
the note on mctl-glovo#49.

### Who may use C2
Every token must be an mctl-api token for this resource that names exactly
one grant: a team grant (`tgr` + `stn`) or a personal grant (`tgr` +
`tgk=personal`, no `stn`). Who is admitted then depends on whether
mctl-api's consent route is configured (`GLOVO_CONSENT_API_URL` and
`GLOVO_CONSENT_TOKEN_FILE`, set together):

| Consent route | `GLOVO_C2_ALLOWED_PRINCIPALS` | Admitted |
|---|---|---|
| set | unset | any principal with an active `glovo:c2` consent |
| set | set | an active consent **and** on the list (staged pilot) |
| unset | set | the list alone; no code can be issued (no consent source) |
| unset | unset | startup error |

The consent is read through `internal/consentgate`: a positive answer is
cached for at most 60 s (30 s by default), a "no" and a failed read are never
cached, and a failed read refuses (503), never passes. Withdrawing the
consent in mctl-api therefore takes effect here within a minute. The same
gate re-checks the consent on `/link/upload`, after the code is redeemed and
before the session is validated or stored; `/link/code` (which takes the same
fresh link-audience token as preview and upload, not an MCP token) reads the consent
uncached. Any future MCP route that acts with a stored session must check
the same gate on every call.

`GLOVO_C2_REQUIRE_ACCESS` adds the owner's approval to that gate
(`docs/design/self-service-onboarding.md` 5.2, stage B). Unset or `false`
(the default) changes nothing: an active consent admits, and the `access`
field of mctl-api's consent answer is not looked at. `true` admits a
principal only when the same answer carries an active consent to the current
text **and** `access` is exactly `approved`. Any other value (`TRUE`,
` true`, `1`, `yes`) stops the process at startup, and so does `true`
without the consent route, which is where the access state comes from. With
the flag on:

| mctl-api answer | Result |
|---|---|
| consent active, `access: approved` | admitted; the only answer that is cached |
| consent active, `access` is `none`, `pending`, `rejected` or `revoked` | refused, 403 (`principal_not_allowed` on `/mcp`, `access_denied` on the link routes, "access refused" on a tool call) |
| consent active, `access` absent, empty or a value this service does not know | refused, 503: the state was not observed, so it is neither approved nor a decision |
| no consent, whatever `access` says | refused, 403 `not_admitted` / `consent_required`, as without the flag |
| mctl-api unreachable, non-200 or a malformed body | refused, 503, as without the flag |

The flag is carried by the one consent source that the token verifier gate,
the per-call admitter (`admit.go`, 5 s) and the link routes all read, so none
of them can admit on consent alone, and the cache times above are unchanged:
a positive answer is cached only when both hold, and no refusal or failed
read is cached. Revoking an approval in mctl-api therefore stops tool calls
within 5 s and new requests within a minute. mctl-api serves the `access`
field from 4.176.0, and only while its access store is wired.

Switch it on in this order, never the reverse:

1. mctl-api `GLOVO_ACCESS_REQUIRED=true`. Its backfill approves every
   principal who already holds an active consent, the owner included.
2. Check that the consent read of a known principal answers
   `access: approved`.
3. `GLOVO_C2_REQUIRE_ACCESS=true` here (a gitops change that needs the
   owner's go-ahead).

Turning this flag on first refuses everyone, the owner included, until
mctl-api has approved them; setting it back to `false` restores the consent
gate.

Until mctl-api serves masked identities (C2-1c), no preview answers, even
with the flag on.

`cmd/glovo-link` is the local helper. It does the whole flow with one fresh
MCTL login: it requests its own one-time code, shows the masked MCTL identity
the code belongs to, asks y/N, and uploads `~/.glovo/session.json` (or
`-session -` for stdin: the y/N answer line, then the session JSON) over
https. There is no code to paste; the code stays in memory and is never
printed. One link uses three attempts of the per-IP and global rate limits
(code, preview, upload); the helper never retries a refused request. Its
session file is checked before the login. The helper and the service must be
released server first: a new helper against an old server is refused at the
code step.

## Image and schema migration
`Dockerfile` builds one image with three binaries: `mctl-glovo` (default
entrypoint), `glovo-migrate` and `token-refresher` (see "Token refresher" below). Releases are cut by release-please (tag
without `v`); the image is built and published centrally by mctl-gitops'
`build-image.yaml` as `ghcr.io/mctlhq/mctl-glovo:<semver>`. The dispatch to
mctl-gitops is off until the repository variable `GLOVO_IMAGE_DISPATCH` is
`true`.

`glovo-migrate` applies the token store schema as the migration owner role.
It reads the DSN from `GLOVO_MIGRATE_DATABASE_URL` (sslmode=require or
stricter), checks the connected role (`-expect-role`, default
`glovo_c2_migrator`), is safe to rerun (an up-to-date schema is left alone,
the kill switch included; a partial or different schema is refused) and with
`-runtime-role` also applies the runtime grants. Exit 0 ok, 1 refused, 3
could not observe or failed.

`internal/identmask` holds `Email` (the masked identity for the link
preview, `a***@e***.com`) and `Pseudonym` (a keyed stable stand-in for a
principal id in logs).

## Development
```bash
go test -race ./...
go run ./cmd/mctl-glovo   # serves /livez and /readyz on :8080
```

## Token refresher
`token-refresher` is the sidecar command (override the entrypoint) that keeps
a ZITADEL machine-user access token in `TOKEN_FILE`, for a consumer that
re-reads the file per call. Environment: `ZITADEL_ISSUER` (https),
`ZITADEL_USER_ID`, `ZITADEL_KEY_ID`, `ZITADEL_SCOPE`, `KEY_FILE` (the private
key PEM, or the JSON key document ZITADEL emits), `TOKEN_FILE` (absolute,
its directory must be writable); optional `TOKEN_FILE_MODE` (octal, default
0644) and `READY_FILE` (exists while a valid token is on disk, for an exec
probe). It signs an RS256 JWT-profile assertion, exchanges it at
`{issuer}/oauth/v2/token`, writes the token atomically and renews at half of
`expires_in`. Failure policy is fail closed: a failed refresh keeps the old
token only until its own expiry, then the file is removed and the process
exits 1 (the container restarts); a leftover file is removed on start. Exit 2
is a refused configuration. No redirects, no proxy, response size bounded,
nothing secret logged.
Expiry is counted from before the request was sent, minus a 5 s safety skew
(at most a quarter of the lifetime), so the file never outlives the real
validity. Renewal is at half of `expires_in` (floor 5 s, or half the lifetime
when that is shorter than 10 s), so a short-lived token is renewed before it
expires.

## License
Apache-2.0, see [LICENSE](LICENSE).
