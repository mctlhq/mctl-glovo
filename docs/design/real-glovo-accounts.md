# Design: real Glovo accounts behind glovo.mctl.ai

Status: DECIDED 2026-11-15 (section 8): C1 and C2. Nothing is built yet.
Scope decision (owner, 2026-11-15): option C, designed for external users
from the start, not only for the owner.

## 1. Where we are

- `https://glovo.mctl.ai/mcp` (v1) is live inside mctl-api: MCTL OAuth,
  team + allowlist scope, six tools on a **synthetic** catalog, owner
  approval on a page in mctl-api. It never talks to Glovo.
- Real Glovo access exists today only as a **local** stdio MCP on the
  user's own machine: the open-source `glovo-skill`
  (github.com/denya/glovo-skill, MIT, v0.2.1). It reads suggestions,
  stores, catalogs and order history, and edits the real basket. It has no
  checkout or payment tool.
- How that local tool authenticates: `glovo_login` opens a dedicated Chrome
  window on `glovoapp.com/en/login`, the user signs in, and the tool captures
  the web app's own OAuth exchange (`/oauth/token`, `/oauth/refresh`,
  `/oauth/2fa/`): an access token, a **refresh token** and the
  `glovo-device-urn` header. These are stored in a local `session.json`
  (0600). Every later call goes straight to `https://api.glovoapp.com`.

## 2. The core problem

Glovo offers no public customer API and no way to delegate a narrow,
revocable scope to a third party. The only credential available is the
user's own **web-app refresh token**, and it is a full-account credential:
whoever holds it can do anything the Glovo web app can, including placing
orders against the saved payment methods. Our "no checkout" rule is a rule
in our code, not a limit of the token.

For one user on their own machine that risk stays with them. A hosted,
multi-user glovo.mctl.ai would mean MCTL **custodies full-account
credentials of third parties** for a service it does not control and that
has not agreed to this use. That is the decision to make; everything else
below is engineering.

## 3. Risks

| # | Risk | Notes |
|---|---|---|
| R1 | Terms of service | Using Glovo's private API from a third-party server on behalf of other people is likely outside Glovo's terms. Needs a legal read before any external user. Possible outcomes: account bans for our users, IP blocks, a cease-and-desist. |
| R2 | Credential custody | A breach of the token store = control of every connected Glovo account, payment methods included. Highest-value secret MCTL would hold. |
| R3 | Bot detection | Calls from data-center IPs, many accounts behind few IPs, no real device: much more visible than the local tool. Glovo can change the API or add checks at any time; the service breaks without notice. |
| R4 | Personal data (GDPR) | Order history, addresses, phone, names. MCTL becomes a processor/controller of this data for EU users: lawful basis, DPA, retention, deletion, data subject requests. |
| R5 | Token capture UX | glovo.mctl.ai cannot read glovoapp.com tokens cross-origin. Getting the token to the server needs the user's machine (a local helper or browser extension) or pasting tokens. Pasting is unacceptable for external users. |
| R6 | Agent misuse | A model with basket write access can be prompt-injected (product descriptions, store names). Writes must stay behind human approval. |

## 4. Options

**C0. Partner route first.** Ask Glovo (Delivery Hero) for a partner or
affiliate integration: deep links, a basket-handoff URL, or an official
API. Lowest risk; may take long or be refused. Does not block anything else.

**C1. Hosted read + deep-link handoff (no stored write power).** The server
keeps no refresh token. Read-only data comes from public pages/endpoints
that need no account (stores, catalog, prices near an address). The agent
builds a basket in MCTL; the user approves it and gets a link or a list that
opens Glovo to finish there. No credential custody, no ToS-sensitive
account automation. Loses personal suggestions and order history.

**C2. Hosted with custodied tokens (what "real account" literally means).**
User links their Glovo account through a local helper; the refresh token is
stored server-side, envelope-encrypted per user (Vault transit key, never
in Postgres in clear), used only inside mctl-glovo, never logged, revocable
from the UI. Tools: read (history, suggestions) + basket edit, each basket
change behind the existing owner-approval flow. Hard requirements before
any external user: legal opinion on R1, DPA and privacy notice (R4), a
security review of the token store, per-user and global caps, kill switch,
breach runbook. Highest value, highest risk.

