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

## Development
```bash
go test -race ./...
go run ./cmd/mctl-glovo   # serves /livez and /readyz on :8080
```

## License
Apache-2.0, see [LICENSE](LICENSE).
