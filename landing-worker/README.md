# glovo-landing (Cloudflare Worker)

Serves the landing page at `https://glovo.mctl.ai/` from the edge, so it
stays up when the origin (glovo-home on the owner's machine) is down.
Only the root path is routed here; `/mcp` and `/.well-known/*` keep going
through the tunnel to glovo-home.

- `src/pages.gen.js` is generated from the Go template in
  `internal/gateway/landing.go`: run `go run ./cmd/landing-gen` from the
  repository root after changing the page. `go test ./cmd/landing-gen`
  fails while the file is stale.
- `src/index.js` repeats glovo-home's language choice (`?lang`, then the
  `glovo_lang` cookie, then `Accept-Language`) and its headers.
- Tests: `node --test test/*.test.js` (Node 22, no dependencies).

Deploy: `.github/workflows/landing-worker.yml` runs `wrangler deploy` after a
merge to main, with the repository secrets `CLOUDFLARE_API_TOKEN` (an
account-owned token with Workers Scripts: Edit only, declared in
mctl-gitops `infrastructure/cloudflare/tokens/manifest.tf`) and
`CLOUDFLARE_ACCOUNT_ID`.

The route is not configured here. It lives in mctl-gitops
`infrastructure/cloudflare/zones/mctl-ai/workers.tf` (`glovo.mctl.ai/`);
`wrangler deploy` with `routes` would overwrite it. Rollback: delete the
route there and the origin serves the same page again.