**C3. Local bridge (tokens never leave the user's machine).** The hosted
service holds MCTL identity, policy and approvals; the real Glovo calls are
made by a small local connector on the user's machine (like the
`mctl-telegram` local bridge), which glovo.mctl.ai drives over an
authenticated channel. No credential custody, user's own IP and device
(less R3), but the user's machine must be online, and external users must
install something.

## 5. Recommendation

1. Start **C0** now (one email to Glovo partnerships; owner sends).
2. Build **C1** as the external-user product: it is the only option that
   needs no third-party credentials and no legal grey zone.
3. Offer **C3** for power users (the owner first): real account, no
   custody, reuses the bridge pattern we already run.
4. Do **not** build C2 for external users without a written legal opinion
   on R1 and an explicit owner decision recorded here. If C2 is ever built,
   it is owner-only first, behind its own flag.

## 6. What stays the same in every option

- MCTL OAuth, team scope, allowlist (later self-service) from mctl-api.
- Owner approval page and ledger stay in mctl-api; the service never
  asserts a human decision.
- No checkout, no payment, no order placement tools.
- Fail closed: unreadable Glovo, token store or mctl-api = refusal, never
  an empty or "nothing to do" answer.

## 7. Open questions for the owner

1. Which of C0-C3 to pursue (several can run in parallel)?
2. For C1: which cities/addresses must work first?
3. For C3: acceptable that external users install a local connector?
4. Who gives the legal read on R1/R4 if C2 is ever considered?

## 8. Decision (owner, 2026-11-15)

Pursue **C1 and C2**. C0 and C3 are not pursued now.

How the two fit together:

- **C1 is the external-user product.** No Glovo credential is stored.
  `glovo-skill` already separates the calls: store walls, search and store
  content go to `api.glovoapp.com` without `Authorization` (default
  `auth = false` in `src/glovo/api.mjs`); baskets, orders, profile and
  addresses need the user's token. C1 uses only the first group, with an
  address the user types, and ends in a handoff (list and/or deep link) the
  user completes in Glovo. R1 still applies in a weaker form (automated
  reads of a private API from our servers); the partner question stays open.
- **C2 is owner-only first**, behind its own flag, never on by default.
  It is opened to anyone else only after: a written legal read on R1 and R4,
  a security review of the token store, the DPA and privacy notice, and a
  new owner decision recorded here. Until then the allowlist for C2 is the
  owner alone, enforced in mctl-api like today's pilot allowlist.

### Decision update (owner, 2026-11-15, evening)

- **Build to the C2-3 gate now.** Everything for external users is built,
  but used by the owner alone: the cluster service, the token store,
  linking and isolation. External users are switched on by a flag, and
  only after the owner obtains the written legal read on R1 and R4 and the
  C2-0 security review is done. The gate itself is unchanged.
- **Egress: all users through the owner's Mac mini.** The cluster service
  calls `api.glovoapp.com` only through a forward proxy on the mini. The
  proxy allows CONNECT to that host and nothing else. The owner accepts
  the R3 consequence: every linked account leaves from one home IP, so
  Glovo may block that IP or ban accounts, the owner's included. Mitigations
  that are not optional:
  - a per-account and global request rate cap;
  - no traffic when the proxy is down. A failed proxy means 503, never a
    direct call from the cluster;
  - a kill switch that stops all outbound Glovo calls;
  - an alert when Glovo starts answering 403/429.

### Decision update (owner, 2026-11-16): scale and parked gates

- **Target scale is a small friends cohort of 10-20 people**, not a public
  launch. `GLOVO_C2_CAPACITY` defaults to 10 and can be raised to 20 by
  env; going past that is a new decision.
  The service reads `GLOVO_C2_CAPACITY` (1-20) and sizes the cohort-wide
  caps from it (12 requests/min, 10 decrypts/h and 5 link attempts/h per
  slot); `GLOVO_C2_CAP_*` variables override single caps within fixed
  ceilings. Zero, negative, malformed or oversized values stop the service
  at start (fail closed, never uncapped).
- **The written legal read (R1 terms, R4 GDPR) and the formal security
  review of the token store are parked**, not blockers for the code work
  or for the C2-3 gate at this scale. Revisit both before any growth
  beyond about 20 users. The risks themselves are unchanged and stay
  accepted by the owner.
- **Every technical safeguard stays**: per-pid isolation tests, the
  recorded consent, per-pid and global caps, the kill switch, the encrypted
  token store, and no checkout, payment or order placement.

