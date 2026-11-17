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
before the session is validated or stored; `/link/code` reads the consent
uncached. Any future MCP route that acts with a stored session must check
the same gate on every call.

Until mctl-api serves masked identities (C2-1c), no preview answers, even
with the flag on.

`cmd/glovo-link` is the local helper. It reads the code from stdin, shows
the masked MCTL identity, asks y/N, and uploads `~/.glovo/session.json`
(or `-session -` for stdin) over https.

## Image and schema migration
`Dockerfile` builds one image with three binaries: `mctl-glovo` (default
entrypoint), `glovo-migrate` and `token-refresher`. Releases are cut by release-please (tag
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

## License
Apache-2.0, see [LICENSE](LICENSE).

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
