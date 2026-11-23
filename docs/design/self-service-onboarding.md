# Design: self-service onboarding for the hosted Glovo connector

Status: proposal for owner review, 2026-11-23. Tracking: #82 (plan and gates),
#11 (epic), #43 (two-principal proof). Nothing here is implemented; every
change below needs its own reviewed PR and its own owner decision to go live.

## 1. Goal

A person adds `https://glovo.mctl.ai/mcp` as a connector in Claude and, with
no help from the owner except one approval, ends up working with their own
Glovo orders and basket:

1. adds the connector and signs in (Google only);
2. asks for access;
3. the owner approves or rejects with one tap in Telegram;
4. the person gets an email: connected, how to link, example commands;
5. links their Glovo account and uses the read tools.

Everything except the approval is self-service.

Non-goals: basket write tools or placing orders (a separate decision, the
write tools stay absent on the cluster); Glovo accounts that can only sign in
with Google or Facebook (section 6.6); mobile linking.

## 2. What exists today (verified on `origin/main`, 2026-11-23)

Facts only; where a fact could not be verified it is marked UNVERIFIED.

### 2.1 Sign-in and principals

- The connector's protected-resource metadata points the client to the
  authorization server `https://api.mctl.ai` (mctl-glovo
  `internal/gateway/gateway.go`). Clients that publish a metadata document
  (claude.ai among them) need no registration (CIMD).
- `/oauth/authorize` in mctl-api shows a chooser with three entries: Google,
  GitHub, and an MCTL staff login (`internal/api/oauth_upstream.go`,
  `pickOAuthUpstream`). Google and GitHub go through ZITADEL's customers org
  with an IdP scope, so ZITADEL's own login page is skipped; the staff entry
  uses ZITADEL's instance login (password plus a hardware key).
- A principal (`prn_...`) is created automatically at the first sign-in of an
  external identity (`internal/principals`, `internal/auth/link.go`). A ZITADEL
  hook creates the customer account; creation is open to any Google account
  (`ZITADEL_HOOK_CUSTOMER_CREATE_GOOGLE_SUBS="*"`) and limited to a short list
  of ids for GitHub. The hook creates an account and a principal only: no
  tenant, namespace or database.
- The access token carries no sign-in provider claim (`iss`, `sub`, `aud`,
  `pid`, `client_id`, grant fields, `auth_time`). The provider is known only at
  the ZITADEL callback.
- A Google account alone cannot obtain a Glovo token: a personal grant needs a
  recorded `glovo:c2` consent (`internal/auth/oauth_teamgrant.go`).

### 2.2 Gates in front of the service

- Static allowlist, `GLOVO_C2_ALLOWED_PRINCIPALS`: parsed once at boot from a
  Vault-backed secret, immutable afterwards. Adding a person means editing
  Vault and restarting the pod.
- Consent, read from mctl-api per call and cached (30 s at entry, 5 s per
  call). Recording consent claims a slot under `GLOVO_C2_CAPACITY`; a full cap
  is `capacity_reached`. Both gates are on, and the allowlist narrows consent.
- Kill switch, capacity-scaled rate caps, per-principal caps.

### 2.3 What a user sees today

| State | Response | Tells the user what to do? |
|---|---|---|
| no token | 401 with the resource metadata challenge | yes (OAuth discovery) |
| not on the allowlist | 403 `access_denied`, "this account is not allowed on this server" | no |
| no consent | the same 403, indistinguishable from the line above | no |
| admitted, never linked | tool error: "Your Glovo account is not linked. Link it at https://glovo.mctl.ai/link." | a URL only |
| `needs_relink` / refresh refused | tool error with the same link | a URL only |
| suspended | "Your Glovo link is suspended." | no |
| kill switch on | "Glovo account access is switched off." | no |
| per-call admission refused | bare "access refused" | no |

Authentication runs on every `/mcp` request including `initialize`, so a
person who is not admitted never receives the tool instructions that contain
the `/link` URL.

### 2.4 The link page and the helper

- `GET /link` needs no token and no script. It shows the consent text
  (version `c2-link-v2`) and "how linking works": record consent in MCTL, run
  `glovo-link`, confirm. It gives no install command, no download, no value
  for the helper's required `-client-id`, and no way to record consent from
  the page. Whether `glovo-link` is built or published anywhere is
  UNVERIFIED (no release path in the repository).