### Phases

| Phase | What | Gate |
|---|---|---|
| C1-S0 | Spike: which read endpoints work without an account (stores near an address, catalog, prices, product options); rate behaviour from one IP; response stability. Output: endpoint list + fixtures. | none (read-only, a few calls, no account) |
| C1-1 | Real catalog tools in mctl-glovo replacing the synthetic catalog for C1 users: search stores by address, list products, get product. Server-side cache with short TTL, global request cap, kill switch. | review + owner GO for prod |
| C1-2 | Basket in MCTL from real product ids, owner approval as today, then the handoff output. | review + owner GO |
| C2-0 | Token store design review: Vault transit envelope encryption per user, no clear token in Postgres, logs or traces; link/unlink/revoke; audit lines; breach runbook. | security review |
| C2-1 | Account linking for the owner only via a local helper (the token never passes through the chat or the model). | owner GO |
| C2-2 | Owner-only tools: order history, suggestions, real basket edit, every write behind approval. Still no checkout or payment. | owner GO |
| C2-3 | External users. | legal read + new owner decision |

## 9. OAuth and the user flow with several users (draft, 2026-11-15)

Today `glovo.mctl.ai` serves the owner alone: `glovo-home` on the owner's
machine checks the mctl-api token and a local principal allowlist
(`GLOVO_HOME_ALLOWED_PRINCIPALS`), and holds one Glovo session. This
section is how the same surface works once more than one person uses it.
It applies to C1 for everyone, and to C2 once section 8 lets anyone but
the owner in.

### 9.1 Roles stay as they are

- **Authorization server: `https://api.mctl.ai`.** Login (Google,
  ZITADEL), consent, token issue, JWKS. `glovo.mctl.ai` is only the
  protected resource (RFC 9728); its metadata points at api.mctl.ai and
  that does not move.
