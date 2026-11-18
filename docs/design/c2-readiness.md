# C2 readiness matrix (G0 to G4)

Status: PLANNING ARTIFACT. Nothing here authorizes anything. It is not an
authorization engine or a runtime controller, and no code reads it. It does
not satisfy issue #40 on its own.

Source of truth for the data is `c2-readiness.json`. A test in this package
fails if the table below drifts from it.

Baseline: `main` at `ca4a09aaf6b9b99f106177aee206726ff6b004b0`
(https://github.com/mctlhq/mctl-glovo/commit/ca4a09aaf6b9b99f106177aee206726ff6b004b0).
Issue: https://github.com/mctlhq/mctl-glovo/issues/40.

## 1. Rules

- Every live value that has not been verified is `UNKNOWN`. No gate is PASS.
- Missing, aged, ambiguous or unobservable evidence cannot produce PASS.
- The owner pre-GO to perform the E2E (G0) is a decision. The later live
  results (G1) are evidence. One never stands in for the other.
- The owner, two-user, ten-user and twenty-user decisions are separate
  (G1, G2, G3, G4). Each has its own decision id. A PASS at one stage never
  authorizes another.
- #50 and #51 are separate required decisions. #52 is optional and is never
  implied by them. Their stage mapping is UNKNOWN.
- G1 to G3 are read-only and do not depend on #55 or #57. G4 (twenty-user
  writes) requires the existing #1011 and #1012 proof and a separately
  authorized canary.
- The order, checkout and payment surfaces are forbidden at every stage.
- The stage split above is a proposal for the lead and owner to confirm.

## 2. Evidence freshness (proposals, not approved)

No evidence TTL is owner-approved, so there is no effective TTL. The rules
below are proposals only:

- P1: evidence needs a full 40-hex SHA and an https evidence URL.
- P2: evidence needs an `observed_at` timestamp (RFC 3339, UTC).
- P3: evidence older than an owner-approved maximum age is stale. The value
  is not set here.
- P4: evidence observed after the reference time is ambiguous and refused.
- P5: missing, aged, ambiguous or unobservable evidence yields UNKNOWN.

Tests use a fixed reference time (2026-11-18T12:00:00Z) and synthetic
fixtures. They use a synthetic maximum age only to exercise the refusal
paths. They do not mark any live gate satisfied.

## 3. Matrix

<!-- BEGIN MATRIX -->
| Gate | Status | Role | Phase | Evidence SHA | Evidence URL | Freshness | Rollback |
|---|---|---|---|---|---|---|---|
| G0 | UNKNOWN | owner | pre-go | UNKNOWN | UNKNOWN | UNKNOWN | Withdraw the pre-GO; no E2E starts. Nothing to undo because nothing ran. |
| G1 | UNKNOWN | owner | owner-read-only | UNKNOWN | UNKNOWN | UNKNOWN | Kill switch and credential revocation. Exact procedure UNKNOWN. |
| G2 | UNKNOWN | owner | two-user-read-only | UNKNOWN | UNKNOWN | UNKNOWN | Remove both users from the allowlist and revoke their tokens. Exact procedure UNKNOWN. |
| G3 | UNKNOWN | owner | ten-user-read-only | UNKNOWN | UNKNOWN | UNKNOWN | Return to the two-user allowlist. Exact procedure UNKNOWN. |
| G4 | UNKNOWN | owner | twenty-user-write | UNKNOWN | UNKNOWN | UNKNOWN | Disable basket writes, return to the ten-user read-only allowlist. Exact procedure UNKNOWN. |

| Topic | Status | Gates |
|---|---|---|
| #2777 cutover | UNKNOWN | G1 |
| Rollback and kill switch | UNKNOWN | G1, G2, G3, G4 |
| OAuth | UNKNOWN | G1 |
| Consent versions | UNKNOWN | G2 |
| Scopes | UNKNOWN | G1 |
| Vault and control identity | UNKNOWN | G1 |
| Proxy | UNKNOWN | G1 |
| Migrations | UNKNOWN | G1 |
| Image pins | UNKNOWN | G1 |
| Capacity | UNKNOWN | G3, G4 |
| Existing glovo-home writes | UNKNOWN | G1, G4 |
| Isolation | UNKNOWN | G2, G3 |
<!-- END MATRIX -->

## 4. Authority and follow-ups

- Section 9.4 of the design and its unresolved items 1 to 3 belong to #44
  (https://github.com/mctlhq/mctl-glovo/issues/44, same-repository number
  assumed, lead to confirm). This document links that verdict and does not
  restate or override it.
- Correcting the text of #11 is a lead-owned follow-up. It is not done here
  and no GitHub issue is edited from this artifact.
- Existing glovo-home basket writes are tracked as a topic, not as an
  approval for any user stage. glovo-home never gains an order, checkout or
  payment tool.