- The helper uploads a session file that already exists. The tool that produces
  it (`glovo-skill`'s `login.mjs`) starts Chrome through Playwright and
  captures the token responses. Tested 2026-11-23: with a Glovo account tied
  to Google, the Google button fails ("This browser or app may not be secure",
  Google refuses the automated browser) and "Continue with password" works.
- Unlink exists only through the helper (`glovo-link -unlink`); a person whose
  consent is withdrawn or who left the allowlist gets 403 and cannot unlink
  through the user route.

### 2.5 Notification infrastructure

- Telegram: the owner alert for signup requests is a Cloudflare Worker
  sending plain text without buttons. Only the personal-assistant bot in
  mctl-telegram has working inline buttons and callbacks (webhook secret
  header, an allowlist of Telegram user ids, opaque `callback_data` resolved
  in a table, durable `update_id` against replay). The login bot polls and has
  no callback handler.
- Email: sent only by the mctl-web Worker through a transactional provider
  from a platform no-reply sender; mctl-api has no mail client. The signup
  pipeline delivers its decision email by leasing a row, POSTing to the Worker
  and storing the receipt (`notification_pending`, `delivery_id`).
- The signup store is tenant-specific (GitHub id, workspace slots) and does
  not fit a Glovo request for an existing principal. Its patterns do: the
  status model, one advisory lock, expiry sweep, mandatory decision reason,
  delivery lease and receipt.
- The kill-switch and signup admin routes require a "direct human admin"
  (`directHumanAdmin`): agent, service and relay callers are refused.

## 3. Gaps this design closes

1. A person who is not admitted is told nothing and cannot find `/link`.
2. "Not on the allowlist" and "no consent" are the same 403.
3. Admission is a Vault edit plus a pod restart, so one-tap approval is
   impossible.
4. No request, notification or decision path exists for Glovo access.
5. `/link` cannot be followed without knowing the helper and its client id.
6. Sign-in is not limited to Google for this resource.
7. No email to the person at any step.

## 4. Proposed flow

```
Claude: add connector -> OAuth at api.mctl.ai (Google only)
  -> principal exists (automatic)
  -> first /mcp call: 403 access_not_requested + pointer to /link
person: /link -> reads the terms, records consent, presses "Request access"
  -> request = pending                      (mctl-api)
owner: Telegram card [Approve] [Reject]     (PA bot, one-time token)
  -> approve: access = approved, slot reserved, email "you are connected"
  -> reject: email "not now", request closed
person: installs the helper (or, later, the extension) and links
  -> read tools work; first useful commands are in the email
```

State per principal, kept in mctl-api next to consent:

`none -> pending -> approved -> (revoked)`, and `pending -> rejected`, with an
expiry for `pending` (the signup store uses seven days) and an audit row per
decision (actor, reason, time).

## 5. Components

### 5.1 Google only on the Glovo resource (mctl-api)

Fact: the token has no provider claim, so the Glovo verifier cannot enforce
this. The cheapest correct place is the ZITADEL callback
(`handleOAuthZitadelCallback`), before the code is issued.

- Guard: when the resource is the Glovo MCP resource (and the link resource),
  accept only a customers-org identity proven as Google; otherwise
  `access_denied`. No change to the ZITADEL IaC, the hook allowlists, the
  customers or instance login policies, `OAUTH_UPSTREAM`, or principal
  resolution: those also serve the staff, admin and other products.
- Offering, not only accepting: for the Glovo resource skip the chooser and
  start `customer_google` directly. The choice must be made server-side from
  `resource`; whether a CIMD client can carry the `?upstream=` query is
  UNVERIFIED, so do not depend on it.
- Tests both ways: Google passes; GitHub, staff and password sign-ins are
  refused; reverting the guard makes the refusal test fail.
- Risk to check first: a principal that signed in through GitHub or the staff
  login is locked out from Glovo. The pilot principal may be one of them; its
  sign-in method must be checked before the guard goes live, and if needed its
  Google identity linked to the same principal first.

### 5.2 Access request and approval flag (mctl-api, mctl-glovo)

- New table next to the consent store (not the signup table): principal id,
  status (`pending|approved|rejected|revoked`), requested, decided, actor,
  reason, expiry. Human-only routes mirror `link-consent`: the caller's own
  principal, no route names another.
- The consent read route the service already calls
  (`GET /api/v1/assistants/glovo-consent/v1/principals/{id}`) gains a separate
  `access` field (`approved|none`, absent when unreadable). `state` stays the
  consent state. The service fails closed on absent or unknown, as it does for
  consent today.
- The static allowlist stops being the day-to-day gate. Keep it only as an
  owner-only narrowing for break-glass, or remove it in a later PR; decision
  D3.
- Slot: reserve the consent slot when access is approved, so an approved
  person is never refused for capacity afterwards (decision D2). Capacity still
  bounds the cohort.

### 5.3 Messages that tell the user what to do (mctl-glovo, mctl-api)

- Split the 403: `access_not_requested`, `access_pending`,
  `access_rejected`, `consent_required`, each with a one-line text and the
  `https://glovo.mctl.ai/link` URL. Keep the protocol shape the clients
  already accept.
- Landing page: say how it works in four lines (sign in, request, approval,
  link), not only "only invited accounts get in".
- `/link`: show the current state of the request, a "record consent" and
  "request access" action that calls the human-only routes with the person's
  own sign-in, the install steps for the helper with the OAuth client id, and
  the requirement that the Glovo account must allow password or code sign-in.
- Replace the bare "access refused" with a cause; give the kill-switch and
  suspended texts a next step.
- Unlink: a button on `/link` and a mention in the error texts; let a person
  whose access was revoked still delete their stored session.

### 5.4 Owner notification and the decision (mctl-telegram, mctl-api)

- Home: the personal-assistant bot in mctl-telegram (it already has webhook
  verification, an owner id allowlist, callback ids resolved in a table and
  `update_id` replay protection). The login bot is not reused.
- Flow: mctl-api creates the request and a one-time token (hash, request id,
  expiry, `used`) and asks mctl-telegram to send the card; the callback
  carries only an opaque id; mctl-telegram checks the sender against a
  dedicated owner list and calls mctl-api with a service identity; mctl-api
  consumes the token atomically, so a replay is `409`.
- Conflict with `directHumanAdmin`: a Telegram callback is a service path and
  cannot pass that rule as written. Two options, decision D1:
  (a) the button opens a decide page that requires MFA (the DevLoop decide-page
  model; the rule is intact, one more tap); (b) a deliberate, narrowly scoped
  exception for this one action, trusting the allowlisted Telegram id plus the
  one-time token and recording the actor as the Telegram id. (b) is a weakening
  of the rule and is the owner's call.

### 5.5 Email (mctl-web Worker)

Reuse the existing contract: mctl-api leases a row, POSTs to the Worker, the
Worker sends through the existing provider with an idempotency key and returns
a receipt; add a Glovo type with two templates.

- Approved: you are connected; sign in; link your Glovo account (steps and
  helper link); what it can do (read only: basket, order history, nearby
  shops); example commands to say to Claude; how to unlink.
- Rejected: a short, polite no.
- The template texts and the sender are decision D6.

### 5.6 Linking: helper first, extension second

- Wave 1 (a handful of technical users): the helper. This requires publishing
  a release of `glovo-link` with install instructions, which does not exist
  today, and stating the password or code sign-in requirement.
- Wave 2: a browser extension that watches the same token responses in the
  person's own browser and uploads to `/link`. It needs no helper, no
  Playwright and no Chrome install, and it removes the Google block, because
  Google sees a normal browser. It is a separate project (build, review,
  store, privacy statement) and is decision D4.
- Accounts that only have Google or Facebook sign-in cannot be captured with the
  automated tool; say so in the consent and `/link` text until the extension
  exists.

### 5.7 Offboarding

Revoking access (`revoked`) deletes the stored session, frees the slot and
emails the person. A person can unlink themselves at any time, also after
revocation.

## 6. Security and privacy

- Creation of MCTL accounts is open to any Google account, so the Glovo gates
  are approval, consent and capacity, not sign-in. They must stay closed on
  any doubt: an unreadable approval is "none", never "approved".
- A Telegram callback is attacker-influenced input: opaque ids, owner-only
  sender check, single-use token, expiry, audit with the decision reason.
- The service holds other people's Glovo sessions. The existing token-store
  design (encryption, decrypt caps, audit) applies; the consent text, the
  retention and the deletion path must cover people other than the owner.
- The service uses an unofficial client of Glovo. The stance on Glovo's terms
  and the partnership contact (C0 in `real-glovo-accounts.md`) remain open and
  gate any use beyond the owner's circle.
- Public repository: no principal ids, emails, chat ids or hostnames beyond the
  public ones in code, comments, tests or commit messages.

## 7. Rollout (maps to #82)

| Stage | Change | Repository |
|---|---|---|
| A | Messages and `/link` page (5.3), helper release and install text | mctl-glovo |
| B | Access table, routes and the `access` field (5.2), service reads it | mctl-api, mctl-glovo |
| C | Telegram card and callback (5.4), email (5.5) | mctl-telegram, mctl-api, mctl-web |
| D | Google-only guard (5.1), after checking the pilot principal | mctl-api |
| E | Two principals end to end (#43), then 2-3 trusted users at capacity 3-5 | all |
| F | Extension (5.6), wider cohort | new |

Each stage is its own PR per repository with a mutation-checked test, ships
dark behind a switch, and goes live only on the owner's explicit decision.
Stage B must land before C, and C before any real user is asked to sign up.

## 8. Decisions for the owner

- D1. Telegram tap: open an MFA decide page (a), or a narrow exception to
  `directHumanAdmin` for this one action (b).
- D2. Reserve the slot at approval, or claim it at consent as today.
- D3. Keep the static allowlist as an owner-only break-glass narrowing, or
  remove it once the approval flag is live.
- D4. Helper only for the first wave, or the extension in the first release.
- D5. Google-only scope: the MCP resource only, or the link resource too; and
  how the pilot principal is handled before the guard.
- D6. Email sender, wording and example commands.
- D7. Expiry of an unanswered request (seven days is the signup precedent) and
  whether the person is told.

## 9. Unverified

How each Claude client renders a 403 body and a JSON-RPC error; whether a
CIMD client can pass `?upstream=`; the mapping of tool errors in the MCP
library for store failures; the consent PUT handler's gating and the
`capacity_reached` message the page shows; whether `glovo-link` is published;
whether mail is configured in ZITADEL or elsewhere besides the Worker; the
DevLoop decide-page mechanics; the live state of the public workspace-signup
switch.
