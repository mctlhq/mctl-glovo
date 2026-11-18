# C2 pilot feedback protocol (draft)

Status: DRAFT, proposal only. Documentation for mctlhq/mctl-glovo#47. It
grants no permission, records no GO, enrolls nobody, collects no data and
enables nothing. Every threshold, count, date and owner decision below is
PENDING. Nothing here was run, and no result in this file is a live result.

Related issues, scope taken from existing repository documents and not
re-read from GitHub while writing this: #47 (cohort feedback, this document),
#40 (gate and stage record), #43 (offline acceptance and the later live
owner-only read-only results, G1), #46 (operations runbook), #50 (two-user
admission, G2), #51 (ten-user admission, G3), #44 and #45 (dependencies of
G3, never restated here).

Sources, all in this repository at `origin/main`
`65582474f878b29011f83d6604e4ac8e34340e4e` (merge of #56):
`docs/design/c2-readiness.md`, `docs/design/c2-offline-acceptance.md`,
`docs/design/c2-operations.md`. Section names below are stable anchors;
line numbers are omitted because they drift.

## 1. Scope and limits

- Offline protocol only. It describes what would be asked and recorded if
  and when the owner decides to run a rehearsal or a wave.
- No participant is contacted, no account is linked, no service is called,
  no flag is touched and nothing is deployed or published by this document.
- The stage sequence is the one recorded in #40 (see `c2-readiness.md`
  section 1): the rehearsal maps to G2 (#50), the 10-user wave to G3 (#51).
  Both are read-only. Basket writes, #55 and #57 are not part of either.
- Checkout, payment, order and `place_order` surfaces are forbidden at every
  stage. This protocol never asks a participant to try one.
- Merged is not deployed. What the hosted service does today is UNKNOWN
  until live evidence exists (`c2-operations.md` section 1.3).

## 2. Opt-in and conditionality

Two separate groups, each with its own gate. No participation in one implies
anything for the other.

| Group | Size | Gate | Issue | Required before any invitation |
| --- | --- | --- | --- | --- |
| Rehearsal | at most 2 accounts (the G2 limit in #50) | G2, its own recorded GO | #50 | recorded GO, recorded capacity for the stage, G1 results confirmed under #43, exit path proven (rule 6) |
| Wave | 10 people | G3, its own recorded GO | #51 | recorded GO, recorded capacity for the stage, G2 confirmed, #44 and #45 dependencies as #40 records, exit path proven (rule 6) |

Rules:

1. A group is not invited until its own GO and its own capacity record
   exist. A GO for the rehearsal is not a GO for the wave. A capacity record
   for one stage does not cover another (`c2-readiness.md` section 1).
2. Where a GO or capacity record is missing, aged or unreadable, the answer
   is "not started". It is never assumed.
3. Participation is opt-in, in writing, and revocable at any time without
   giving a reason. No one is enrolled by default, by role or by silence.
4. The rehearsal and the wave use the same questions, so the two can be
   compared in aggregate. A change of questions between them is recorded.
5. Who is eligible, how many are invited, the invitation wording and the
   channel are PENDING (owner decision). The rehearsal never exceeds the
   account limit recorded for G2 in #50 (two); issue #47 speaks of 2 to 3
   participants, and the third is not admitted unless #50 raises its limit.
6. Launch condition: before any invitation, a participant-usable exit is
   proven for that group, that is revoking consent plus a working unlink and
   erase path, with evidence recorded under #43. At this baseline no
   operator or user unlink or erase route exists in this repository
   (`c2-operations.md` sections 6.2 to 6.4), so this condition is NOT met
   and no group may be invited until it is. Any pilot check (T1 to T8) run
   in a rehearsal is read-only and uses the same exit path.

What a participant is told before opting in (wording PENDING, content
required):

- the service reads their Glovo account history and basket and nothing else;
- in this trial it cannot change a basket, and it cannot order or pay;
- what is recorded about them (section 6), and that individual answers are
  never published;
- how to leave (section 7), and that leaving ends collection;
- linking requires their own consent version as the repository defines it
  (`c2-link-v2`, `c2-operations.md` section 1.3).

## 3. What each participant is asked to try

Each item is an observation, not a test of the person. Tasks are asked
verbally or in a form (PENDING), one at a time. The participant may skip any.

| # | Area | What is observed | Reference |
| --- | --- | --- | --- |
| T1 | Link completion | Whether the participant reaches a linked state on their own, where they stopped if not, and whether the consent text was understood | `/link` routes, `c2-operations.md` section 6.1 |
| T2 | Relink and exit | Whether they can relink after the session expires (`needs_relink` text), and whether they can find how to stop and leave | `c2-operations.md` sections 3.2 and 6.1 |
| T3 | Useful history answers | Whether an answer about their own ordering history was useful, in their own judgement | order intelligence, #45 |
| T4 | Uncertain history answers | Whether an answer that expressed doubt was understood as doubt, and whether it was still acted on | #45 |
| T5 | Basket reading | Whether they can ask what is in the basket and get an answer they recognise as theirs | read tools |
| T6 | Mutation refused in read-only | When they ask to add, change or remove an item, whether the refusal `Basket changes are not enabled in this deployment. Basket tools are read-only here.` was understood, and whether they expected it | `c2-operations.md` section 3.2, `basket_writes_off` |
| T7 | Latency | How long answers felt, per task: acceptable, slow, or gave up | see section 4 |
| T8 | Safe refusal comprehension | For each fixed refusal they happen to meet (not linked, access switched off, suspended, busy, rate limited), whether they understood what to do next | `c2-operations.md` section 3.2 |

T6 expects a refusal. A participant who sees the read-only refusal is the
correct outcome, not a failure of the service or of the participant. A
mutation that succeeds in read-only would be a safety finding to stop on and
report under #41 and #44, not a feedback datum.

## 4. Latency

- Latency is recorded as the participant's own category per task (T7:
  acceptable, slow, gave up), not as a measured server value.
- Server-side timing is not claimed. The runbook records that no metric with
  the proposed names exists (`c2-operations.md` section 9.1), so no
  measured latency is promised by this protocol.
- Any numeric target (a percentile or a limit) is PENDING and owner-owned.
  This document sets none.

## 5. Recording form (per participant, held offline)

One record per participant, kept by the owner or lead, never copied into
this repository or an issue. Fields:

```
participant:      <local label, not a name, handle, principal or email>
group:            rehearsal | wave
GO recorded:      <reference to the group's own GO, not its content>
T1 link:          completed | completed with help | not completed
T2 relink/exit:   found | not found | not asked
T3 useful:        yes | partly | no | not asked
T4 uncertain:     understood | not understood | not asked
T5 basket read:   recognised | not recognised | not asked
T6 mutation:      refusal understood | confusing | expected | not asked
T7 latency:       acceptable | slow | gave up (per task, may be mixed)
T8 refusals:      understood | not understood | none met
free note:        <one line, no order content, no store, no item names>
opted out:        no | yes (date bucket only)
```

Rules: no order content, store names, item names, addresses, amounts or
identifiers in any field. A free note that would contain one is dropped, not
redacted.

## 6. Aggregate-only weekly summary (proposed)

The only artifact that leaves the offline record is a weekly aggregate. It
is proposed here, not approved.

Rules:

- Counts per area, no per-person row, no per-person quote.
- A bucket with fewer than a minimum count is merged or shown as "fewer than
  N". The value of N is PENDING. With 2 to 3 participants the rehearsal
  summary may need to be a single qualitative paragraph for that reason.
- No identifiers, principals, dates narrower than the week, order content or
  free-text quotes.
- Opt-outs are reported as a count only (section 7).
- A section that cannot be filled says "not observed", never an estimate.
- The summary states its group, the week, and which GO and capacity records
  it relies on, by reference.

Synthetic example (invented numbers, no real participant, illustrates shape
only):

```
Week:             synthetic-week-1
Group:            wave (synthetic)
Participants:     10 enrolled, 9 active, 1 opted out
T1 link:          7 completed, 2 completed with help, 0 not completed
T2 relink/exit:   found by 8 of 9 asked
T3 useful:        6 yes, 2 partly, 1 no
T4 uncertain:     5 understood, 3 not understood, 1 not asked
T5 basket read:   9 recognised
T6 mutation:      7 refusal understood, 2 confusing
T7 latency:       6 acceptable, 3 slow, 0 gave up
T8 refusals:      4 met one, 4 understood, 0 not understood
Safety findings:  none observed
Not observed:     server-side timing, deployed state
```

This example is not data. It must not be quoted as an expectation or a
target.

## 7. Opt-out handling (proposed)

- A participant may opt out at any time through the channel they were given
  at invitation (PENDING). No reason is asked and no follow-up pressure is
  applied.
- On opt-out: stop asking, stop recording, and remove their record from
  the offline form. Already-published aggregates are not recomputed unless
  the owner decides otherwise (PENDING).
- Ending feedback is separate from ending access. Revoking the grant,
  unlinking and erasing are mctl-api and operator actions. At this baseline
  no unlink or erase route exists for operators or users in this repository
  (`c2-operations.md` sections 6.2 to 6.4, UNAVAILABLE). The protocol must
  not promise either to a participant. What can be said is that the
  participant may revoke consent, and the next call is then refused within
  the documented windows.
- Opt-outs enter the weekly summary only as a count.
- Legal and data-subject questions are parked under #44 and are not decided
  here.

## 8. Stop conditions

Stop asking tasks, and report to the lead, if:

- a mutation succeeds in a read-only trial (T6);
- a participant sees another person's data;
- a checkout, payment or order surface appears in any answer;
- the kill switch is on or its state is unreadable (the service is switched
  off, so there is nothing to observe);
- a participant asks to stop, in which case section 7 applies first.

Whether and how a stop pauses the whole group is PENDING (owner).

## 9. Pending, owner decisions

Not decided here, in no particular order:

1. Whether the rehearsal and the wave run at all, and their GO and capacity
   records.
2. Eligibility, invitation wording, channel, and who contacts participants.
3. Number of participants per group and the aggregation minimum N.
4. Every numeric threshold: link completion, understanding, usefulness,
   latency, opt-out rate, and any stop rate.
5. The weekly summary format, owner, audience and cadence, and whether it is
   published or kept private.
6. Where the offline records live and who may read them.
7. Retention of the offline records and treatment on opt-out.
8. Whether feedback gates any later stage. Under `c2-readiness.md` the
   G4 dependency on #47 is recorded, and this protocol sets no pass level.

## 10. Not in this document

No participant contact, enrollment, data collection, service test, live
account, flag, deployment or publication. No real identifier, payload,
credential or participant record. No checkout, payment or order tool. No
claim that any gate, GO or capacity record exists.
