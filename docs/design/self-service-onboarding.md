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
person: installs the helper and links
  -> read tools work; first useful commands are in the email
```

State per principal, kept in mctl-api next to consent:

`none -> pending -> approved -> (revoked)`, and `pending -> rejected`, with an
audit row per decision (actor, reason, time). A `pending` request expires after
seven days and the person is emailed that it lapsed and that they can ask again
(D7).

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
- Scope (D5, owner, 2026-11-23): the MCP resource and the link resource, so one
  identity covers both who links and who reads.
- Risk: a principal that signed in through GitHub or the staff login is locked
  out from Glovo, and the pilot principal may be one of them. It cannot be read
  from the API: mctl-api has no read route for a principal's external
  identities, only writes (`POST /admin/principals/{from}/merge-into/{into}`
  and `.../disable`). The owner does not remember which login was used.
- Therefore ship the guard in **shadow mode** first: it refuses nothing and
  logs, for every Glovo-resource sign-in, `would_refuse` with the provider and
  the masked principal. A few days of the owner's own use show whether the
  pilot principal would be refused. Only then switch it to enforcing. If the
  pilot principal would be refused, the options (decided from the data, as an
  owner decision at that time) are to attach the owner's Google identity to it
  with the existing merge route, or to re-link the owner under the Google
  principal.

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
- Service side (stage B, mctl-glovo): `GLOVO_C2_REQUIRE_ACCESS`, default
  off. On, a principal is admitted only with an active consent and
  `access: approved` in the same answer. mctl-api serves five values
  (`none|pending|approved|rejected|revoked`); the four that are not
  `approved` refuse with 403, and an absent or unknown value refuses with
  503, because it was not observed. The 403 uses the causes that already
  exist (`principal_not_allowed`, `access_denied`). The answer would let
  the service tell pending, rejected and not requested apart, but the split
  codes of 5.3 (`access_not_requested`, `access_pending`, `access_rejected`)
  are not implemented in the service yet, and this stage adds no new
  protocol shape; mapping them is a follow-up. Order: mctl-api `GLOVO_ACCESS_REQUIRED` first (its backfill
  approves existing active consents), this flag second; the reverse locks
  the owner out.
- The static allowlist stops being the day-to-day gate (D3, owner,
  2026-11-23: keep it as an emergency lock). In consent mode the service already
  treats it as optional narrowing, so in normal operation it is switched off
  (the gitops `allowlist` switch false) and consent plus the approval flag are
  the gates. In an emergency the owner switches it on with only the owner's
  principal in the Vault value; because it is read at boot, that costs a pod
  restart, so the fast emergency tools stay the kill switch and `revoked`.
  If the allowlist is left on, every newly approved person would also have to
  be added to it by hand, which defeats the flow.
- Slot (D2, owner, 2026-11-23): reserved when access is approved, so an
  approved person is never refused for capacity afterwards. Capacity still
  bounds the cohort. A reserved slot that the person never uses is released
  when the approval is revoked or expires; the expiry of an unused approval
  is a follow-up to decide with the first real cohort.
- A dynamic pilot allowlist already exists at the team-grant level in mctl-api
  (`PUT/DELETE /glovo/pilot/{tenant}/principals/{principal_id}`). Whether it is a
  better home for the approval flag than a new table is UNVERIFIED and is the
  first thing to check when stage B starts.

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
- What `/link` must say (stage A), because the helper does not log in to
  Glovo, it uploads a session file that already exists:
  1. Requirements: a Glovo account that signs in with a password or an email
     code (not only Google or Facebook), Node.js 18+ and Google Chrome, a Go
     toolchain.
  2. Record consent (and, later, request access).
  3. Create the session file with the third-party `glovo-skill`
     (github.com/denya/glovo-skill, MIT): clone, `npm ci`, `npm run build`,
     then `node scripts/login.mjs`, and choose the password or email-code
     sign-in in the window (Google sign-in is refused in that window).
  4. Install the helper: `go install
     github.com/mctlhq/mctl-glovo/cmd/glovo-link@<release>` (tags carry no `v`
     prefix, so `@latest` does not resolve to a release; the page names the
     current release).
  5. Within about twenty minutes of step 3, run `glovo-link -client-id
     <origin>/glovo-link/client.json`, sign in to MCTL and confirm. The
     access token lives about twenty minutes and the service never refreshes
     an uploaded session, so an older file is refused.
  6. Ask Claude; unlink with `glovo-link -unlink -client-id ...`.
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
- Decided (D1, owner, 2026-11-23): the Telegram button does not decide
  anything. It opens a decide page (a link carrying the one-time token), and
  the decision is taken on that page by the signed-in owner with MFA. The rule
  `directHumanAdmin` is untouched: agent, service and relay callers still
  cannot decide, and no exception is added. The cost is one more step than a
  bare tap.
  - The page follows the existing precedent: the DevLoop decide page and the
    release approval page require `amr=mfa` (mctl-api
    `handlers_devloop_decide.go`, `release_approval_page.go`), and the staff
    login that the owner uses has a hardware key as its only second factor
    (`zitadel-iac/iac/login_policy.tf`). A Glovo access decision page should
    reuse that shape rather than invent a new check.
  - The Telegram side therefore needs no callback handler and no owner
    Telegram-id check for the decision itself; the card is a notification with
    a link. The one-time token is still required: it binds the link to one
    request, expires, and is consumed on the decision, so a forwarded or
    replayed link decides nothing without the owner's own MFA session.
  - The page shows who asked (the masked identity), when, the consent version,
    the slot count, and the Approve and Reject actions with a mandatory
    reason, and records the actor and the reason in the audit row.
  - UNVERIFIED until the page is built: how the page reads the owner's MFA
    state for a new route (the existing pages are the model, not a shared
    library), and whether the link can be opened from the Telegram in-app
    browser or needs the system browser.

### 5.5 Email (mctl-web Worker)

Reuse the existing contract: mctl-api leases a row, POSTs to the Worker, the
Worker sends through the existing provider with an idempotency key and returns
a receipt; add a Glovo type with two templates.

- Approved: you are connected; sign in; link your Glovo account (steps and
  helper link); what it can do (read only: basket, order history, nearby
  shops); example commands to say to Claude; how to unlink.
- Rejected: a short, polite no.
- Sender and texts: D6 (decided), Appendix A.

### 5.6 Linking: the helper (the extension is parked)

- Wave 1 (D4, owner, 2026-11-23: a handful of technical users): the helper.
  This requires publishing
  a release of `glovo-link` with install instructions, which does not exist
  today, and stating the password or code sign-in requirement.
- PARKED (owner, 2026-11-23): a browser extension that would watch the same
  token responses in the person's own browser and upload to `/link`. It would
  need no helper, no Playwright and no Chrome install, and it would remove the
  Google block, because Google sees a normal browser. The owner judged it not
  worth building now; it is a separate project (build, review, store, privacy
  statement) and stays out of scope until the owner reopens it.
- Consequence of parking it: accounts that only have Google or Facebook
  sign-in cannot be captured with the automated tool and stay unsupported.
  Say so in the consent, `/link` and approval-email text (it already does).

### 5.7 Offboarding

Revoking access (`revoked`) deletes the stored session, frees the slot and
emails the person. A person can unlink themselves at any time, also after
revocation.

## 6. Security and privacy

- Creation of MCTL accounts is open to any Google account, so the Glovo gates
  are approval, consent and capacity, not sign-in. They must stay closed on
  any doubt: an unreadable approval is "none", never "approved".
- The decide link is attacker-influenced input if it leaks: a single-use token
  bound to one request, an expiry, MFA on the page, and an audit row with the
  actor and the decision reason. The Telegram message decides nothing.
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
| C | Telegram card with a decide-page link, the decide page (5.4), email (5.5) | mctl-telegram, mctl-api, mctl-web |
| D | Google-only guard (5.1), shadow mode first, enforcing after the pilot principal is checked from the logs | mctl-api |
| E | Two principals end to end (#43), then 2-3 trusted users at capacity 3-5 | all |
| F | Extension (5.6): PARKED by the owner, 2026-11-23 | none |

Each stage is its own PR per repository with a mutation-checked test, ships
dark behind a switch, and goes live only on the owner's explicit decision.
Stage B must land before C, and C before any real user is asked to sign up.

## 8. Decisions for the owner

- D1. DECIDED 2026-11-23: the Telegram card links to a decide page with MFA
  (section 5.4); no exception to `directHumanAdmin`.
- D2. DECIDED 2026-11-23: reserve the slot at approval.
- D3. DECIDED 2026-11-23: keep the static allowlist as an emergency lock,
  switched off in normal operation (section 5.2).
- D4. DECIDED 2026-11-23: the helper. The extension, first planned as a second
  wave, is PARKED by the owner the same day.
- D5. DECIDED 2026-11-23: Google only on the MCP resource and the link
  resource. The pilot principal is handled from shadow-mode data (section 5.1).
- D6. DECIDED 2026-11-23: sender `MCTL <noreply@mctl.ai>` (the existing
  sender; no reply-to until a monitored support mailbox exists); the texts are
  in Appendix A. Nothing is sent before stage A makes `/link` self-explanatory.
- D7. DECIDED 2026-11-23: a request lapses after seven days and the person is
  emailed.

## Appendix A. Email drafts (D6)

Sent from `MCTL <noreply@mctl.ai>` through the existing Worker contract
(section 5.5). Each mail carries the English text first and the Russian text
below it. The data paragraph repeats the consent text `c2-link-v2` (the
Vault-held key, deletion on unlink, the 14-day backup ageing, no revocation at
Glovo); if that text changes, this paragraph changes with it. The 14 days come
from the consent text; the actual backup retention is UNVERIFIED.

### A.1 Approved

Subject: `Your Glovo access is approved / Доступ к Glovo одобрен`

```
Hi,

