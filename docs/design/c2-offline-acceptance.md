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
