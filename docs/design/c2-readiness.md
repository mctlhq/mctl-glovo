# C2 readiness matrix (G0 to G4)

Status: PLANNING ARTIFACT. Nothing here authorizes anything. It is not an
authorization engine or a runtime controller, and no code reads it. It does
not satisfy issue #40 on its own.

Source of truth for the data is `c2-readiness.json`. A test in this package
fails if the tables below drift from it.

Baseline: `main` at `ca4a09aaf6b9b99f106177aee206726ff6b004b0`
(https://github.com/mctlhq/mctl-glovo/commit/ca4a09aaf6b9b99f106177aee206726ff6b004b0).
Issue: https://github.com/mctlhq/mctl-glovo/issues/40.

## 1. Rules

- Every live value that has not been verified is `UNKNOWN`. No gate is PASS.
  Known issue references are not proof of deployed behavior.
- Missing, aged, ambiguous or unobservable evidence, including an UNKNOWN
  freshness state, cannot produce PASS.
- The owner decision is G0, the pre-GO decision to perform the E2E. G1 is the
  later live read-only results (#43), not a separate owner admission decision.
  The results do not substitute for the decision.
- G0 requires OFFLINE readiness proof (static or synthetic) for its pre-GO
  prerequisites: the topics listed in the matrix with earliest gate G0 (#49
  decision, #41 disabled-write proof, #42 linking, mctl-gitops#2777 cutover
  plan, #46 operations, the readiness externals, and the OAuth, scopes,
  glovo-home safety, consent versions, isolation and capacity safeguards).
  These safeguards are needed for the first owner-only live E2E, so they sit at
  G0. G0 never requires their future LIVE results, nor live G1 or #43 results,
  so there is no cycle. Live result evidence only arrives after G0, at G1.
- The same readiness requirements carry forward to every later applicable
  phase. Capacity must be bounded for owner-only, 2, 10 and 20 users.
- Nothing here marks a condition PASS or authorizes opening any flag.
- Operator-recorded stage sequence: Owner-only, 10 read-only, Approval+Canary,
  20 read+write. This is a recorded requirement, not a proposal.
- The owner pre-GO, two-user, ten-user and twenty-user decisions are separate
  (G0, G2, G3, G4); G1 holds results. Each decision has its own decision id. A PASS at one stage
  never authorizes another.
- #50 maps to G2 (two users), #51 to G3 (ten users) and #52 to optional G4
  (twenty users). They are separate decisions and none implies another.
- G0 to G3 are read-only and do not depend on #55 or #57. G4 (twenty-user
  writes) depends on G3, #55 and #57, requires the existing #1011 and #1012
  proof, and needs a separately authorized canary.
- Requirements carry forward to later phases through the "Carried to" column
  without adding #55 or #57 to any phase up to G3.
- The order, checkout and payment surfaces are forbidden at every stage.

## 2. Evidence freshness (proposals, not approved)

No evidence TTL is owner-approved, so there is no effective TTL. The rules
below are proposals only:

- P1: evidence needs a full 40-hex SHA and an https evidence URL with a
  non-empty host.
- P2: evidence needs an `observed_at` timestamp (RFC 3339, UTC) and a
  freshness state of FRESH.
- P3: evidence older than an owner-approved maximum age is stale. The value
  is not set here.
- P4: evidence observed after the reference time is ambiguous and refused.
- P5: missing, aged, ambiguous or unobservable evidence yields UNKNOWN.

Tests use a fixed reference time (2026-11-18T12:00:00Z) and synthetic
fixtures. They use a synthetic maximum age only to exercise the refusal
paths. These are offline syntax and state checks: no evidence URL is fetched
and no valid operational GO exists.

## 3. Baseline facts and unknowns

Verified code facts are limited to these source paths existing at the baseline
SHA. A path existing is not proof of deployed or runtime behavior:

- `cmd/glovo-home/main.go` (separate legacy glovo-home safety inventory only)
- `cmd/mctl-glovo/mcp.go` (hosted plane)
- `internal/glovotools/hosted.go` (hosted plane)
- `cmd/glovo-migrate/main.go`
- `internal/authn/verifier.go`
- `internal/consentgate/gate.go`
- `internal/linkflow/consent.go`
- `internal/tokenstore/killswitch.go`
- `internal/tokenstore/migrations/0001_tokenstore.sql`
- `internal/tokenstore/isolation_pg_test.go`
- `deploy/mac-mini/tinyproxy.conf`
- `Dockerfile`

Open Draft PRs, separate from the baseline and not merged:

- #54 (head `a2b20b65`)
- #56 (head `c1179519`)
- #53 (head `a3af531c`)

Their content is not asserted here. The existing main baseline does not prove
PR #54 behavior: disabled-write behavior is UNKNOWN pending reviewed proof. A
source path existing is a pin, not proof of behavior.

Runtime unknowns, all UNKNOWN until evidence exists:

- deployed image digest
- chart pin
- Vault paths and policies
- database and migration state
- proxy egress state
- kill switch state
- consent-reader and control identity
- deploy flag value

## 4. Matrix

<!-- BEGIN MATRIX -->
| Ref | Required | Stage | Note |
|---|---|---|---|
| #50 | yes | G2 | Two-user admission. Separate from #51 and #52. |
| #51 | yes | G3 | Ten-user admission. Separate from #50 and #52. |
| #52 | no | G4 | Optional twenty-user. Never implied by #50 or #51. |

| Gate | Status | Role | Phase | Depends on | Prerequisites | Evidence SHA | Evidence URL | Freshness | Rollback |
|---|---|---|---|---|---|---|---|---|---|
| G0 | UNKNOWN | owner | pre-go | - | owner-decision-49, disabled-write-proof-41, linking-42, cutover-2777, operations-46, deploy-false, vault-control-identity, proxy, migrations, image-pins, rollback-kill, oauth, scopes, glovo-home-writes, consent-versions, isolation, capacity | UNKNOWN | UNKNOWN | UNKNOWN | Withdraw the pre-GO; no E2E starts. Nothing to undo because nothing ran. |
| G1 | UNKNOWN | owner | owner-read-only | G0 | - | UNKNOWN | UNKNOWN | UNKNOWN | Kill switch and credential revocation. Exact procedure UNKNOWN. |
| G2 | UNKNOWN | owner | two-user-read-only | G1 | - | UNKNOWN | UNKNOWN | UNKNOWN | Remove both users from the allowlist and revoke their tokens. Exact procedure UNKNOWN. |
| G3 | UNKNOWN | owner | ten-user-read-only | G2 | - | UNKNOWN | UNKNOWN | UNKNOWN | Return to the two-user allowlist. Exact procedure UNKNOWN. |
| G4 | UNKNOWN | owner | twenty-user-write | G3, #55, #57 | - | UNKNOWN | UNKNOWN | UNKNOWN | Disable basket writes, return to the ten-user read-only allowlist. Exact procedure UNKNOWN. |

| Topic | Role | Refs | Earliest gate | Carried to | Status | Evidence SHA | Evidence URL | Freshness | Rollback |
|---|---|---|---|---|---|---|---|---|---|
| Owner decision #49 | owner | #49 | G0 | - | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN | Withdraw the decision. Exact procedure UNKNOWN. |
| Disabled-write proof #41 | service-maintainer | #41, #54 (draft, behavior UNKNOWN) | G0 | G1, G2, G3, G4 | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN | Keep writes disabled. Exact procedure UNKNOWN. |
| Linking #42 | service-maintainer | #42 | G0 | G1, G2, G3, G4 | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN | Unlink the owner account. Exact procedure UNKNOWN. |
| mctl-gitops#2777 cutover plan | platform-operator | mctl-gitops#2777 | G0 | G1 | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN | Revert the cutover change. Exact procedure UNKNOWN. |
| Operations #46 | platform-operator | #46 | G0 | G1, G2, G3, G4 | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN |
| deploy:false state | platform-operator | UNKNOWN | G0 | G1 | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN |
| Vault and consent-reader/control identity | platform-operator | UNKNOWN | G0 | G1, G2, G3, G4 | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN | Revoke the identity. Exact procedure UNKNOWN. |
| Proxy | platform-operator | UNKNOWN | G0 | G1, G2, G3, G4 | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN | Remove proxy egress. Exact procedure UNKNOWN. |
| Database and migrations | service-maintainer | UNKNOWN | G0 | G1, G2, G3, G4 | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN |
| Image and chart pins | platform-operator | UNKNOWN | G0 | G1, G2, G3, G4 | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN | Revert to the previous pins. Exact pins UNKNOWN. |
| Rollback and kill switch readiness | platform-operator | UNKNOWN | G0 | G1, G2, G3, G4 | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN | This is the rollback mechanism. Exact procedure UNKNOWN. |
| OAuth | service-maintainer | UNKNOWN | G0 | G1, G2, G3, G4 | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN |
| Scopes | service-maintainer | UNKNOWN | G0 | G1, G2, G3, G4 | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN |
| Existing glovo-home writes | service-maintainer | #41 | G0 | G1, G2, G3, G4 | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN | Disable basket writes. Exact procedure UNKNOWN. |
| Consent versions | service-maintainer | UNKNOWN | G0 | G1, G2, G3, G4 | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN |
| Isolation | service-maintainer | UNKNOWN | G0 | G1, G2, G3, G4 | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN |
| Capacity | platform-operator | UNKNOWN | G0 | G1, G2, G3, G4 | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN |
<!-- END MATRIX -->

## 5. Authority and follow-ups

- Section 9.4 of `docs/design/token-store.md` and its unresolved items 1 to 3
  belong solely to #44 (https://github.com/mctlhq/mctl-glovo/issues/44,
  verified). That verdict is linked here once. This document does not assume,
  restate, replace or override it.
- Correcting the text of #11 is a lead-owned follow-up. It is not done here
  and no GitHub issue is edited from this artifact.
- Existing glovo-home basket writes are tracked as a topic, not as an
  approval for any user stage. glovo-home never gains an order, checkout or
  payment tool.
- Role names are planning labels, not assignments of people.