Your request for access to the MCTL Glovo connector was approved.

Next steps
1. In Claude, make sure the connector https://glovo.mctl.ai/mcp is added
   and that you signed in with Google.
2. Link your Glovo account (one time): open https://glovo.mctl.ai/link,
   read and accept the terms, then follow the steps there to install
   glovo-link. Your Glovo account must be able to sign in with a password
   or an email code. Accounts that only use Google or Facebook sign-in
   cannot be linked yet.
3. Ask Claude.

Things you can say (read-only)
- "Show my Glovo basket."
- "What did I order last month?"
- "What do I order most often?"
- "Plan a repeat of my last order."
- "Show the menu of <store>." / "Find <item> in <store>."
- "Which stores are available at my saved address?"

What it does not do: it does not change your basket, check out or pay.

Your data: your Glovo session is stored encrypted with a key held in Vault,
and only the service can decrypt it, for requests made with your MCTL
account. Unlinking deletes it at once (encrypted database backups age out
within 14 days). We cannot revoke the session at Glovo, so after unlinking,
sign out of other sessions in Glovo. You can unlink at any time
(glovo-link -unlink), and we can revoke access.

If Claude says your Glovo account is not linked or has expired, link it
again at https://glovo.mctl.ai/link.

MCTL
```

```
Здравствуйте,

