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
