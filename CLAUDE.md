# mctl-glovo

Glovo Basket MCP server for `https://glovo.mctl.ai/mcp`. It builds a basket
from a user's requests and asks the owner to approve it. It never places
orders, never pays and never calls the real Glovo API.

## Boundary with mctl-api
mctl-api stays the authority for identity and decisions:
- OAuth authorization server and JWKS (`https://api.mctl.ai`);
- team grants, team membership and the Glovo pilot allowlist;
- the approval ledger and the approval page, where the authenticated owner
  approves or rejects. This service never asserts a human decision.

This service owns the MCP surface (the six basket tools, RFC 9728 metadata,
the 401 challenge, the audience check) and the basket data.

## Rules
- Fail closed. A dependency that could not be read (JWKS, the mctl-api
  resolve call, the database) is an error answered with 503 or a refusal,
  never an empty result, "not found" or a pass. Errors are never cached.
- A token is accepted only if `aud` is exactly the Glovo resource, `iss` is
  mctl-api and the signature verifies against mctl-api's JWKS (EdDSA only).
  Local verification is a pre-filter; authorization comes from mctl-api.
- No secrets in the repository. Configuration arrives through the
  environment, from Vault via ExternalSecrets in mctl-gitops.

## Conventions
- Go, standard library first. `go.mod` pins Go 1.26.11; if the local brew
  toolchain is broken, use `GOTOOLCHAIN=go1.26.11`.
- Conventional commits; every commit carries an `Agent:` trailer
  (`claude`, `codex`, `agy` or `none`).
- English for code, comments, docs and everything on GitHub. No emoji.
- Branch + PR for every change; merge commits only.
- Tags and images use semver without a `v` prefix:
  `ghcr.io/mctlhq/mctl-glovo:1.2.0`.

## Testing
```bash
go build ./...
go test -race ./...
golangci-lint run
```