Ваш запрос на доступ к коннектору MCTL Glovo одобрен.

Что дальше
1. В Claude убедитесь, что коннектор https://glovo.mctl.ai/mcp добавлен и
   вы вошли через Google.
2. Привяжите аккаунт Glovo (один раз): откройте https://glovo.mctl.ai/link,
   прочитайте и примите условия, затем следуйте шагам там и установите
   glovo-link. В Glovo должен работать вход по паролю или коду из письма.
   Аккаунты, которые входят только через Google или Facebook, пока
   привязать нельзя.
3. Спрашивайте Claude.

Что можно сказать (только чтение)
- «Покажи мою корзину в Glovo.»
- «Что я заказывал в прошлом месяце?»
- «Что я заказываю чаще всего?»
- «Составь план повторного заказа.»
- «Покажи меню <магазина>.» / «Найди <товар> в <магазине>.»
- «Какие магазины доступны по моему сохранённому адресу?»

Чего сервис не делает: не меняет корзину, не оформляет и не оплачивает заказ.

Ваши данные: сессия Glovo хранится в зашифрованном виде, ключ находится в
Vault, расшифровать её может только сервис и только для запросов от вашего
аккаунта MCTL. Отвязка удаляет её сразу (зашифрованные резервные копии базы
устаревают в течение 14 дней). Мы не можем отозвать сессию на стороне Glovo,
поэтому после отвязки выйдите из остальных сессий в Glovo. Отвязать аккаунт
можно в любой момент (glovo-link -unlink), доступ мы тоже можем отозвать.

Если Claude пишет, что аккаунт Glovo не привязан или истёк, привяжите его
заново на https://glovo.mctl.ai/link.

MCTL
```

### A.2 Rejected

Subject: `About your Glovo access request / О вашем запросе на доступ к Glovo`

```
Thanks for asking. We can't give access to the MCTL Glovo connector right
now: it is a small private pilot. You are welcome to ask again later at
https://glovo.mctl.ai/link.

MCTL
```

```
Спасибо за запрос. Сейчас мы не можем открыть доступ к коннектору MCTL Glovo:
это небольшой закрытый пилот. Вы можете подать запрос снова позже на
https://glovo.mctl.ai/link.

MCTL
```

### A.3 Lapsed after seven days (D7)

Subject: `Your Glovo access request expired / Запрос на доступ к Glovo истёк`

```
Your request was not reviewed within 7 days and has expired. If you still
want access, ask again at https://glovo.mctl.ai/link.

MCTL
```

```
Ваш запрос не рассмотрели за 7 дней, и он истёк. Если доступ всё ещё нужен,
подайте запрос снова на https://glovo.mctl.ai/link.

MCTL
```

## 9. Unverified

How the pilot principal signs in (no read route for external identities);
whether the team-grant pilot allowlist route is a better home for the approval
flag; how each Claude client renders a 403 body and a JSON-RPC error; whether a
CIMD client can pass `?upstream=`; the mapping of tool errors in the MCP
library for store failures; the consent PUT handler's gating and the
`capacity_reached` message the page shows; whether `glovo-link` is published;
whether mail is configured in ZITADEL or elsewhere besides the Worker; the
DevLoop decide-page mechanics; the live state of the public workspace-signup
switch.
