# C2 offline acceptance plan (draft)

Status: draft for review. Documentation only. Nothing here was run, and no
result in this file is a live result. It grants no permission, implies no
rollback claim and enables nothing.

Related issues: mctlhq/mctl-glovo#43 (this plan), #40 (operator pre-GO),
#49 (scope decision), #50 (downstream), #57 (write canary), Draft #56
(fresh-login link, lead-owned, not duplicated here), mctlhq/mctl-gitops#2457
(M1), mctlhq/mctl-api#1011 and #1012 (basket approval). The content of those
issues was not re-read while writing this; their scope is taken from the task
statement and from `docs/design/c2-readiness.md` at the pinned revision.

Section 10 (G-3 rehearsal plan) was added against `origin/main` at
`fb8638a0947d32bef4c484c82868efb4fa35e393` (merge of #64), which postdates the
pin in section 1. Only the unlink and erase statements of section 10 were
checked at that revision.

## 1. Source revision and limits

All source references are pinned to `origin/main` at
`aa9b3cc39cc7a8bbc834f04dfe841868a3ec65d6` (merge of #54). This supersedes the
earlier pin `ca4a09aaf6b9b99f106177aee206726ff6b004b0`. The test inventory in
section 3 was re-checked by function name against that tree: every test named
below exists there. Between the two pins `cmd`, `internal` and `.github`
changed only through #53 and #54 (read-only hosted default, order-history
analysis). Neither changes a scenario row, and the read-only default adds
tests that are outside this plan's scope (`cmd/mctl-glovo/readonly_test.go`).
The operations-runbook branches (#46) and Draft #56 are separate work and
were not merged into, or re-verified against, this plan. Test names are the
stable anchor; line numbers are omitted on purpose because they drift.

CI configuration versus an observed run. These are different facts:

- Configuration (read from the pinned `.github/workflows/validate.yml`): the
  `test` job declares a `postgres:17` service, sets
  `TOKENSTORE_TEST_PG_DSN` to that service, and sets
  `TOKENSTORE_TEST_PG_REQUIRED: "1"`. It runs `go build ./...` and
  `go test -race -count=1 ./...`. The PG test harnesses
  (`internal/tokenstore/harness_test.go`, `cmd/mctl-glovo/control_test.go`,
  `cmd/glovo-migrate/migrate_test.go`) read these two variables, and with
  `TOKENSTORE_TEST_PG_REQUIRED` set a missing DSN fails instead of skipping.
- Observation: no CI run, job log or result was fetched or inspected for this
  document. Configuration shows what a run would require. It is not evidence
  that any run happened or passed.

Uncertainty, stated explicitly:

- Test files were inventoried by function name and by reading selected
  bodies. A name that matches a scenario is evidence that coverage was
  written, not that it passes. No test was run for this document.
- Tests marked PG skip without `TOKENSTORE_TEST_PG_DSN` in a local run. In
  the pinned CI configuration they cannot skip (see above), but that remains
  configuration, not an observed run.
- The issues named above were not readable from this checkout.
- Behaviour of the deployed image, Vault, mctl-api and the real Glovo API is
  unobserved. Nothing here speaks to it.

## 2. Evidence classes

| Class | Meaning | Counts as |
| --- | --- | --- |
| SR | Source review: a reader checked code at a pinned revision | Design evidence only |
| ST | Synthetic test: an automated test against fakes, run at a pinned revision | Offline acceptance, once run and recorded |
| LH | Live-hosted proof: observed behaviour of the deployed service | Not available; needs operator pre-GO under #40 |

Per scenario the status is one of: `source` (existing test, source-pinned),
`proposed` (no test yet, described here), or `unknown` (cannot be decided
from the source).

## 3. Scenario inventory

Packages: `cmd/mctl-glovo` (MCP surface), `internal/tokenstore` (store,
link flow, kill switch, isolation), `internal/consentgate`,
`internal/glovoapi` (client and egress). "mem" tests use the in-memory DB
double and need nothing external; "PG" tests need a local Postgres DSN and
skip otherwise.

### 3.1 Two-principal isolation

| Evidence | Where | Status |
| --- | --- | --- |
| Distinct MCP sessions; request carries only the caller's token; foreign session id refused with 404 and no lookup | `cmd/mctl-glovo` `TestMCPTwoPrincipalsNeverShareASession` | source (mem, fake Glovo) |
| Per-PID isolation incl. relink | `tokenstore` `TestPIDIsolationIncludingRelink` | source (mem) |
| Ciphertext swap never serves another user | `TestCiphertextSwapFailsToDecrypt` (mem), `TestIsoSwappedCiphertextsNeverServeAnotherUsersSession` (PG) | source |
| Every tool runs on the caller's tokens only; location per user; concurrent users with refresh; lookalike and malformed principals; refusals reveal nothing; other user's lifecycle, failed refresh, erasure, cap exhaustion and kill do not leak; audit scoped | `isolation_pg_test.go`, `TestIso*` (12 tests) | source (PG) |
| Admission cache per principal | `cmd/mctl-glovo` `TestAdmitPrincipalsNeverShareCache` | source (mem) |

Gap: the whole `TestIso*` suite, the only one that drives every tool against
an attributing fake Glovo, is PG-only. In mem-only runs, isolation is covered
at the MCP layer by one test whose fake Glovo returns `{}` and attributes only
by the Authorization header.

### 3.2 Link, relink, unlink, erase

| Evidence | Where | Status |
| --- | --- | --- |
| Link code flow, single use under concurrency, expiry, new code invalidates old, rate limits, session shape burns code, validation failure stores nothing, failed relink keeps old link | `linkflow_test.go` `TestLink*` | source |
| Version monotonic across relink; cache keyed on link id; refresh fenced by relink | `TestVersionMonotonicAcrossRelink`, `TestCacheChecksLinkIDNotOnlyVersion`, `TestRefreshLosesFenceToRelink` | source |
| Old-link ciphertext fails to decrypt | `TestOldLinkCiphertextFailsToDecrypt` | source |
| Erase pseudonymised in audit; audit insert-only and erasable | `TestEraseIsPseudonymisedInAudit`, `TestPostgresAuditIsInsertOnlyAndErasable` (PG) | source |
| One user's erasure leaves the other intact | `TestIsoErasureOfOneUserLeavesTheOtherIntact` (PG) | source (PG) |
| A not-linked principal gets a "not linked" refusal at the MCP surface | `cmd/mctl-glovo` `TestMCPAuditLine` (removes the principal from the session-source stub) | source (mem, stub only) |
| Owner-facing unlink, and a read after a real `Unlink` returning not-linked rather than a cached session | `TestLinkUnlinkRemovesTheSessionAndOnlyThat`, `TestLinkUnlinkWorksUnderKillAndWithoutConsentOrVault`, `TestLinkUnlinkNeedsAFreshLinkToken`, `TestLinkUnlinkCannotTargetAnotherPrincipal`, `TestLinkUnlinkFailureIsNeverASuccess` (linkflow over a real store, memDB), `cmd/glovo-link` `TestUnlink*` | source (mem) |
| Operator unlink and erase routes: identity gate, strict body, failure never ok, clean logs | `cmd/mctl-glovo` `TestControlAccount*` | source (stub store); live run proposed |

Fresh-login linking is Draft #56 and out of scope.

### 3.3 Consent revocation

| Evidence | Where | Status |
| --- | --- | --- |
| Positive answer cached at most `MaxTTL` (60 s); revocation takes effect within TTL | `consentgate` `TestPositiveCacheBoundsRevocation`, `cmd/mctl-glovo` `TestAdmitRevocationWithinTTL` | source (mem, fake clock) |
| "No" and failed read never cached; other consent version is no consent | `TestNegativesAndErrorsAreNotCached`, `TestOtherVersionIsNoConsent` | source |
| Upload re-checks consent; store re-checks consent | `TestLinkUploadRechecksConsent`, `TestC2StoreRechecksConsent` | source |
| Revocation observed end to end at the MCP surface after a session is open, with the next call refused and no Glovo request | not found | proposed |

### 3.4 Kill

| Evidence | Where | Status |
| --- | --- | --- |
| Killed blocks every hosted tool before any lookup; middleware never calls next | `TestKilledBlocksEveryHostedToolBeforeAnyLookup`, `TestKilledMiddlewareNeverCallsNext` | source (mem) |
| Unreadable or slow control row blocks calls | `TestUnreadableControlRowBlocksToolCalls`, `TestSlowControlReadBlocksToolCalls`, `TestUnreadableControlRowMeansKilled` | source |
| Zero decrypts while killed; only "running" cached; stale read does not outlive a flip | `TestKilledMeansZeroDecrypts`, `TestKillSwitchCachesOnlyRunning`, `TestKillSwitchStaleReadDoesNotOutliveAFlip` | source |
| Flip answers stored state; failure is never success; mismatched read-back is refused; unkill read-back failure re-kills; unkill while latched answers killed | `TestControl*`, `TestKillSwitchSet*`, `TestKillSwitchUnkill*` | source |
| Kill stops all users and resumes isolated | `TestIsoKillSwitchStopsAllAndResumesIsolated` (PG) | source (PG) |
| Link refused before redemption while killed | `TestLinkKillSwitchRefusesBeforeRedemption` | source |

Kill authority and the live drill belong to the operations runbook, not here.

### 3.5 Proxy and provider errors

| Evidence | Where | Status |
| --- | --- | --- |
| Proxy down fails closed; upstream never contacted directly; proxy required; all requests stay on base URL; redirects bounded | `glovoapi` `TestProxyDownFailsClosed`, `TestRequireProxyConstructorErrors`, `TestEveryRequestStaysOnBaseURL`, `TestRedirectsStayOnOriginAndAllowlist` | source |
| Fail-closed responses; error redaction; 401 retries exactly once; refresh failure is an error; non-array lists are errors | `TestFailClosedResponses`, `TestErrorRedaction`, `TestRefreshOn401RetriesExactlyOnce`, `TestRefreshFailureIsError`, `TestClientReadsFailOnNonArrayLists` | source |
| Mutation allowlist | `TestMutationAllowlist` | source |
| Provider error surfaced through the MCP layer for one principal while the other is unaffected | `TestIsoFailedRefreshOfOneUserDoesNotTouchTheOther` (PG) covers refresh only; MCP-level provider 5xx and proxy-down per principal not found | proposed |

### 3.6 DB, Vault and consent dependency failures

| Evidence | Where | Status |
| --- | --- | --- |
| DB read failure is unavailable, not "not linked"; cache not served on failed read | `TestReadFailureIsUnavailableNotNotLinked` (mem fault injection) | source |
| Decrypt needs the audit row; commit outcome unknown resolved | `TestDecryptNeedsAuditRow`, `TestCommitOutcomeUnknownResolved` | source |
| Vault: address must be in cluster; no redirects; errors carry no bodies; Kubernetes login failure not cached | `transit_test.go`, `k8slogin_test.go` | source |
| Vault unavailable at decrypt time mapped to a refusal at the MCP surface | not found | proposed |
| Consent source error and timeout refuse, with no detail | `TestAdmitEveryFailureClassRefuses`, `TestAdmitTimeoutAndTransportRefuse`, `TestAdmitRefusalCarriesNoDetail`, `TestMCPCallAdmitterFailsClosed` | source |

### 3.7 Recovery

| Evidence | Where | Status |
| --- | --- | --- |
| Kill then resume restores service per user | `TestIsoKillSwitchStopsAllAndResumesIsolated` (PG), `TestKillSwitchSetPersistsAcrossRestart` | source |
| Errors and refusals not cached, so the next request re-reads | `consentgate`, `TestKubernetesLoginFailureIsNotCached` | source |
| Recovery after a dependency returns (DB, consent, provider), shown at the MCP surface with both principals | not found | proposed |
| Restart of the deployed pod and fail-closed boot | boot refusals are tested (`TestC2OnRefusesBadDependencies`); a real restart is not an offline fact | unknown (LH) |

## 4. M1 versus hosted C2 acceptance

mctl-gitops#2457 (M1, per the task statement) is a synthetic basket with human
approval: the owner approves or rejects a basket on the mctl-api approval
page. That proves the basket path and the human decision. It does not prove
C2, which differs in kind:

| Aspect | M1 (#2457) | Hosted C2 acceptance |
| --- | --- | --- |
| Data | synthetic basket, never a real provider | per-principal linked session held encrypted in the store |
| Decision | human approval in mctl-api | none; C2 tools act within the linked account's read scope |
| Identity | mctl-api owns the decision | this service owns isolation, consent re-check and kill |
| Failure modes | approval ledger | DB, Vault, consent source, proxy, provider |
| Provider | none | real Glovo API through the egress proxy (live only) |

Passing M1 says nothing about C2 isolation, link lifecycle, kill or
dependency failure, and the reverse also holds. C2 acceptance does not
replace the human approval rule and never asserts a human decision.

## 5. Smallest next offline harness slice (proposed only)

Not implemented. Not run. No packages, services or network.

Goal: close the gaps marked `proposed` at the MCP surface with in-memory
fakes only, reusing the existing `newMCPEnvWith` test environment in
`cmd/mctl-glovo` (two synthetic principals `prn_A`, `prn_B`, tokens
`tok-a`, `tok-b`, injectable admit, kill and clock).

Fixtures (all synthetic, invented in the test):

- F1 fake Glovo that attributes each request to the owner of the
  Authorization token and can be told, per owner, to answer 200, 401, 5xx,
  or to be unreachable.
- F2 admit stub with a switchable consent state per principal (granted,
  revoked, error, timeout).
- F3 kill stub (running, killed, unreadable).
- F4 session source stub that can return not-linked or unavailable per
  principal.
- F5 fake clock to step across the consent TTL.

Passing control (must pass first): both principals call a read tool, each
request reaches F1 with only its own token, lookups are `[prn_A prn_B]`, and
no refusal text appears. If the control fails, no negative result counts.

Negative checks (each must refuse, name no other principal, reach F1 zero
times unless stated, and leave the other principal at the control result):

1. Revoke consent for A: A refused after TTL, B unchanged.
2. Consent source error for A: A refused, not cached, A recovers on next call
   once cleared.
3. Kill on: both refused before any lookup; kill off: both recover.
4. Unreadable kill row: both refused.
5. A not linked / unlinked: A gets a not-linked refusal, B unchanged.
6. Session source unavailable for A: unavailable, not "not linked".
7. F1 answers 5xx and unreachable for A only: A gets a redacted error, B
   unchanged, A recovers when F1 recovers.

Expected outcome recorded per check: `refused` or `served`, number of F1
requests, and the other principal's outcome. Open question for the
reviewer: whether to assert refusal text equality (existing tests do for the
killed text) or only `IsError`.

PG-only items (the full `TestIso*` suite) are not part of this slice. Running
them needs a local Postgres DSN, which is a separate prerequisite and is not
requested here.

## 6. Evidence template

One row per scenario per revision. Copy, do not merge classes.

```
scenario:        <3.x name>
class:           SR | ST | LH
source revision: <full 40-char SHA of mctl-glovo>
dependency revs: <full SHAs or "none">; mctl-gitops: <SHA or "n/a">
artifact:        <test name or doc section; for ST the exact command>
expected:        <refused|served|state, counts>
observed:        <filled only after a real run; "not run" otherwise>
result:          pass | fail | not run
control passed:  yes | no | n/a
prerequisites:   <what is still missing>
recorded by / date: <role, date>
```

Rules: an ST row without a recorded run is `not run`. An LH row needs an
operator pre-GO under #40 first. A result at one SHA is not evidence for
another.

## 7. Owner-only read-only trial (separate record)

Unavailable until explicit operator pre-GO under #40. This plan does not
supply that GO and implies no permission to start it. The live trial is the
later G1 result of #43, not part of this plan.

Evidence the trial would need, recorded separately from sections 3 and 5:

- the written operator pre-GO, with scope and time window;
- the exact deployed image tag and digest and the source SHA it was built
  from;
- the owner as the only principal, read-only tools only, no write or basket
  mutation;
- the kill state before and after, and who may kill;
- audit lines for each call (pseudonymous who, tool, outcome);
- the outcome of each call, including refusals and errors, unedited;
- explicit statements of what was not observed.

No live result exists. No rollback claim is made. Operating steps stay in the
operations runbook.

## 8. Gate mapping

Terms follow `docs/design/c2-readiness.md` at the pinned revision.

| Item | What it is | Relation to this plan |
| --- | --- | --- |
| #49 | Records the scope decision (G0 topic `owner-decision-49`) | This plan does not make or imply it |
| #40 | Where owner-only pre-GO is recorded (G0) | This plan is offline readiness input to it, not the GO |
| #43 / G1 | The later live owner-only read-only results | Not this plan. Needs the #40 pre-GO first |
| #50 / G2 | Two-user read-only admission | Needs confirmed #43 / G1 live results |

This plan maps to G0 offline readiness only. Synthetic passes (ST rows) are
offline evidence. They do not satisfy G1 and they do not satisfy #50. #50
depends on confirmed live G1 results, which do not exist. A plan, a proposal,
source review or a synthetic pass alone does not confirm anything.

Separate owner GOs, none implied by this plan, by an ST pass or by another
GO:

- the live E2E (the #40 pre-GO, then G1);
- each admission stage on its own: 2, then 10, then 20 users;
- the #57 write canary, which belongs to G4 only.

Basket writes may only go through the approval defined by
mctlhq/mctl-api#1011 and #1012, with no second approval ledger in this
service. Their status was not fetched here. Checkout, payment and
`place_order` stay forbidden at every stage.

## 9. Not in this plan

No harness implementation, test runs, drills, live access or publication. No
checkout, payment or order tool, no real identifiers, payloads or
credentials, no flag, ingress, Vault or runtime change.

## 10. G-3 rehearsal plan: live unlink and erase evidence (proposed)

Status: PROPOSED. Nothing in this section was run, and it is not a GO. "G-3
rehearsal" is this plan's name for the live exit rehearsal. It is not the
readiness gate G3 (ten-user admission, #51).

### 10.1 What the source does at `fb8638a0947d32bef4c484c82868efb4fa35e393`

CURRENT in the repository (merge of #64). Deployed state is UNKNOWN: whether
an image with #64 runs, whether the control endpoint is configured, and
whether either route is reachable are all unobserved.

| Route | Where | Authorization | Evidence it leaves |
| --- | --- | --- | --- |
| `POST /link/unlink` (user), reached with `glovo-link -unlink` | `internal/linkflow/linkflow.go` `handleUnlink`; `cmd/glovo-link/main.go` `runUnlink` | A fresh MCTL login with the link audience, the same as an upload. The principal is the verified one, never a body field. No consent, code or Vault is needed, and the kill switch does not block it. | `200 {"result":"unlinked"}`; helper prints `unlinked`; `link` log line `route=unlink outcome=unlinked`; store audit event `unlink` with actor user and reason `user_request` |
| `POST /internal/v1/control/unlink` (operator) | `cmd/mctl-glovo/control.go` `handleAccount(false)`; `internal/tokenstore/store.go` `OperatorUnlink` | Machine-user token on the control subject allowlist, no proxy forwarding headers (404 otherwise), strict body `{"principal": "prn_..."}`. Mounted only if the control endpoint is configured (`cmd/mctl-glovo/c2.go` `buildControl`). | `200 {"result":"unlinked"}`; audit line `account_unlink outcome=ok`; store audit event `unlink`, actor operator, reason `operator_request`. Failure is `503 state_unknown` plus `outcome=unknown`, never a 200. |
| `POST /internal/v1/control/erase` (operator) | `control.go` `handleAccount(true)`; `store.go` `Erase` | Same as operator unlink | `200 {"result":"erased"}`; audit line `account_erase outcome=ok`; unlink and `erasure` audit events and subject pseudonymisation in one transaction |

Limits of that evidence:

- Coverage is source and synthetic tests only: `TestControlAccount*` (stub
  store, `cmd/mctl-glovo`), `TestLinkUnlink*` (real store over the in-memory
  DB double, `internal/tokenstore`) and `TestUnlink*` (`cmd/glovo-link`).
  Nothing has run against a deployed service, a real Postgres, Vault or
  mctl-api.
- The source has no caller of the operator routes. Whether mctl-api has one
  was not read here and is UNKNOWN. Without a caller, the operator route
  needs a human holding a valid machine-user token, which is an owner decision.
- There is no read route for link state. After a call, the only observation
  is the response, the audit lines and a following `tool_call` that answers
  "not linked". That follow-up is itself a live call and needs its own GO.
- A backup may still hold ciphertext (`token-store.md` 4.4). The rehearsal
  must not claim crypto-erasure.

### 10.2 Order rule

The first live link of any account needs prior live unlink and erase
rehearsal evidence recorded under #43. Rules:

1. At most two owner-authorized test accounts, never a participant or user
   account. Not named or identified in any record (use `test-A`, `test-B`).
2. Every effect below needs its own explicit owner GO recorded before it
   happens: each link, each tool call, each unlink, each erase, each kill or
   un-kill. A GO for one never covers another (`c2-operations.md` section
   2.2). This plan is not a GO.
3. Read-only tools only. No basket write, checkout, payment or order.
4. No flag, Vault, deploy or release change is part of this plan.

### 10.3 Blocker for owner decision: no linked test account may exist

Observing an unlink or an erase needs a linked account to remove, but the
order rule forbids the first live link until that evidence exists. If no
suitably linked test account already exists, the rehearsal cannot start and
this is a BLOCKER for the owner. It is not solved here and must not be solved
by linking live. Options for the owner to decide (none is chosen or implied):

- name an existing owner-authorized account that is already linked, with the
  evidence that it is;
- grant a recorded, explicitly scoped exception to the order rule for one
  test account (a link and its immediate unlink, in one window);
- accept an isolated environment (own Vault, proxy and database, none serving
  production, `c2-operations.md` section 11) for the rehearsal, which then
  gives synthetic evidence only and does not satisfy the live evidence
  requirement.

Whether a linked test account exists is UNKNOWN.

### 10.4 Proposed preconditions

- Written owner GO per effect (10.2), with scope and time window.
- Deployed image tag and digest and the source SHA, recorded, containing #64.
  If unknown, record UNKNOWN and stop.
- Owner decision on which backend serves the public host (`c2-operations.md`
  section 2).
- Control endpoint confirmed configured for the operator route. If not, only
  the user route can be rehearsed and the operator route stays UNKNOWN.
- Kill state recorded as UNKNOWN or as a dated non-mutating observation. The
  kill route has no read.
- A named holder of the machine-user token and a named person who operates
  `glovo-link -unlink`. The two test accounts are consenting owner-controlled
  accounts.
- Where records are kept. No participant data, identifier, token, link code,
  principal id or Glovo account id enters this repository or an issue.

### 10.5 Proposed sequence and expected evidence

For each test account, in order, each step behind its own GO. Stop at the
first unexpected result.

| Step | Effect | Expected evidence | Readback limit |
| --- | --- | --- | --- |
| R1 | User unlink of `test-A` with `glovo-link -unlink` | helper prints `unlinked`; `200 result unlinked`; `link` line `route=unlink outcome=unlinked`; `unlink` audit event, actor user | `200` is the answering process's result. Other replicas, in-flight calls and backups are not observed. |
| R2 | One read tool call as `test-A` after R1 | "not linked" refusal, `tool_call outcome=error`, no Glovo request | Needs its own GO. A "not linked" text is only valid when the database read succeeded; `unavailable` is a different answer. |
| R3 | Operator unlink of `test-B` | `200 result unlinked`; `account_unlink outcome=ok`; `unlink` audit event, actor operator | Same as R1. |
| R4 | Operator erase of `test-B` (after R3 or alone) | `200 result erased`; `account_erase outcome=ok`; `unlink` and `erasure` events | Pseudonymisation is observable only through audit rows the owner may read. Absence of a log line is not proof. |
| R5 | One read tool call as `test-B` after R4 | "not linked" refusal | As R2. |
| R6 | Repeat R3 on an already unlinked account | Recorded unedited. The result for an already absent link was not read in source for this document; the live result is UNKNOWN. | Do not retry in a loop. |

Each step is recorded with the section 6 template, class LH, the full source
SHA, "not run" until a real run exists. The control and `account_*` log
fields carry no principal, so tie a step to the account by the recorded GO
and the time window, not by an identifier in the record.

### 10.6 Aborts

Stop, do not retry in a loop, do not use SQL and ask the owner if:

- any response is `503 state_unknown`, a non-200 or a refusal;
- the answer to R2 or R5 is anything other than "not linked", or a Glovo
  request is observed;
- a real user's data, principal or session appears;
- a checkout, payment or order surface appears;
- the control route answers 404 (not mounted or proxy headers present): the
  operator route stays UNKNOWN and is not worked around;
- the image or source SHA cannot be established;
- the owner withdraws a GO.

### 10.7 Contingency

- A user unlink that fails leaves the link state UNKNOWN. Do not assume
  removal. The operator route is a separate effect with its own GO.
- An operator failure, or a missing control endpoint, leaves the operator
  route UNKNOWN. That blocks the "exit proven" condition for the cohort
  (`c2-pilot-feedback.md` rule 6) until the owner decides.
- A partial result (unlink without erase evidence) is recorded as partial and
  does not meet the #43 requirement.
- Kill is not part of this plan. If one is needed it has its own GO and its
  own `c2-operations.md` row.
- If an account is left linked after a failure, it remains an owner-authorized
  test account and is not invited to anything else.

### 10.8 What this does not do

It starts no rehearsal and links nothing. It does not meet the #43 live
requirement, grants no GO, and does not satisfy the launch condition of
`c2-pilot-feedback.md` or the first-live-link gate of `c2-operations.md`.
Cross references: `c2-operations.md` section 6.5, `c2-pilot-feedback.md` rule
6.