- **Clients are not users.** Client registration follows
  mctlhq/.github#137: CIMD first, DCR as the fallback.
  - CIMD is live on api.mctl.ai (`OAUTH_CIMD_ACCEPT` and `_ADVERTISE`).
    Clients that publish a metadata document (claude.ai, chatgpt.com,
    vscode.dev, listed in `OAUTH_CIMD_ALLOWED_HOSTS`) need no registry row
    and no allowlist entry. Their own document decides the callback.
  - DCR stays for clients without CIMD: Gemini custom connectors, Cursor,
    Antigravity, the Cloudflare portal. Only callbacks on
    `OAUTH_ALLOWED_REDIRECT_URIS` are accepted; wildcard entries are path
    prefixes on a fixed https host (mctl-api#1059, #1060).
  - When a DCR-only client starts publishing a metadata document, its host
    moves to `OAUTH_CIMD_ALLOWED_HOSTS` and its allowlist entries go.
  - The user is the `pid` claim in the token. Neither path grants access
    to anyone's Glovo data.
- **No hand-written registrations.** A client that cannot register through
  DCR gets an allowlist entry or a pre-registered client through a PR,
  never a row inserted into the database by hand.

### 9.2 Access decision

`GLOVO_HOME_ALLOWED_PRINCIPALS` is replaced by a decision from mctl-api,
in the shape that the v1 service already uses:

- grants per principal: `glovo:c1` (catalog, MCTL basket, handoff) and
  `glovo:c2` (linked real account; owner only until C2-3);
- checked on every tool call, with a short positive cache and **no**
  negative or error cache; mctl-api unreachable means 503, never a pass;
- revoking a grant stops the next call; it does not wait for the token to
  expire.

Implementation for `glovo:c2` (owner decision 2026-11-16: access is an
active consent plus a slot, with no manual step per user):

- The `glovo:c2` grant is the consent mctl-api records
  (`/api/v1/glovo/link-consent`), which holds a slot under
  `GLOVO_C2_CAPACITY`. mctl-glovo reads it through `internal/consentgate`:
  positive answers cached for at most 60 s, no negative or error cache, a
  failed read refuses with 503.
- The token verifier admits a principal through that gate when
  `GLOVO_CONSENT_API_URL` is configured. `GLOVO_C2_ALLOWED_PRINCIPALS`, when
  set, still narrows access (an intersection). Until C2-3 it stays set to
  the owner in mctl-gitops, which keeps C2 owner-only; removing it is the
  switch that opens C2 to every consenting principal.
- A token must name one grant: a team grant, or a personal grant
  (`tgk=personal`) that mctl-api issues to a human with no team who holds
  the consent. Both are keyed by `pid`.

### 9.3 First use, step by step

1. The user adds `https://glovo.mctl.ai/mcp` in their client. The client
   gets the 401 challenge and the metadata, then identifies itself by its
   CIMD URL (or registers via DCR if it has none) and opens api.mctl.ai.
2. The user signs in at api.mctl.ai and sees its consent page (client
   name, resource `glovo.mctl.ai`). The token carries `aud` =
   `https://glovo.mctl.ai/mcp` and the user's `pid`.
3. The first tool call checks the grant (9.2). No grant: a refusal that
   says how to ask for access. Never an empty catalog.
4. C1 tools work from here: an address the user types, catalog reads with
   no Glovo account, a basket kept in MCTL, then the handoff.
5. C2 tools for a user with `glovo:c2` and no linked account return
   `account not linked` with a link to `https://glovo.mctl.ai/link`.

### 9.4 Linking a Glovo account (C2)

The Glovo token can only be obtained in a browser on glovoapp.com
(R5), so linking needs the user's machine:

1. `/link` (after api.mctl.ai login) shows what linking allows: read order
   history and suggestions, edit the real basket; never checkout, payment
   or order placement. Explicit consent is recorded in mctl-api with a
   timestamp and the text version.
2. The page shows a one-time link code: random, single use, valid for
   10 minutes, bound to the `pid`, stored as a hash.
3. The user runs the local helper (the `glovo-relogin.sh` pattern,
   mctl-glovo#4). It opens the Glovo login and captures the session
   locally. Then it uploads the session over TLS with the code, and with
   nothing in argv or the shell history. The token never passes through
   the chat or the model.
4. The server checks the code, stores the session for that `pid` (C2-0:
   Vault transit envelope per user, never in clear in Postgres, logs or
   traces) and burns the code.
5. `/link` shows the linked state, the last successful use and **Unlink**.
   Unlinking deletes the stored session at once. The user is also told to
   sign out of other sessions in Glovo, because we cannot revoke Glovo's
   refresh token ourselves.

Re-login after an expired session follows the same path with a new code.
The keepalive alert goes to the account's owner, not to the operator.

### 9.5 Isolation between users

- One Glovo client per `pid`. With the Go port (#7), this is one
  `glovoapi.Client` per linked session inside one process, not one Node
  child per user.
- The session is looked up by `pid` from the verified token only, never
  from a tool argument.
- Basket locks, rate limits and the global request cap are per `pid`.
  The kill switch is global.
- The audit line carries `pid`, tool, outcome and duration. Arguments,
  results and tokens are never logged.
- A required test: user A's token can never reach user B's session,
  basket or history, including after B unlinks and relinks.

### 9.6 Where the owner-only gateway fits

`glovo-home` on the owner's machine is the owner's C2-1/C2-2 path today,
with custody on the owner's own hardware. Moving to the shared service
replaces only the allowlist (by 9.2) and the session store (by 9.4). The
tool surface, the no-checkout rule and the token checks stay the same.

## Kill switch flip path (C2-2 slice 5)

State lives in the `glovo_control` row (the store ships killed; an unreadable
or missing row is killed). `PUT /internal/v1/control/kill-switch` with body
`{"killed": bool}` writes the row, reads it back, audits the flip
(`kill_on` / `kill_off`, actor operator) and answers `200 {"killed": <stored>}`;
any failure is a non-200 and means "state unknown" to the caller.

- Auth: a machine-user bearer token verified by `authn.ServiceVerifier`
  (pinned alg EdDSA or RS256, exact issuer, audience, exp/nbf/iat, key set
  fetched over https) and an explicit subject allow-list. An empty allow-list
  refuses everyone; with the four `GLOVO_C2_CONTROL_*` variables unset the
  route is not mounted.
- Cluster-internal only: a request carrying proxy forwarding headers is a 404
  even with a valid token. The ingress must not route `/internal/` either.
- Enforcement: every hosted tool call reads the switch (a "running" answer is
  cached for at most 5 s; a failed or slow read is killed) before the admitter
  and before any session lookup, and answers a fixed text.
- A tripped switch (global decrypt cap) latches in the process until restart;
  a later flip to running changes the row, not the latch.
