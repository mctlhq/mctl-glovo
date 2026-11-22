# C2 operations runbook (Draft)

Status: DRAFT. Offline documentation for issue #46. Nothing in this document
was executed, deployed, measured or sent. It grants no right to run any
production action.

Source baseline: `origin/main` at `aa9b3cc39cc7a8bbc834f04dfe841868a3ec65d6`
(the merge of #54). Every CURRENT statement below cites a path at that SHA.
The paths are repository-relative. Two baselines came earlier:
`ca4a09aaf6b9b99f106177aee206726ff6b004b0` and
`11009fd61586ddb67b92a8e2e44d32d2dfa2774c` (the merge of #58).

Two merged changes since `11009fd` are reflected here. #53 merged at
`167f326fb` (order-intelligence tests and `internal/glovologic` statistics).
#54 merged at `aa9b3cc39` (hosted C2 is read-only by default). The diff
`11009fd..aa9b3cc` touches, among other files, `cmd/mctl-glovo/c2.go`,
`cmd/mctl-glovo/mcp.go`, `internal/glovoapi/{client,hosted}.go`,
`internal/glovotools/{backend,handlers}.go`, `internal/glovologic/{stats,suggestions}.go`,
`internal/linkflow/consent.go` and `README.md`. The files under
`internal/tokenstore`, `internal/authn`, `internal/gateway`,
`internal/health`, `internal/consentgate` and `cmd/mctl-glovo/{main,control}.go`
did not change in non-test source. Every CURRENT citation of a changed file
was re-read at `aa9b3cc` and its line numbers were updated; where this
revision corrects a claim, it says so. The `internal/glovologic` changes
(statistics and reorder suggestions) affect only what read tools return and
no cited statement here.

Merged is not deployed. That #53 and #54 are merged is a CURRENT fact about
the repository at the baseline. Whether any image built from them is
running, and whether the hosted route is served by this service at all, is
UNKNOWN (section 2, section 12).

Related work, referenced and not duplicated:

- mctlhq/mctl-glovo#40 (open issue): records readiness requirements
  and planning evidence for G0-G4 and the stage sequence. Its merged
  artifacts are `docs/design/c2-readiness.md` and
  `docs/design/c2-readiness.json`, cited by path at
  `11009fd61586ddb67b92a8e2e44d32d2dfa2774c`. A merged planning artifact
  gives no live GO and authorizes no stage or drill. This document does not
  copy gate status and does not change those files. The correction to that
  matrix, #58, merged at `11009fd61586ddb67b92a8e2e44d32d2dfa2774c`.
- mctlhq/mctl-glovo#41 and PR #54 (merged at `aa9b3cc39`): hosted C2 is
  read-only by default. The merged repository behavior is described in
  sections 1.3 and 3.2 from the source at the baseline. That the repository
  contains it is not evidence that a build with it is deployed: the running
  image and its configuration are UNKNOWN. #54 authorizes no live smoke test
  and no drill.
- mctlhq/mctl-glovo#45 and PR #53 (merged at `167f326fb`): order-intelligence
  tests and statistics for the read tools. It changes what read tools
  return. It does not change any operation in this runbook.
- mctlhq/mctl-glovo#44: unresolved security items. They stay open here.
- mctlhq/mctl-gitops#2777
  (https://github.com/mctlhq/mctl-gitops/issues/2777): cutover. Out of scope.
  It is a different repository from the Glovo issues in this list.
- mctlhq/mctl-glovo#46: this slice. This document does **not** close #46's
  dashboard, alert or live-recovery acceptance.

Bare `#N` below means a mctlhq/mctl-glovo issue or PR. Cutover is always
written `mctlhq/mctl-gitops#2777`.

## 0. How to read this document

Every statement carries one of these labels.

| Label | Meaning |
|---|---|
| CURRENT | Observed in the source at the baseline SHA, with a path. Says what the code does, not what is deployed. |
| DESIGN | Stated in `docs/design/*.md` as intent. The code may or may not implement it. |
| PROPOSED | A suggestion from this document. No operator decision exists. |
| UNAVAILABLE | The operation is not provided by this repository (no route, no command, no caller). |
| UNKNOWN | Runtime or external state not observed by this task. |

Nothing here describes deployed state. The cluster manifests, ExternalSecrets,
Vault policies, the mctl-api consent and control clients, dashboards and
alert rules live outside this repository and were not read. Whatever they
contain is UNKNOWN.

Limits that bind every section:

- Scope per the operator-recorded stage sequence (see #40): read-only for up
  to 10 users, and only approval plus a separately authorized canary for 20.
  Up to 10 read-only users is the planned target. It is not proved hosted
  baseline behavior. #54 and #58 are merged, and a merge is not a deployment
  or a GO: neither authorizes a live smoke test or a drill. Basket writes
  are not part of the read-only stages. This document changes neither limit.
- Current operator decision: every action with production effects,
  including a kill, needs a prior explicit owner GO. "Operator decision"
  here means the owner's recorded decision, not the operator role of
  section 2.1. Synthetic exercises (section 11) are different only once the
  owner has recorded an isolated environment for them: a record that names
  an environment with its own Vault, its own egress proxy and its own
  database, none of which serves production. Until that record exists, an
  exercise is a production action and needs a GO under section 2.2. This
  overrides any historical emergency wording in the sources cited below. This document confers no extra
  authority on the orchestrator or on a reviewer.
- No checkout, payment or order-placement tool exists or may be added
  (`CLAUDE.md`; `internal/glovotools/nocheckout_test.go` is the existing
  guard, not run here).
- Live `glovo-home` on the owner's mini is outside this task. Its README is
  cited only where the hosted service reuses the same mini and proxy.
- No script, detector, test or alert is added here. Any such item is
  PROPOSED and unbuilt.

## 1. Readiness is not proven by `/readyz`

### 1.1 Wiring trace (CURRENT)

- `internal/health/health.go`: `health.New(timeout, checks...)` runs each
  check on `GET /readyz`. With no checks it answers `200 ok`. `/livez` always
  answers `200 ok`. Neither reads a dependency by itself.
- `cmd/mctl-glovo/main.go:74`: `health.New(2*time.Second, app.ready...)`.
- `cmd/mctl-glovo/c2.go:218-221`: with `GLOVO_C2_ENABLED` unset or `false`,
  `buildLink` returns an `c2App` whose `ready` is nil. `/readyz` then has no
  checks and answers `200 ok` while no store, Vault login, database or
  proxy exists, and no `/link` or `/mcp` route is mounted.
- `cmd/mctl-glovo/c2.go:339`: with the flag on, `ready` is exactly
  `[]health.Checker{verifier.Ready}`. `verifier.Ready`
  (`internal/authn/verifier.go:174`) passes when a usable JWKS key set is
  cached, with one throttled refresh attempt. The cache is unusable after 24
  hours without a successful fetch (`internal/authn/jwks.go:26`).
- Not in readiness (CURRENT): Postgres, Vault, the egress proxy, the kill
  switch row, the mctl-api consent route, the control endpoint's own JWKS.

### 1.2 What follows

| Question | Answer | Label |
|---|---|---|
| Does HTTP 200 on `/livez` prove anything? | Only that the process answers. | CURRENT |
| Does HTTP 200 on `/readyz` prove C2 works? | No. With the flag off it is vacuous. With the flag on it proves only a cached JWKS. | CURRENT |
| Is Postgres loss visible in `/readyz`? | No. It surfaces per call (section 3.2). Postgres unreadable at process start exits the process: `tokenstore.NewPostgres` (`internal/tokenstore/postgres.go:27-38`) fails and `run` returns the error (`cmd/mctl-glovo/main.go:66-69`). | CURRENT |
| Is the deployed flag value `true`? | Not observed. | UNKNOWN |
| Is the deployed readiness probe `/readyz`, and what else gates traffic? | Not observed. | UNKNOWN |
| Is the C2 wiring in the running image the wiring above? | The image tag running is not observed. | UNKNOWN |

Rule for this runbook: an HTTP 200 from `/livez` or `/readyz` is never
operational proof. Operational proof is a named positive signal listed in the
"Evidence after" column of the action tables (section 10), such as a
`tool_call` line with `outcome="ok"` for a synthetic principal, plus the
absence of the failure signal in the same window. A missing log source is
`UNKNOWN`, not a pass.

PROPOSED (unbuilt, needs its own owner-approved slice): readiness checks for
the database and the kill switch row, so that `/readyz` stops hiding them.
This document does not make that change and does not assume it is made.

### 1.3 Read-only default (CURRENT in the repository; deployed state UNKNOWN)

Merged by #54 at `aa9b3cc39`. These statements describe the source at the
baseline, not a running service.

- `cmd/mctl-glovo/c2.go:82-90`: `loadBasketWrites` accepts only unset, empty
  and `false` for `GLOVO_C2_BASKET_WRITES`. `true` and any other spelling are
  a startup error, so the variable cannot enable basket writes
  (`c2.go:61-62`). It is read only with C2 on (`c2.go:132`). The code seam
  for writes, `c2Config.basketWrites` and `basketApprover`, is set by no
  loader (`c2.go:51-58`).
- `cmd/mctl-glovo/mcp.go:157-159`: `newMCPRoutes` refuses to serve the write
  tools without a `BasketApprover`. `mcp.go:164-167` serves
  `hostedReadOnlyInstructions` unless writes are on.
- `internal/glovotools/backend.go:291-298`: with a hosted client set and no
  `BasketWrites`, a call by name to a basket write tool returns the fixed
  result `Basket changes are not enabled in this deployment. Basket tools are
  read-only here.` before the tool lookup and any client lookup. The tools are
  not listed.
- `cmd/mctl-glovo/mcp.go:262-278` (`approvalMiddleware`): a basket write tool
  is refused when there is no approver or the approver errors.
- `internal/glovoapi/hosted.go:81-116` and `internal/glovoapi/client.go:621-636`:
  a hosted client built without `WithBasketWrites` refuses a basket mutation
  (`ErrBasketWritesDisabled`) before the network, also on a redirect hop
  (`client.go:356-362`).
- `internal/linkflow/consent.go`: the consent version is `c2-link-v2` and its
  text claims only reading. Until mctl-api's `internal/glovoconsent` holds the
  same version, no link code is issued (comment in `consent.go`). The state
  of mctl-api is UNKNOWN here.

What this does not show (UNKNOWN): that the deployed image contains it, that
`GLOVO_C2_ENABLED` is `true`, that mctl-api serves `c2-link-v2`, and whether
`glovo.mctl.ai` reaches this service or `glovo-home`, which is not covered by
the read-only default (`README.md`, "Read-only by default"). For operations,
the effect is that a basket write is not an operation of the hosted service
at this baseline: none is listed in this runbook, and enabling one is a code
change with its own review, not a runbook step.

## 2. Boundaries and who decides

CURRENT (`CLAUDE.md`, `docs/design/real-glovo-accounts.md` section 6):

- mctl-api owns identity, JWKS, grants and consent, the pilot allowlist and
  the approval ledger. This service never asserts a human decision.
- Fail closed: a dependency that could not be read is a refusal or 503,
  never an empty result, "not found" or a pass. Errors are never cached.
- Token accepted only with exact `aud` (`https://glovo.mctl.ai/mcp`), exact
  `iss` (`https://api.mctl.ai`), EdDSA signature against mctl-api's JWKS
  (`internal/config/config.go`; `deploy/mac-mini/README.md` for the same
  values on `glovo-home`).

Hosting ambiguity (UNKNOWN): `README.md` says the host moves here in phase 4,
and `deploy/mac-mini/README.md` describes `glovo.mctl.ai` served through a
Cloudflare tunnel by `glovo-home`. Which backend answers the public host at
any time is not observed. Every production section below must first record
which backend is live as evidence, and does not assume either.

### 2.1 Roles

Role names are the ones the repository already uses. No new person or
account is created by this document.

| Role | Holds | Source |
|---|---|---|
| owner | Every GO in this document. Owner of the Mac mini, the Tailscale admin console and the accepted R3 risk. | `docs/design/real-glovo-accounts.md` section 8 |
| operator | Executes an owner-approved action on the cluster (gitops change, rollout, Vault, Postgres). Never decides. | `docs/design/token-store.md` section 7 |
| mctl-api admin | Flips the kill switch through mctl-api's control client; changes grants, consent, allowlist. | `cmd/mctl-glovo/control.go:19-23` (caller is external, UNKNOWN) |
| mini host owner | Runs tinyproxy, `tailscale serve`, launchd on the mini. Same person as owner today. | `deploy/mac-mini/README.md` |
| reviewer | Reads evidence, signs "evidence complete". Not the operator who acted. | PROPOSED; the reviewer policy is the owner's to set |

Same-person caveat (UNKNOWN): if one human holds owner, operator and mini
host owner, "separate" below means separate explicit statements recorded in
order (GO, then action, then evidence), not separate people. Whether a
reviewer must be a second person, and what a reviewer may decide, is
referred to the owner. This document gives the orchestrator no reviewer
authority.

### 2.2 Rule for every production action

Every production action in this document needs, in this order:

1. A named responsible role (column "Role").
2. A separate, explicit owner GO, given before the action, for that action
   and that window. This includes a kill: there is no exception for an
   emergency or a breach criterion. A GO for one row never covers another
   row, another environment, or a retry after a changed state. The task or
   section that describes an action is not a GO.
3. Evidence before (column "Evidence before").
4. The action.
5. Evidence after, including the failure signal being absent (column
   "Evidence after").
6. A recorded abort point (column "Abort").

This document contains no GO. Executing any action is out of scope for #46's
documentation slice.

## 3. Dependency outage

### 3.1 Behavior matrix (CURRENT)

| Dependency | Read where | When it fails | User-visible result | Cached? |
|---|---|---|---|---|
| mctl-api JWKS (verifier) | `internal/authn/jwks.go`, `verifier.go` | No usable key (never fetched, or older than 24 h) | `503 temporarily_unavailable`, `Retry-After: 30` (`internal/gateway/gateway.go:194-198`); `/readyz` not ready | Last good key set up to 24 h; errors not cached |
| mctl-api consent route | `internal/consentgate/gate.go`, `cmd/mctl-glovo/admit.go` | Any read failure, non-200, timeout (4 s per call), malformed body, other principal | Verifier: 503. Per call: `access refused` | Positive only: 30 s default, 60 s ceiling (verifier gate); 5 s per call (`admitTTL`) |
| Kill switch row (Postgres) | `internal/tokenstore/killswitch.go` | Read failed or timed out (5 s) | Fixed text `Glovo account access is switched off.`, `tool_call outcome=refused` at the middleware check | "running" cached at most 5 s; killed and failed reads never cached |
| Postgres (store) | `internal/tokenstore/postgres.go` | Unreadable | `ErrUnavailable`, answered as `unavailable` (`internal/glovotools/hosted.go:37`) | No |
| Vault transit | `internal/tokenstore/transit.go`, `k8slogin.go` | Login or decrypt fails | `ErrUnavailable`, same answer | No |
| Egress proxy | `internal/glovoapi/client.go:140-165,274-293` | Unreachable proxy | The Glovo call fails; classified transient in refresh (`internal/tokenstore/store.go:448`) | No |
| Glovo (via proxy) | `internal/glovoapi` | 401/403 on refresh | `needs_relink` (section 7); 429 is `RateLimited`, transient | No |

Key points:

- A Postgres outage during a call is first seen as the kill switch refusal,
  because the kill check precedes the session lookup
  (`cmd/mctl-glovo/mcp.go:303-312`). It is indistinguishable from an
  intentional kill in the user-visible text. When the middleware kill check
  refuses, the `tool_call` audit line is `outcome="refused"` for both. A kill
  seen later, inside a handler (the store's own check in `decryptRow`,
  `internal/tokenstore/store.go:254-264`, including a decrypt-cap trip), comes
  back as the same text in a tool result and the line is `outcome="error"`
  (section 3.2). The text and the `tool_call` line cannot tell an
  intentional kill from a storage failure. The control route has no read, so
  the row state cannot be read through it (section 8.1). Whether any
  discriminator is wired in the deployed alerting is UNKNOWN.
- A Vault or Postgres failure never reads as "not linked". `ErrNotLinked`
  is returned only for an absent row that was read successfully
  (`internal/tokenstore/errors.go`).

### 3.2 Safe refusal and reason categories (CURRENT)

The categories describe what the client sees. They are not the audit
outcome. Texts are fixed and carry no dependency detail
(`internal/glovotools/hosted.go:30-70`).

The `tool_call` line is written by `auditMiddleware`
(`cmd/mctl-glovo/mcp.go:248-331`). Its `outcome` is `ok` only when the call
returned no error and a non-`isError` result (`mcp.go:324-327`). It is
`refused` only for an actual rejection by the middleware: no verified
principal, the kill check, or the admitter. Every other result is `error`.
That includes every `isError` result a tool returns: `unavailable`,
`not_linked`, `needs_relink`, `suspended`, `rate_limited`, `busy`, a kill
text raised inside a handler, the read-only refusal of a basket write tool
(`basket_writes_off` below), and any other error. The middleware does not
parse the result text (`mcp.go:253-256`). So in current `tool_call` lines `unavailable` cannot be
told apart from any other `error`, and the `tool_call` line alone never
proves "unavailable".

| Category (proposed label value) | Trigger | Client-visible result | `tool_call` outcome | Other evidence |
|---|---|---|---|---|
| `unauthenticated` | No verified principal | error `unauthenticated` | `refused` | none |
| `access_refused` | Admitter said no or could not read | error `access refused` | `refused` | none |
| `killed_or_unreadable` (middleware) | Kill row on, or unreadable, at the middleware check | tool result `Glovo account access is switched off.` | `refused` | none |
| `killed` (in handler) | Kill seen by the store inside a call, including a decrypt-cap trip | same text | `error` | `tokenstore` events for a trip (section 8.1) |
| `not_linked` | No link row, read ok | tool result `Your Glovo account is not linked. Link it at https://glovo.mctl.ai/link.` | `error` | none for this call |
| `needs_relink` | State `needs_relink`, or Glovo rejected refresh | tool result `Your Glovo session expired. Link your account again at ...` | `error` | `tokenstore` event reason `glovo_rejected` or `refresh_failures` when the state is set |
| `suspended` | State `suspended` (decrypt cap or operator) | tool result `Your Glovo link is suspended.` | `error` | `tokenstore` event `suspend` when set |
| `rate_limited` | Request cap hit | tool result `Glovo request rate limit reached. Try again later.` | `error` | possibly a `tokenstore` event; not confirmed here |
| `busy` | Another holder has the refresh lease | tool result `Your Glovo link is busy. Retry in a moment.` | `error` | none |
| `basket_writes_off` | A call by name to `glovo_add_to_basket`, `glovo_set_quantity` or `glovo_remove_from_basket`, which are not listed (section 1.3). Refused by the backend (`internal/glovotools/backend.go:296-298`), or by `approvalMiddleware` when no approver exists (`mcp.go:268-273`) | tool result `Basket changes are not enabled in this deployment. Basket tools are read-only here.` | `error` | none; it follows the kill check and the admitter, so a refusal by either is `refused` first |
| `unavailable` | Postgres, Vault, integrity, supersede, proxy or anything else unclassified | error `childmcp.ErrUnavailable` | `error` | none; the same `outcome` as every row above marked `error` |
| `token_unverifiable` | JWKS not observable | HTTP 503 | no `tool_call` line | `request refused` line, `reason`, `status=503` |
| `token_invalid` | Bad token for this resource | HTTP 401 `invalid_token` | no `tool_call` line | `request refused` line, `status=401` |
| `access_denied` | Principal not allowed | HTTP 403 | no `tool_call` line | `request refused` line, `status=403` |

PROPOSED (unbuilt, needs its own owner-approved slice): a fixed-value
`reason` field on the `tool_call` line, so that `unavailable` can be
separated from the other `error` cases. No such field exists. No threshold
that would use it is approved.

The label values are proposed names for grouping. The log fields they map
from exist today, except where a row says "none". No metric with these names
exists (section 9.1).

## 4. Egress proxy loss: no direct fallback

CURRENT:

- Hosted clients are built with `glovoapi.WithProxy(c.proxy)` and
  `glovoapi.WithRequireProxy()` (`cmd/mctl-glovo/c2.go:234,270`; the
  session validator at line 234 uses `WithProxy` only, the clients at line
  270 add `WithRequireProxy`). `WithRequireProxy` makes a missing proxy a
  constructor error and requires an https base URL
  (`internal/glovoapi/client.go:274-293`).
  `GLOVO_EGRESS_PROXY` is required when the flag is on
  (`cmd/mctl-glovo/c2.go:167-169`).
- The client cannot be combined with `WithHTTPClient`, so there is no code
  path to a direct call (`client.go:283-285`).
- `HTTPS_PROXY` and `NO_PROXY` are not consulted
  (`deploy/mac-mini/README.md`, "Cluster side").
- The proxy's own filter and listener are described in
  `deploy/mac-mini/README.md`: tinyproxy on `127.0.0.1:3128`, `ConnectPort
  443`, host filter `api.glovoapp.com` only, tailnet access through
  `tailscale serve`, ACL tag `tag:glovo-egress`. Whether any of this is
  installed is UNKNOWN.

Behavior with the proxy down (CURRENT): the Glovo call errors. In refresh it
is "transient": no state change and no failure counted
(`internal/tokenstore/store.go:448`). The client sees `unavailable` and the
`tool_call` line is `outcome=error` (section 3.2). Nothing
falls back to a direct call.

Verification that there is no direct fallback in a drill (PROPOSED):
evidence from the cluster NetworkPolicy and the pod's egress denial counters
that no packet from the pod reached a Glovo address except through the
sidecar. The NetworkPolicy and counters are UNKNOWN; the existing source
design only states the intent (`docs/design/token-store.md` T3 row).

### 4.1 Proxy loss and recovery actions

Existing documented operations on the mini, reviewed against
`deploy/mac-mini/README.md`. They are documented in the repository; this task
did not run them and does not know that the mini matches the README.

| Operation | Source | Label |
|---|---|---|
| `launchctl bootout gui/$(id -u)/com.mctl.tinyproxy`, wait until `launchctl print gui/$(id -u)/com.mctl.tinyproxy` reports gone, then `launchctl bootstrap gui/$(id -u) <plist>` | README "launchd" and "Tailnet exposure" | CURRENT (documented) |
| `tailscale serve status`, `tailscale serve --bg --tcp 3128 tcp://127.0.0.1:3128` | README "Tailnet exposure" | CURRENT (documented) |
| `curl -x http://127.0.0.1:3128 ...` to test tinyproxy alone on the mini | README "Checks" | CURRENT (documented) |
| `curl -x http://<mini-tailscale-ip>:3128 https://api.glovoapp.com/` from another tailnet node | README "Checks" | CURRENT (documented); only meaningful from a node the ACL allows |
| `grep -i filtered ~/Library/Logs/mctl-tinyproxy.log` | README "Checks" | CURRENT (documented) |
| Cluster-side pod/sidecar restart or status | not in this repository (mctl-gitops, E3) | UNAVAILABLE here |

Do not use: `brew services start tinyproxy` (listens on every interface,
README), testing through the mini's own Tailscale address (does not pass
data, README).

| Step | Role | Evidence before | Action | Evidence after | Abort |
|---|---|---|---|---|---|
| Declare proxy-loss incident | operator | Alert or user report plus a rise in `tool_call outcome=error`, which includes `unavailable` and every other result error and does not by itself show proxy loss (section 3.2) | none (observe) | Owner GO recorded for each step below | Any evidence that traffic exits the cluster without the proxy: stop and ask the owner for a GO to kill (section 8); no action before that GO |
| Restart tinyproxy launch agent | mini host owner | `launchctl print` state, last tinyproxy log lines (no payload exists in them) | bootout, wait for gone, bootstrap | `launchctl print` shows running; allowed curl from an allowed node returns an HTTP status from `api.glovoapp.com`; a `https://example.com/` curl is refused with 403; optionally a `tool_call outcome=ok` for the synthetic principal, which is a live smoke check of production and needs a separate prior explicit owner GO (section 2.2; not a section 11 drill, and the isolated-environment record does not replace this GO) | `bootstrap` fails with `5: Input/output error`: stop, do not retry in a loop, owner decides |
| Restore `tailscale serve` | mini host owner | `tailscale serve status` output | per README | `tcp://<mini>:3128 -> 127.0.0.1:3128` listed; same curl checks | Policy change would be needed: stop. ACL edits are a separate owner GO. |

## 5. Vault and Postgres

### 5.1 Vault (CURRENT)

- Login uses Kubernetes auth with role `GLOVO_VAULT_ROLE` (default
  `mctl-glovo`) (`cmd/mctl-glovo/c2.go:125-127,238`). Address from
  `GLOVO_VAULT_ADDR`.
- Decrypt happens only in `decryptRow` (`internal/tokenstore/store.go:253`),
  after the kill check, the caps, the key version check and a successful
  audit row. A decrypt that cannot be audited does not happen.
- Global decrypt cap breach trips the kill switch and latches it in the
  process until restart (`store.go:259-265`, `killswitch.go:99-146`). Per-pid
  breach suspends that link.
- `Unlink` never needs Vault (`store.go:748`).

Vault outage: calls answer `unavailable` (client-visible; the `tool_call`
line is `outcome=error`, section 3.2); no user is told "not linked"; no
cache longer than the in-memory client's lifetime hides it (UNKNOWN how long
a loaded client stays usable; `internal/tokenstore/sessions.go` holds the
idle TTL).

### 5.2 Postgres (CURRENT)

- Schema version is checked at start; a different or unreadable version
  stops the process (`internal/tokenstore/postgres.go:14-38`).
- The store ships killed: a fresh schema sets `glovo_control.c2_killed =
  true` (`cmd/glovo-migrate/main.go` header).
- An unreadable control row is killed.

### 5.3 Recovery actions

Vault and Postgres administration is not provided by this repository. The
only repository-provided operations are the kill flip (section 8) and
`glovo-migrate` (section 10). Everything else is UNAVAILABLE here; the
breach runbook in `docs/design/token-store.md` section 7 is DESIGN only and
its Vault commands were not checked against a Vault.

| Step | Role | Evidence before | Action | Evidence after | Abort |
|---|---|---|---|---|---|
| Establish the kill state before touching either dependency | operator | The hosted control route is PUT only and has no read (section 8.1), so the current state is UNKNOWN unless an available non-mutating observation exists. A response to a previous authorized write and audit lines are observations at their recorded time only. Any new PUT, including one that asks to keep the state killed, calls `Set` and needs its own prior explicit owner GO (section 2.2) | none | State recorded as UNKNOWN, or as the dated observation with its source. In-flight work, other replicas and decrypt counts are not observed (section 8.1); a reduced count of `decrypt` or `tool_call outcome=ok` lines is not proof, and absence of logs is not proof | State cannot be established: record UNKNOWN and escalate to owner; no PUT without a GO |
| Restore Vault or Postgres availability | operator, with owner | Provider status, last good time | Outside this repository: UNAVAILABLE here | Service restarted if it exited; `/readyz` 200 is not enough: synthetic `tool_call outcome=ok` with kill off, a live smoke check that needs a separate prior explicit owner GO (section 2.2; not a section 11 drill, and the isolated-environment record does not replace this GO) | Schema version mismatch on start: do not migrate by hand; section 10 |
| Un-kill | mctl-api admin, owner GO | Dependencies observed healthy by an independent read; owner GO for this PUT; state before is UNKNOWN or a dated observation (section 8.1) | Flip to running (section 8) | `200 {"killed": false}` identifies the stored row as read back by the answering process and no latch in that process; fleet and effective state remain UNKNOWN. A database `kill_off` audit record, if readable, is an observation at its time (section 8.1) | `200 {"killed": true}` on an un-kill identifies a latch in that process, with the stored row possibly open (section 8.1); a non-200 or 503 `state_unknown` is UNKNOWN. Restart to clear a latch is a separate prior explicit owner GO (section 8.2) |

## 6. Session lifecycle: refresh, relink, revoke, suspend, erase

### 6.1 Refresh expiry and relink (CURRENT)

- A refresh that Glovo rejects with 400/401/403 sets `needs_relink` at once
  (`reason=glovo_rejected`). Other refresh failures (and refused session
  shapes) count; at `RefreshFailuresInARow` they set `needs_relink`
  (`reason=refresh_failures`). The default is 3, tunable by
  `GLOVO_C2_CAP_REFRESH_FAILURES_IN_A_ROW` up to 10
  (`internal/tokenstore/caps.go:32`, `cmd/mctl-glovo/c2caps.go:36`,
  `store.go:428-450`). Proxy, network and deadline errors are transient and
  count nothing.
- The user relinks through `/link` with a new one-time code and the local
  helper `cmd/glovo-link`, which does the whole flow with one fresh MCTL
  login: it requests its own code, previews, asks y/N, uploads. The routes that exist are `GET /link`, `POST
  /link/code`, `POST /link/preview`, `POST /link/upload`
  (`internal/linkflow/linkflow.go:88-91`). Until mctl-api serves masked
  identity (C2-1c), preview refuses (`README.md`).
- Who tells the user their session expired: no code sends an alert to the
  account owner. Design 4.5 says the channel is open (UNKNOWN). The only
  signal today is the user's next call returning `needs_relink`.
- A keepalive per pid is BUILT and DARK (`token-store.md` 4.5,
  `internal/tokenstore/keepalive.go`). It runs only when
  `GLOVO_C2_KEEPALIVE_INTERVAL` is set to a duration of 1h to 24h; unset or
  `0` is off, and a value outside the range stops the boot. Whether it is
  enabled in the deployed pod is UNKNOWN from this repository: turning it on
  is a gitops value plus an owner GO. Until then sessions refresh only on
  use. Once on, it reads each active pid once per interval through the same
  client, refresh coordinator and audit rows as a tool call, writes one
  `keepalive` log line per read, and stops under the kill switch. A
  refused refresh chain surfaces as `event=keepalive outcome=needs_relink`
  (with `alert=GlovoSessionNeedsRelink`) as well as the existing
  `refresh`/`glovo_rejected` audit row. The refresh-chain lifetime Glovo
  enforces is not measured (UNKNOWN); 24h is the glovo-home default and the
  upper bound of this setting.

### 6.2 Revoke (what is and is not available)

- Revoke at Glovo: not possible from here. The design says the user must
  sign out of other Glovo sessions (`token-store.md` 4.3). That Glovo's
  "sign out everywhere" kills refresh tokens is marked **unverified** there
  and stays UNKNOWN.
- Revoke the glovo:c2 grant or consent: a mctl-api action. The next call is
  refused within `admitTTL` (5 s) in consent mode; the verifier gate is at
  most 60 s (CURRENT). Which mechanism a given pilot uses is UNKNOWN.
- Unlink: `Store.Unlink` (`store.go:750`) works under kill without Vault
  or consent. Two callers exist in the source:
  - user: `POST /link/unlink` on the link routes, behind the same fresh MCTL
    login and link audience as an upload, reached with `glovo-link -unlink`
    (one y/N confirmation, no code, no session file). The pid is the
    verified one, never a body field.
  - operator: `POST /internal/v1/control/unlink`, `{"principal": "prn_..."}`,
    behind the control route's machine-user identity and proxy lock. Audit
    lines `account_unlink` / `account_unlink_refused` carry no principal.
  The route exists only when the control endpoint is configured
  (`cmd/mctl-glovo/c2.go` `buildControl`, `control.go` `WithAccounts`).
  No caller exists in this repository. Whether mctl-api has one was not read
  here (UNKNOWN), and there is no live run. The state in the source is
  CURRENT at `fb8638a0947d32bef4c484c82868efb4fa35e393` (merge of #64);
  whether a deployed image contains it is UNKNOWN. The live rehearsal plan
  is section 6.5. Design 4.3
  also describes an Unlink control on the `/link` page and a grantless sweep;
  neither is implemented (UNAVAILABLE).

### 6.3 Suspend and resume

- Automatic suspend exists for the per-pid decrypt cap (`store.go:266-272`).
- Operator suspend and `resume` are DESIGN only (`token-store.md` section 6).
  No resume operation exists in the source read (UNAVAILABLE). A suspended
  link therefore has no documented operator recovery other than relink. A
  new operator operation would need its own owner-approved slice.

### 6.4 Erase

- `Store.Erase` exists (`store.go:768`): unlink, an `erasure` event and the
  subject pseudonymisation in one transaction. Its caller is
  `POST /internal/v1/control/erase` (same identity as the unlink route,
  audit `account_erase`). Nothing in mctl-api calls it yet, so an erasure
  request has no end-to-end path until a caller (not found in this
  repository; mctl-api not read, UNKNOWN) and a live run exist.
- Backups: DESIGN says Postgres backups for 14 days can still hold
  ciphertext, and a restore must re-apply recorded unlink and erasure events
  before the service starts. That procedure is DESIGN; no tool for it exists
  here (UNAVAILABLE). The statement to a data subject must not claim
  crypto-erasure (`token-store.md` 4.4).
- Legal and GDPR ownership are parked by the owner (`real-glovo-accounts.md`
  section 8, 2026-11-16) and tracked under #44. They are not decided here.

| Action | Role | Evidence before | Evidence after | Abort |
|---|---|---|---|---|
| Relink a user | the user, with owner GO to open a code | consent recorded, pid admitted | `link` event outcome `ok`, a `tool_call outcome=ok` | upload refused or consent not held: stop, no retry loop |
| Revoke consent or grant | mctl-api admin, owner GO | principal and window recorded without raw pid in this repo | next `tool_call` for that principal is `refused` within the TTLs | none: refusal is the safe state |
| Unlink / erase by operator | machine-user caller, owner GO | principal and window recorded without raw pid in this repo | `account_unlink` / `account_erase` audit line outcome `ok`, a `tool_call` for that principal answers not linked | `state_unknown` or any refusal: stop, do not retry in a loop, do not improvise SQL |
| Unlink by the user | the user, `glovo-link -unlink` | fresh MCTL login | helper prints `unlinked`; `link` row gone; next `tool_call` answers not linked | refusal text from the helper: stop |
| Suspend / resume by operator | operator, owner GO | UNAVAILABLE | UNAVAILABLE | do not improvise SQL; ask the orchestrator for a design slice |

### 6.5 First live link requires prior unlink and erase rehearsal (PROPOSED)

PROPOSED, no GO. The first live link of any account needs prior live unlink
and erase rehearsal evidence recorded under #43, using at most two
owner-authorized test accounts and an explicit prior owner GO for each
effect (section 2.2). The plan, with preconditions, expected evidence,
readback limits, aborts and contingency, is
`docs/design/c2-offline-acceptance.md` section 10. If no suitably linked test
account exists, the plan is blocked for an owner decision there (10.3); it is
not worked around by linking live. Readback limit that applies to every
action row above: there is no link-state read route, so evidence is the
response, the audit lines and a following "not linked" answer, each with the
limits stated in that section.

## 7. Alert and evidence signals: what exists today

CURRENT: the service writes JSON `slog` lines to stdout
(`cmd/mctl-glovo/main.go:24`). It has no metrics endpoint, no Prometheus or
OpenTelemetry client and no alert sender (grep of non-test code finds no
`prometheus`, `promhttp`, `/metrics` or `expvar`). The only alert hook in
the repository is `GLOVO_HOME_ALERT_CMD` for `glovo-home`, which is outside
this task.

Log lines available as evidence (all CURRENT, fields as coded):

| Line | Fields | Source |
|---|---|---|
| `tool_call` | `who` (keyed digest `pid-<24 hex>`, not the raw principal), `tool`, `outcome` (`ok`; `refused` only for no principal, the middleware kill check or the admitter; `error` for everything else, including every result error, section 3.2), `duration_ms` | `cmd/mctl-glovo/mcp.go:248-298` |
| `request refused` | `reason`, `status`, `method` | `internal/gateway/gateway.go:192` |
| `tokenstore` | `event`, `outcome`, `reason`, `actor_kind`, `link_id`, `request_id`, `audit_row_written` on failure. Emitted by `store.audit`, which also writes the row (`audit.go:60-72`) | `internal/tokenstore/audit.go:60-93` |
| `keepalive` | `event=keepalive`, `outcome` (`ok`, `needs_relink`, `transient_error`, `auth_rejected`, `integrity_error`, `suspended`, `unlinked`, `killed`, `list_failed`), `reason`, `link_id`, `failures` and `retry_in` on a failure. Only when the keepalive is enabled; carries no `pid` | `internal/tokenstore/keepalive.go` |
| `kill_switch_flip` | `requested`, `stored`, `outcome` (`ok` with `stored`, or `unknown`). Emitted only by the control route after a `Set` attempt, never by a decrypt-cap trip | `cmd/mctl-glovo/control.go:168,173` |
| `kill_switch_refused` | `status` | `control.go:154` |
| `kill switch flip failed` | `error_class` (`store_unavailable`, `timeout`, `other`) | `control.go:169` |
| `initial jwks fetch failed`, `initial control jwks fetch failed` | `error` | `c2.go:291,435` |
| `exit` | `error` | `main.go:26` |

Gaps (CURRENT): no line separates a Postgres-caused refusal from an
intentional kill on the `tool_call` path (section 3.1), and no line
separates `unavailable` from any other `outcome=error` (section 3.2). The
control route has no state read (section 8.1). No line counts
Glovo 403/429 as a rate. The `tokenstore` events carry `link_id`, which is
not the pid.

### 7.1 Current MCTL monitoring

Reuse is the owner's instruction. What MCTL monitoring exists for this
service (Loki scope, dashboards, alert routes, notification channel) is
UNKNOWN: this repository does not contain it and this task did not read the
cluster. `docs/design/token-store.md` section 5 states a Loki retention of 14
days and an hourly Vault-vs-audit reconciliation, both DESIGN, not verified as
deployed.

Do not invent a metric name, dashboard or query. Any alert below is a
PROPOSAL to be expressed in MCTL's existing system by someone who can read
it.

## 8. Kill, rollback and the flip path

### 8.1 Kill (CURRENT)

- Runtime kill row `glovo_control.c2_killed`. An unreadable or missing row
  is killed. Every hosted tool call checks it before the admitter and before
  any session lookup (`cmd/mctl-glovo/mcp.go:303-312`).
- Control route: `PUT /internal/v1/control/kill-switch`, body
  `{"killed": bool}` (`control.go:23,101-105`). It is mounted only when all
  four `GLOVO_C2_CONTROL_*` variables are set. It verifies a machine-user
  token against an explicit subject allowlist, answers 404 for any request
  carrying proxy forwarding headers, and on success answers 200
  `{"killed": <value returned by Set>}`. Any non-200 means "state unknown"
  (`control.go:132-175`, design "Kill switch flip path").
- The route is PUT only. There is no read-only kill-state route
  (UNAVAILABLE): `Register` mounts only the `PUT` pattern. Every PUT calls
  `Set` (`control.go:166`), including a request to keep the state killed,
  so it writes the row and audit and is a production action that needs its
  own separate prior explicit owner GO under section 2.2. The runbook
  therefore has no way to read the current kill state through the hosted
  control route. Where a step needs the current state, it records UNKNOWN
  or an available non-mutating observation (an emitted log line, a readable
  database audit row, a user-visible refusal) with the time and source.
  An audit log line, or the response to an earlier authorized PUT, is an
  observation at its recorded time. It is not a current-state read and it is
  not proof that any other replica has the same state.
- A flip to killed stays in force even if its audit row cannot be written
  (the kill answer is still `true`, so a `200 {"killed": true}` to a kill
  request does not show that an audit row exists). A flip to running whose
  readback or audit row fails is followed by a best-effort re-kill, which
  itself can fail, and the request ends in `503 state_unknown`, never in a
  200 (`internal/tokenstore/killswitch.go:99-146`).
- Failure and unknown outcomes (CURRENT, `killswitch.go:99-146`,
  `control.go:166-172`). Any `Set` error is answered `503 state_unknown` and
  logged as `kill_switch_flip outcome=unknown`. This is not a latch
  success:
  - The row write fails: the state is UNKNOWN. `Set` tries to record a
    `write_failed` audit row, best effort. It does not re-kill.
  - The readback fails: the state is UNKNOWN. For an un-kill `Set` attempts a
    best-effort re-kill and a `readback_failed` audit row. Neither is
    promised to have succeeded. For a kill, `Set` attempts nothing further.
  - The readback differs from the request: an error is returned and the state
    is UNKNOWN.
  - The audit write of an un-kill fails: `Set` attempts a best-effort
    re-kill and returns the error. Do not assume the re-kill succeeded.
  - A non-200, a 503 or a timeout is UNKNOWN. It is not "still killed".
- Stored row versus effective per-process state (CURRENT,
  `killswitch.go:10-34,45-77,79-146`):
  - The stored row is `glovo_control.c2_killed`. The effective state is what
    each process answers from `Check`.
  - A "running" answer is cached in a process for at most 5 s
    (`MaxKillCache`). A killed answer and a failed read are not cached.
  - `Set` invalidates only the local process's cache. It does not cancel
    operations already past the check, and it does not prove that any other
    replica has stopped calls or decrypts. Other replicas learn the row only
    on their next read, within the 5 s cache.
  - Whether `Set` completed on any replica other than the one that answered,
    and whether in-flight calls drained, is UNKNOWN: no drain or replica
    evidence was observed. Drain and replica checks are future checks that
    need their own separate prior owner GO.
  - Two different paths produce kill evidence. Do not merge them.
    - Manual `Set` (the control route): database audit rows `kill_on` or
      `kill_off` (reason `operator_control`, actor operator) written directly
      by `Set` through `InsertAudit`. `Set` does not call `store.logEvent`, so
      it emits no `tokenstore` log line. The emitted log evidence of a manual
      flip is the `kill_switch_flip` line from `control.go:168,173`
      (section 7). Either the row or the line can exist without the other.
    - Decrypt-cap `Trip` (`internal/tokenstore/store.go:258-265`): a global
      decrypt cap breach latches the switch in that process and attempts the
      row write. The error of that write is ignored. It then writes
      `cap_exceeded` and `kill_on` audit events with reason
      `global_decrypt_cap` through `store.audit`, which writes the row and
      emits a `tokenstore` log line with `event`, `outcome`, `reason` and
      `audit_row_written=false` when the row write failed (`audit.go:60-93`).
      A trip emits no `kill_switch_flip` line.
  - A failed audit write for a kill does not undo it, so the kill can be
    effective while no `kill_on` audit row exists. A failed `kill_on` write
    is recorded best effort as `outcome=error`, which can itself fail.
  - Absence of a log line or row is never proof of a stop, of zero decrypts
    or of anything else.
- Latched process state, stored row and other replicas (CURRENT,
  `killswitch.go:79-146`):
  - A decrypt-cap `Trip` latches kill in that process until restart, even if
    the row is later set to running (`killswitch.go:10-21`). `Set` does not
    clear the latch.
  - While latched, an un-kill PUT still opens the stored row and writes its
    `kill_off` audit row, when those writes succeed, and `Set` then returns
    `true` for the latched process (`killswitch.go:141-144`). The endpoint
    answers `200 {"killed": true}` to a `{"killed": false}` request.
  - That answer identifies the latch in the process that answered. It does
    not report the stored row, which can now be open, and it does not report
    any other replica, which reads the row and can run. The fleet or
    effective state is UNKNOWN. `Trip` takes no `setMu`, so an un-kill that
    lands after a trip's row write has the same result (`killswitch.go:93-98`).
  - A latch can exist with no database row. The `Trip` row write can fail and
    `kill_on` rows can fail. A restart already cleared an earlier latch.
    The `tokenstore` log events `cap_exceeded` and `kill_on` with reason
    `global_decrypt_cap` are evidence of a trip at their time. Treat any
    database row or log line as evidence with its limits: it does not
    identify which replica latched, whether the latch is still held, or
    whether the stored row is killed. Absence is not proof of no latch.
  - Restart to clear the latch is a cluster action (UNAVAILABLE here) and
    needs its own separate prior explicit owner GO.
- The caller of the control route (mctl-api's `glovoc2control` client) and
  its credentials are outside this repository (UNKNOWN). Whether the route is
  mounted in the deployed pod is UNKNOWN.
- If the control route is not mounted, flipping the row has no operation in
  this repository (UNAVAILABLE). Do not edit the row by SQL.

### 8.2 Kill and un-kill actions

| Step | Role | Evidence before | Action | Evidence after | Abort |
|---|---|---|---|---|---|
| Kill | mctl-api admin, only after a prior explicit owner GO for this kill. No exception for a breach criterion in `token-store.md` section 7: that wording is historical and the current operator decision overrides it | Incident or alert recorded; owner GO recorded for this PUT. The current state is UNKNOWN unless a dated non-mutating observation exists (section 8.1); a "keep killed" PUT is also a `Set` and needs its own GO | flip to killed | `200 {"killed": true}` is the answering process's `Set` result after the row readback; a database `kill_on` audit record, if one can be read (it may be missing, section 8.1). Effective stop on this process within 5 s is the code's intent; that other replicas stopped, that in-flight calls drained and that no decrypt occurred afterwards are UNOBSERVED, and absence of logs does not show them. Drain and replica checks need separate prior owner GO | Non-200 or 503 `state_unknown`: state is UNKNOWN; do not improvise and do not assume a stop or a successful re-kill. Any further step, including scaling to zero, is a separate prior owner GO |
| Un-kill | mctl-api admin, prior explicit owner GO | Cause fixed and independently observed; synthetic principal ready; state before is UNKNOWN or a dated observation | flip to running | `200 {"killed": false}` identifies the stored row as read back and no latch in the answering process; fleet and effective state remain UNKNOWN. A database `kill_off` audit record, if readable, is an observation at its time; the optional synthetic `tool_call` is a live smoke check of the production service: even with synthetic inputs it needs a separate prior explicit owner GO (section 2.2), and it is not a section 11 drill (those depend on the owner's isolated-environment record, section 11) | `200 {"killed": true}` on an un-kill: a latch in the answering process (section 8.1); the stored row may be open and other replicas may run, so fleet state is UNKNOWN; do not loop. Any non-200, including 503 `state_unknown`, keeps the state UNKNOWN. It is not a latch result. A best-effort re-kill applies only to an un-kill readback error or an un-kill audit failure (section 8.1, `KillSwitch.Set`), and it is not promised to have succeeded. It does not apply to an initial write error, where `Set` does not re-kill, or to a readback that differs from the request, where `Set` returns an error without re-kill. Restart is a separate prior explicit owner GO |
| Clear a latch | operator, prior explicit owner GO for the restart | Evidence of a trip, if any is available, with its limits (section 8.1): `tokenstore` log events `cap_exceeded` and `kill_on` reason `global_decrypt_cap`, or database rows of the same events. A database row is not required, and none of this proves the latch is still held or on which replica | pod restart (cluster action, not provided here, UNAVAILABLE) | restarted pod, then un-kill rows above | Latch returns: do not un-kill; decrypt cap breach is a security signal for #44 |

### 8.3 Rollback

- Feature rollback: set `GLOVO_C2_ENABLED` to false through the gitops
  change plus rollout. Off removes `/link` and `/mcp`, builds no store, no
  Vault login and no database connection (`c2.go:218-221`). The gitops file
  and rollout procedure are outside this repository (UNAVAILABLE here).
  `/readyz` then answers `200 ok` with no checks (section 1.1): after a
  rollback, readiness says nothing about whether the routes are gone.
  Evidence after must be a 404 on `/link` and `/mcp` observed from the
  ingress, with the backend identified.- Stage rollback (users, writes): sequence and caps come from #40. Not
  restated here.
- Host rollback: `README.md` states rollback of the host move is reverting
  that change (gitops, UNAVAILABLE here).

## 9. Proposed bounded private alerts and safe labels (PROPOSED)

None of this exists. Thresholds without an existing operator decision are
proposals. No metric, dashboard or alert named here is claimed deployed.

### 9.1 Safe labels

Allowed: `component` (`gateway`, `mcp`, `tokenstore`, `control`, `proxy`),
`outcome` (`ok`, `refused`, `error`), a category from section 3.2, `event`,
`reason` (fixed codes only), `stage` (as named in #40).

Forbidden in any label, log, alert text or annotation: a raw principal id
or `pid`, any email, token, session, ciphertext, link code, Glovo account id,
address, order or basket content, IP address of a user, URL with a query. The
digest `who` is a keyed per-deployment value: it must not be placed in a
shared alert text either, because a human could map it back with the key.
Use a count, not an identity. Alert recipients are the private owner channel
only (UNKNOWN which).

### 9.2 Proposed alerts

| # | Signal | Source | Proposed threshold | Clear | Status |
|---|---|---|---|---|---|
| A1 | Any kill event: manual (database `kill_on` audit row, or `kill_switch_flip` log line with `stored=true` or `outcome=unknown`), or a decrypt-cap trip (`tokenstore` log events `cap_exceeded` and `kill_on` with reason `global_decrypt_cap`, or the same database rows). Not one source alone (section 8.1) | database audit table, the control log line and the `tokenstore` log line | any occurrence | recorded owner decision; a later write response is an observation at its time, not a clear of fleet state | PROPOSED |
| A2 | Rise in `tool_call outcome=error`. This outcome includes `unavailable` and every other result error (`not_linked`, `needs_relink`, `suspended`, `rate_limited`, `busy`, in-handler kill and more), so it is not an `unavailable` rate. A real `unavailable` share needs the PROPOSED `reason` field of section 3.2 (unbuilt) | `tool_call outcome=error` | none approved; the earlier figure of more than 5 in 5 minutes is an unowned placeholder only | zero in 15 minutes and a synthetic `ok` | PROPOSED, numbers unowned and unapproved |
| A3 | `tool_call outcome=refused` that may come from the kill check with no kill intended. `refused` also covers no principal and admitter refusals | `tool_call refused`; the control route cannot read the row (section 8.1) | none approved | store readable again | PROPOSED, needs the discriminator of section 3.1 (not built) |
| A4 | JWKS not usable | `request refused` `status=503`, `initial jwks fetch failed` | any sustained for 5 minutes | `request refused` 503 absent for 10 minutes | PROPOSED |
| A5 | Proxy loss | `unavailable` plus tinyproxy log silence, or the owner's existing mini check | UNKNOWN | allowed curl returns an HTTP status; synthetic `ok` | PROPOSED; the mini side belongs to the mini host owner |
| A6 | Glovo 403/429 rising | `tokenstore` refresh reasons `glovo_rejected`; no 429 count exists today | the epic requires an alert; no number decided | decay below threshold for 30 minutes | PROPOSED; 429 counting would be new code (UNAVAILABLE) |
| A7 | `needs_relink` or `suspend` events; with the keepalive on, also `keepalive outcome=needs_relink` (a session that died unused) | `tokenstore`, `keepalive` | aggregate count only | none per user | PROPOSED |
| A8 | `integrity_error` or `cap_exceeded` | `tokenstore` | any | owner security review | PROPOSED; ties to #44 |
| A9 | Decrypt count vs Vault audit mismatch or `unknown` window | Vault audit | per `token-store.md` section 5 | an observed equal window | DESIGN, not verified deployed |

A window in which a source could not be read is `UNKNOWN` and alerts on its
own. It never counts as clear.

## 10. Mini restart, Tailscale, image and migration recovery

### 10.1 Mini restart and Tailscale recovery

The mini hosts the proxy for the hosted service and, separately, `glovo-home`.
Only the proxy part is in scope.

| Step | Role | Evidence before | Action | Evidence after | Abort |
|---|---|---|---|---|---|
| Plan a restart | mini host owner, owner GO | Kill state recorded as UNKNOWN or as a dated non-mutating observation (the control route has no read, section 8.1); whether `glovo-home` also runs recorded (its outage is out of scope) | none | Window agreed | Any user mid-link or write stage active: do not proceed |
| After the mini boots | mini host owner | launch agent for tinyproxy and Tailscale daemon state | start per README | `tailscale serve status` shows the listener; allowed-node curl returns an HTTP status; a non-allowed host is refused; an optional synthetic `tool_call` `ok` after un-kill, which is a live smoke check of production and needs a separate prior explicit owner GO (section 2.2; not a section 11 drill, and the isolated-environment record does not replace this GO) | Serve missing and the ACL changes would be needed: stop (owner GO for ACL) |
| Tailscale auth or tag problem | owner | Admin console state | UNAVAILABLE here (ACL edits are a separate owner GO; README warns tagging changes ownership) | allowed node reaches 3128; a node without `tag:glovo-egress` times out | none |

Unobserved: whether the mini sleeps, auto-updates, or restarts on power loss.
These are UNKNOWN.

### 10.2 Image recovery

- One image, three binaries: `mctl-glovo`, `glovo-migrate`, `token-refresher`
  (`Dockerfile`). Tags are semver without `v`:
  `ghcr.io/mctlhq/mctl-glovo:<semver>`.
- The image build and the deploy are central in mctl-gitops
  (`README.md`). The dispatch to gitops is off until the repository variable
  `GLOVO_IMAGE_DISPATCH` is `true` (value UNKNOWN).
- Rolling the deployment to an older image tag is a gitops action
  (UNAVAILABLE here). A previous image's schema expectation is
  `tokenstore.SchemaVersion` (currently 1): a binary that expects another
  version stops at start (CURRENT).
- Consequence: schema is forward only. `glovo-migrate` has no down path and
  refuses any version other than the one its build expects
  (`cmd/glovo-migrate/migrate.go:185-215`). An image rollback after a schema
  change therefore crashes at start until the schema matches. There is no
  documented schema rollback (UNAVAILABLE). Every future schema change needs
  its own recovery design.
- The consent-token sidecar (`token-refresher`) fails closed: after a failed
  refresh the file is removed at the token's own expiry and the container
  exits 1; the consent route then refuses (CURRENT, `README.md`,
  `internal/tokenrefresh/tokenrefresh.go`). A sidecar stuck restarting makes
  consent reads fail, which makes admission fail closed (503 or refused).

### 10.3 Migration recovery (`glovo-migrate`)

Reviewed against `cmd/glovo-migrate/main.go` and `migrate.go`:

- DSN from the environment variable named by `-dsn-env` (default
  `GLOVO_MIGRATE_DATABASE_URL`), never printed. `-expect-role` default
  `glovo_c2_migrator`. `-runtime-role` applies grants. `-timeout` default 5
  minutes. `-insecure-no-tls` exists for local development and CI only and
  must never be used against production.
- Outcomes: 0 applied or already current; 1 refused (flags, no TLS, wrong
  role, unexpected schema); 3 could not observe or step failed. An up-to-date
  schema is left alone and the kill switch is not reset. A partial or
  different schema is refused, never repaired.
- An empty database gets the schema in one transaction and ships killed.

| Step | Role | Evidence before | Action | Evidence after | Abort |
|---|---|---|---|---|---|
| Run the migration Job | operator, owner GO | Role and TLS requirement recorded without the DSN; current schema version read; kill row state | Job with the `glovo-migrate` command override (a cluster object, UNAVAILABLE here) | Exit 0 and the printed `schema ... at version N`; N equals the target image's `SchemaVersion`; kill still in the state recorded before | Exit 1 or 3: do not rerun in a loop, do not edit tables; owner decides |
| Recover a refused schema | owner | The exact refusal text | UNAVAILABLE: no repair path exists | n/a | Restore from Postgres backup is a separate design (restore must re-apply unlinks, DESIGN) |

## 11. Synthetic future drills (not executed)

None of these has been run. All drills are proposals. This task sends no
alert, generates no traffic, changes no permission or flag, deploys nothing
and implements no test. This document authorizes no drill, no live action
and no alert send, and no kill or recovery operation is invented here.

Isolated-environment record (the prerequisite for every drill and for D9):
a drill is outside section 2.2 only after the owner has made a prior record
that names the isolated environment it runs in. The record must show that
the environment has its own Vault, its own egress proxy and its own
database, and that none of the three serves production. No such record
exists at this baseline (UNKNOWN to this document; none was observed).
Until it exists, every drill below, including a "test copy" of a production
component, is a production action: it needs a named role and a separate,
explicit owner GO under section 2.2, for that drill and that window, with
evidence before and after and an abort point. A record for one environment
does not cover another environment. The record is not a GO for a
production action, and it is not a GO for a live smoke check.

Approval rule (one rule for this section and D9): with that record in
place, a synthetic exercise that runs only inside the named environment and
touches nothing outside it needs no further GO. Any action with production
effects needs a prior explicit owner GO (section 2.2). An exercise that
touches a production system, the real Glovo provider, a real user or the
existing operational alert path is not a synthetic exercise, with or
without the record. It is a production action under section 2.2.

Offline rule: a drill uses provider test doubles (for example a Glovo test
double) and the isolated environment's own Vault, proxy and database, with
no packet leaving for the real provider or any production system. A
synthetic identity or synthetic account that reaches the real provider is
live traffic and falls under the approval rule above. Alert checks use a
local stub or an explicitly isolated test sink, never the existing
operational alert path.

Each drill needs a named operator and pre-created synthetic fixtures (a
synthetic principal and a synthetic Glovo account in a test double, never a
real user's). The existence of those fixtures is UNKNOWN. #54 and #58 are
merged and neither authorizes a live smoke test or a drill. A live smoke
check of the production service is not a drill: it keeps its own separate
prior explicit owner GO (section 2.2), which the record above does not
replace.

Common rules: the first step is to cite the isolated-environment record by
its date and name, and to stop if it is missing or does not name the
Vault, proxy and database in use; the next is a recorded kill state of the
test deployment, taken as a fixture observation (a read of the test double or the
test store, labelled as such), never through the hosted control route, which
has no read (section 8.1); the stage sequence is the one #40 records
(read-only only; writes are not drilled here); stop on any sign of a real
user, a production system or real-provider traffic. Stop means halt and ask
the owner for a GO if production effects are involved. It never means an
unauthorized kill.

| ID | Synthetic case | Method (proposed) | Expected failure signal | Expected clear signal | Abort and rollback evidence |
|---|---|---|---|---|---|
| D1 | JWKS unreachable | Needs the isolated-environment record. Block the JWKS route for the pod in that environment only (a network action, UNAVAILABLE here). Without the record: production action, section 2.2 | `request refused status=503`; `/readyz` 503 after the 24 h stale limit (or immediately if never fetched) | 503 absent and `tool_call ok` | Real traffic seen or block cannot be undone: restore the route, record time |
| D2 | Consent read fails | Needs the isolated-environment record. Remove the consent token file in that environment only. Without the record: production action, section 2.2 | `access refused` refusals; `tool_call refused` | `ok` after restore | Same |
| D3 | Postgres unreadable | Needs the isolated-environment record; the record's database only | Kill text to the client; `tool_call refused` when the middleware kill check fails; A3 discriminator absent today (record that gap) | `ok` after restore | Do not drill on the production database |
| D4 | Vault decrypt fails | Needs the isolated-environment record, which must name a Vault separate from production. Remove the role in that Vault only. Without the record: production action, section 2.2 | Client sees `unavailable`, never `not linked`; `tool_call outcome=error`, indistinguishable from other errors (section 3.2) | `ok` | Do not drill against production Vault |
| D5 | Proxy down | Needs the isolated-environment record, which must name an egress proxy separate from production (the production proxy is the mini's, section 4.1). Stop that isolated proxy only. Without the record: production action, section 2.2 | Client sees `unavailable`; `tool_call outcome=error`, which does not show proxy loss by itself (section 3.2); zero traffic from the pod to Glovo directly (NetworkPolicy counters, UNKNOWN) | `ok` | Any direct connection seen: stop, request an owner GO to kill, security review |
| D6 | Glovo 403 on refresh | Test double returning 403 | `needs_relink` with reason `glovo_rejected`; A7 | relink then `ok` | n/a |
| D7 | Kill flip and readback | Needs the isolated-environment record. Flip the deployment named in the record only, against a test double and the record's database. Each state reading is an explicit fixture observation of the test store or test double, labelled as such; the hosted control route is not used for reads | database `kill_on` audit record (fixture observation), `tool_call refused` within 5 s on that process | database `kill_off` audit record (fixture observation), `ok` | A `200 {"killed": true}` after an un-kill identifies a latch in that test process only; fleet state is not observed. A 503 `state_unknown` is UNKNOWN. Record both |
| D8 | Migration refusal | Test database with a different version | Exit 1 with `schema version is X, this build expects N` | n/a | no repair |
| D9 | Alert sample and clear | Offline only: send a clearly labeled synthetic test event to a local stub or an explicitly isolated test sink, never the existing operational alert path | Alert text captured by the stub or sink with no forbidden field | Clear message captured | Any forbidden field in the text: delete it, treat it as a template defect, owner decides |

D3 and D6 to D9 follow the same record and the approval rule above. Delivery
through the existing MCTL alert path (UNKNOWN) would have production effects,
is not a drill, and needs a prior explicit owner GO. It is not claimed to
exist.

## 12. Decisions and gaps for the owner

Open, not decided by this document:

1. Who holds which role (section 2.1) and whether a second reviewer is
   required.
2. Which backend serves the public host at each stage (section 2).
3. Whether the control route and the consent route are mounted in the
   deployed pod, and how mctl-api reaches the control route.
4. Whether an operator resume route is wanted (UNAVAILABLE). Unlink and
   erase routes exist; who calls them from mctl-api is open.
5. Whether readiness should cover database and kill switch (section 1.2).
6. The notification channel for expired sessions (`token-store.md` 4.5) and
   whether a per-user keepalive is wanted.
7. Thresholds A2, A5, A6 (no operator decision exists; numbers here are
   proposals).
8. A kill-vs-storage-outage discriminator for A3, a `reason` field on the
   `tool_call` line (section 3.2) and a read-only kill-state route (section
   8.1) are all new code, unbuilt and unapproved.
9. Whether schema rollback ever needs a defined path.
10. Whether, and where, an isolated environment exists for the section 11
    drills: a recorded owner decision naming an environment with its own
    Vault, egress proxy and database, none serving production. Until it is
    recorded, D1 to D9 are production actions under section 2.2.
11. Which image is deployed and whether it contains #53 and #54 (merged;
    deployed state UNKNOWN).

Unresolved security items are tracked in mctlhq/mctl-glovo#44 and not
repeated. Gate status and the stage sequence are in mctlhq/mctl-glovo#40.
Cutover is mctlhq/mctl-gitops#2777
(https://github.com/mctlhq/mctl-gitops/issues/2777).

## 13. Evidence used for this document

- Repository state: `origin/main` `aa9b3cc39cc7a8bbc834f04dfe841868a3ec65d6`.
  The changed-file list of `11009fd..aa9b3cc` was read. The changed files
  that this document cites (`cmd/mctl-glovo/{c2,mcp}.go`,
  `internal/glovoapi/{client,hosted}.go`,
  `internal/glovotools/backend.go`, `internal/linkflow/consent.go`) were
  re-read at the baseline and their line numbers updated. The cited files
  that are not in that list were not re-read in this revision; they are
  unchanged since the earlier baseline in which they were read.
  `internal/glovologic` was not read for this runbook: it changed in #53
  and no statement here depends on it.
- Files read: `CLAUDE.md`, `README.md`, `Dockerfile`,
  `deploy/mac-mini/README.md`, `deploy/mac-mini/glovo-alert`,
  `docs/design/real-glovo-accounts.md`, `docs/design/token-store.md`
  (sections 4-7), `cmd/mctl-glovo/{main,c2,c2caps,mcp,control,admit}.go`,
  `cmd/glovo-migrate/{main,migrate}.go` (header and version checks),
  `cmd/token-refresher/main.go`, `internal/health/health.go`,
  `internal/authn/{verifier,jwks}.go` (ready and staleness),
  `internal/gateway/gateway.go` (refusals),
  `internal/glovotools/hosted.go`, `internal/consentgate/gate.go` (TTL),
  `internal/tokenstore/{killswitch,errors,audit,caps,store,postgres}.go`
  (cited ranges), `internal/linkflow/linkflow.go` (routes),
  and for the stage sequence `docs/design/c2-readiness.md` and
  `docs/design/c2-readiness.json` at
  `11009fd61586ddb67b92a8e2e44d32d2dfa2774c` (planning artifacts; no GO).
- Not read, not run: cluster manifests, mctl-gitops, mctl-api, Vault,
  Postgres, Tailscale admin, the mini, MCTL monitoring.
- No test, linter or build was run for this document. No command quoted from
  a README was executed.
