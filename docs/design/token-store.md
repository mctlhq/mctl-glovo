# Design: token store for linked Glovo accounts (C2-0)

Status: REVIEWED. Nothing here is built. The owner decided all ten review
questions on 2026-11-15 (9.3), so go criterion 1 in 9.2 holds. C2-1
(linking) starts only when the rest of 9.2 holds. External users (C2-3)
additionally need the criteria in 9.4.

Scope: epic mctlhq/mctl-glovo#11, slice C2-0. The parent design is
`docs/design/real-glovo-accounts.md` (PR #2): risks R1-R6 (section 3), the
owner decisions (section 8) and the multi-user flow (section 9). This
document does not reopen those decisions. It designs how mctl-glovo stores,
uses and destroys the Glovo sessions of linked accounts.

Conventions:
- **Verified** platform facts give the file they come from (section 11).
  Paths are on `main` of the named repository, read on 2026-11-15.
- **Unverified** marks anything not checked against a file or the live
  system. Such a statement is an assumption, not a fact.
- Vault product behaviour (transit, audit HMAC, policies) is taken from the
  HashiCorp documentation. The live Vault is 1.17.2 according to the
  vault-config README. Every Vault behaviour this design relies on is in
  the acceptance checks of 9.2, so it gets proven on the live version
  rather than assumed.

## 1. What is stored, and why it is the highest-value secret

A linked account is one glovo-skill session object. It is what
`internal/glovoapi.Session` decodes (`internal/glovoapi/session.go`):

| Field | Sensitivity |
|---|---|
| `refreshToken` | **Full-account credential.** Mints new access tokens for as long as Glovo honours it. |
| `accessToken` | A bearer JWT for `api.glovoapp.com`, short-lived (lifetime read from its `exp` claim). |
| `deviceUrn`, `perseusClientId`, `perseusSessionId`, `perseusSessionTimestamp` | The device identity Glovo has seen for this session. Not a credential alone, but it is part of what makes calls look like the user's own device. |
| `customerId` / `customer.id` | Glovo account id: personal data. |
| `location` | A delivery address and coordinates: personal data. |
| `expiresIn`, `createdAt`, `tokenType`, `scope`, `updatedAt` | Metadata. |
| unknown fields | Kept and written back unchanged (the type is a generic map on purpose). |

The hosted `location` is not a separate record: `glovo_set_location` and
`glovo_select_location` write it into this same encrypted session blob,
through the lease's fenced update (`Lease.writeSession`), onto the session the
row holds at that moment so rotated tokens are never overwritten. It is
encrypted at rest, erased with the link (Unlink/Erase), has no separate
retention and is never logged. A location write is audited as `refresh` with
reason `location_set`; a dedicated audit event would need a migration of the
`glovo_store_audit.event` check and is left as an open question.

The **whole object** is encrypted as one blob. Splitting out "only the
refresh token" would leave an address and an account id in clear, and
unknown fields would land on whichever side the code forgot.

Why this is R2, the highest-value secret MCTL would hold:
- Glovo has no scoped or delegated token. The refresh token is the web
  app's own, so whoever holds it can do anything the web app can: read
  the order history and addresses, and **place orders against the saved
  payment methods**. Our "no checkout" rule is a property of
  `internal/glovoapi` (its mutation allowlist), not of the token.
- The token is usable from anywhere. Once it leaves our process, none of
  our controls (proxy, caps, kill switch, approval) apply to it.
- We cannot revoke it. Glovo exposes no revocation endpoint to us. Only
  the user can end it, by signing out of Glovo's sessions or changing the
  password. **Unverified:** whether either of those actually invalidates
  existing refresh tokens on Glovo's side.
- One store holds every linked account, so a single breach scales with
  the number of users.

How a refresh rotates (from `internal/glovoapi/client.go`,
`refreshLocked`): `POST /oauth/refresh` with the refresh token. The new
session takes the response's `refreshToken` if present, otherwise it
keeps the old one. Refreshes are serialised by an in-process mutex, and
the result is written to a 0600 file before it replaces the in-memory
session. **Unverified:** whether Glovo rotates the refresh token on every
refresh, and whether it rejects a refresh token that has already been
used. The design assumes the worst case: every refresh rotates, and the
old token dies at once. Under that assumption, a refresh whose result is
not persisted means a lost account (the user has to link again).

## 2. Storage

### 2.1 Options

| | A. Vault transit, derived key, ciphertext in Postgres (**recommended**) | B. Vault KV v2, one secret per `pid` | C. App-held master key, per-row DEK in Postgres |
|---|---|---|---|
| Where the key lives | In Vault. Never leaves it (`exportable=false`, `allow_plaintext_backup=false`). | Vault storage encryption (barrier) only. | A master key in a Kubernetes Secret or env var (from ESO), in the pod's memory. |
| What a DB dump gives | Ciphertext only. | Nothing (no DB). | Ciphertext, plus wrapped DEKs; with the master key from the cluster, everything. |
| What app RCE gives | Decrypt of every `pid` while the attacker holds the pod (see 3.4). | Read of every `pid` while the attacker holds the pod. | The master key itself: **offline** decrypt of every row, forever, with no further Vault calls and no audit. |
| Per-`pid` binding | Cryptographic: the derivation context is the `pid` and the link id. A ciphertext moved to another row does not decrypt. | Path naming only (`.../<pid>`). The policy cannot express "only the caller's own `pid`". | Possible via AEAD associated data, in our own code. |
| Audit | Every encrypt, decrypt and rewrap is a Vault audit entry with the workload identity (stdout device, Loki). | Every read is audited. | None outside our own code. |
| Key rotation | `rotate` + `rewrap`; rewrap never exposes plaintext to the app. | Not applicable (no app key). Storage key rotation is Vault-wide. | Our own code and procedure. |
| Deletion | Delete the Postgres row. Old copies stay in DB backups but are ciphertext. | `destroy` of all versions (KV `delete` only tombstones; the coolify-mcp policy notes exactly this). Old copies stay in Vault snapshots. | Delete the row. |
| Platform precedent | No transit mount exists in mctl-gitops (searched; section 11). It would be the first. | `coolify-mcp-users/`: a dedicated KV v2 mount per service holding third-party credentials, Kubernetes auth, `destroy` only. | None found. |
| Extra failure mode | Vault down = no decrypt = 503 (fail closed, as required). | Same. | None at decrypt time. That is the problem: nothing can stop or see a decrypt. |

Within A there are two shapes:
- **A1, direct transit encrypt/decrypt** (recommended). The session blob
  (a few KiB) is sent to `transit/encrypt` and comes back as
  `vault:vN:...`. No key material is ever in the app.
- **A2, transit data keys** (`datakey/wrapped`): the app gets a DEK, does
  AES-GCM itself and stores the wrapped DEK. The plaintext session never
  travels to Vault, but the plaintext DEK does, so the wire exposure is the
  same in practice. It adds nonce handling and AEAD code that we would own
  and have reviewed. It only pays off for large payloads or high volume,
  and we have neither.

Within A, the key can be **one derived key** (`derived=true`, the context
selects a per-`pid` key) or **one named key per `pid`**:
- Per-`pid` named keys allow crypto-erasure by deleting the key. They also
  require the app's policy to hold `create` on `keys/*` and
  `delete` with `deletion_allowed`. That lets an RCE create keys, delete
  every user's key (mass destruction), or reconfigure keys. The erasure
  is also weaker than it looks: the daily Vault raft snapshots (the
  `vault-backup` CronJob keeps the last 30) still contain a deleted key.
  mctl-api's Glovo connection store already declines to call a future
  per-connection key design "crypto-shredding" for the same reason
  (`docs/glovo-connections.md`, Retention).
- A derived key keeps the app's policy to three paths with `update` only
  (2.4). Erasure comes from deleting the row, and backups age out (4.4).

**Recommendation: A1 with one derived transit key.** Ciphertext and
metadata go in mctl-glovo's own Postgres database. The key never leaves
Vault, every decrypt is audited by Vault independently of our code, and
the app's Vault policy cannot read, rotate, export or delete keys.
Option B is a sound second choice and has a precedent. It loses the
cryptographic `pid` binding and the plaintext-free rewrap. Option C is
rejected: an RCE would turn into offline, unaudited, permanent access.

### 2.2 Vault objects

- Mount: dedicated, `glovo-transit/` (`type=transit`). It is not a key on
  some shared transit mount. This follows the platform pattern of one
  dedicated mount per hosted service with its own write path
  (vault-config README, coolify-mcp section).
- Mount tune: none. The transit `context` stays HMAC'd in the Vault audit
  log, like plaintext and ciphertext (owner decision Q6, 9.3). The audit
  log is readable by every Grafana viewer (`docs/runbooks/vault-audit.md`),
  so `pid`s must not appear there in plaintext.
  `audit_non_hmac_request_keys` must not include `context`. To find the
  entries for one `pid`, an admin hashes that row's base64 context with
  `vault write sys/audit-hash/stdout input=<base64 context>` and filters
  on the result (the runbook's "one token's entries" method).
- Key `glovo-session`: `type=aes256-gcm96`, `derived=true`,
  `convergent_encryption=false`, `exportable=false`,
  `allow_plaintext_backup=false`, `deletion_allowed=false`.
- Key config (`glovo-transit/keys/glovo-session/config`):
  `auto_rotate_period=2160h` (90 days). The create endpoint also accepts
  this parameter. It is set on the config path anyway, so that the
  setting is one explicit, re-runnable call that go criterion 2 can check.
- Created by the owner with an admin token, as every other Vault object
  in vault-config is today ("applied by hand", README). The commands and
  the HCL are committed to mctl-gitops `infrastructure/.../vault-config/`
  next to the existing policies, and covered by
  `scripts/vault-policy-expectations.json` /
  `tests/test_vault_policy_files.py` (they check repo text only, as the
  README says).

### 2.3 Data layout (Postgres, mctl-glovo's own database)

Postgres holds ciphertext and metadata only. No column holds a token,
Glovo customer id, address, phone, name or e-mail in clear.

```
glovo_links            one row per linked pid; DELETEd on unlink
  pid               text PRIMARY KEY            -- prn_..., from a verified token
  link_id           text NOT NULL UNIQUE        -- gln_<ULID>, new on every link
  state             text NOT NULL               -- active | suspended | needs_relink
  ciphertext        text NOT NULL               -- vault:vN:...
  key_version       int  NOT NULL               -- N from the prefix, for the rewrap sweep
  version           bigint NOT NULL             -- nextval('glovo_link_version_seq') on EVERY write
  consent_version   text NOT NULL               -- copied from the redeemed link code
  lease_token       uuid                        -- refresh lease (2.5); NULL = free
  lease_holder      text                        -- replica id, for diagnostics only
  lease_until       timestamptz
  linked_at, updated_at, last_used_at, last_refresh_at timestamptz
  access_expires_at timestamptz                 -- from the access token, for the keepalive
  refresh_failures  int NOT NULL DEFAULT 0

glovo_link_version_seq one global sequence; never reset, never reused

glovo_link_codes       one-time link codes; swept 24 h after expiry
  code_hash        bytea PRIMARY KEY            -- SHA-256 of the raw code
  pid              text NOT NULL
  consent_version  text NOT NULL                -- the consent mctl-api recorded before the code was minted
  created_at, expires_at, used_at timestamptz

glovo_audit_subjects   pid -> subject_key; the row is DELETEd on erasure (5)
  pid          text PRIMARY KEY
  subject_key  text NOT NULL UNIQUE             -- gas_<random>, never derived from the pid

glovo_store_audit      append-only, keyed by subject_key (section 5)

glovo_control          one row: runtime kill switch and global caps (section 6)
```

The transit context for a row is the bytes
`mctl-glovo/session/v1|<pid>|<link_id>`, base64-encoded as the transit
API requires:
- `pid` binds the ciphertext to its owner. A row swapped or copied to
  another `pid` fails to decrypt.
- `link_id` binds it to one link. A ciphertext from an earlier link of
  the same user (from a DB backup, or a relink after unlink) does not
  decrypt under the current row. That is part of the §9.5 isolation
  requirement "including after B unlinks and relinks".
- The prefix versions the format.

`key_version` duplicates what is in the ciphertext prefix. A mismatch on
read is treated as corruption (refusal, audit event `integrity_error`),
not ignored.

`version` comes from one global sequence, never from `version + 1`. A
link, a refresh, a rewrap and a state change each take a new value. A
relink therefore never brings back a `version` an earlier link had. That
holds even after an unlink deleted the row, because the sequence
outlives the row. Every staleness check compares the pair
(`link_id`, `version`), never `version` alone (4.2).

### 2.4 KEK rotation and rewrap

- Rotation: `auto_rotate_period` on the key, or a manual
  `vault write -f glovo-transit/keys/glovo-session/rotate` by the owner.
  New encryptions use the latest version. Old ciphertexts keep
  decrypting.
- Rewrap: a sweep in mctl-glovo (a subcommand run as a Kubernetes CronJob
  with the same identity, or a background loop) selects rows with
  `key_version < latest` in batches and calls
  `glovo-transit/rewrap/glovo-session` with each row's context. It takes
  the row's refresh lease first (2.5) and skips rows whose lease is held.
  It then writes `UPDATE ... SET ciphertext, key_version, version =
  nextval('glovo_link_version_seq'), lease_token = NULL, lease_until = NULL
  WHERE pid = $1 AND link_id = $2 AND version = $3`. Rewrap returns
  ciphertext only, so the sweep never handles plaintext.
- Retiring old versions: after a sweep reports zero rows below version N
  (a positive count read, never "the query failed so there are none"),
  the owner raises `min_decryption_version` to N. The app cannot do it:
  it has no `update` on `keys/*/config`.
- Reading `latest_version` needs `read` on `glovo-transit/keys/glovo-session`.
  The app does not get it. The sweep gets the target version from config,
  or simply rewraps every row on each run (rewrap to the latest is
  idempotent). The second is simpler, and at owner scale it costs nothing.

### 2.5 Write path, refresh rotation and atomicity

There must be exactly one refresh in flight per `pid` across all
replicas. Two concurrent refreshes with a rotating refresh token would
have the second one present a token the first one already spent.

The in-process `refreshMu` in `glovoapi.Client` is not enough once there
is more than one replica, or a keepalive job beside the server. The design
is a **lease with a compare-and-swap fence** (owner decision Q7). No
database transaction or lock is held while Glovo is called.

1. **Take the lease** in one short statement:
   ```sql
   UPDATE glovo_links
      SET lease_token = $new_uuid, lease_holder = $replica,
          lease_until = now() + interval '30 seconds'
    WHERE pid = $1 AND state = 'active'
      AND (lease_until IS NULL OR lease_until < now())
   RETURNING link_id, version, ciphertext, key_version;
   ```
   Zero rows is followed by one plain read that names the reason: no row
   (`ErrNotLinked`), a state other than `active` (`ErrSuspended` /
   `ErrNeedsRelink`), or a lease held by someone else (`ErrBusy`, a
   refusal, never a silent skip). A failed statement is `ErrUnavailable`.
   The statement runs under `statement_timeout`, which does apply to it.
   It waits on nothing but the row lock of a concurrent short UPDATE.
   Taking or releasing a lease changes only the `lease_*` columns, never
   `version`. A lease is not a session write, so it neither invalidates
   cached clients nor moves the fence.
2. **Decrypt** the returned row with the context built from the returned
   `pid` and `link_id`. If another holder refreshed in the meantime (the
   access token now has more than 60 s left, the same rule `ensureFresh`
   uses), release the lease and return the fresh session without calling
   Glovo.
3. **Call Glovo** (`fn`, the refresh through the proxy) with a hard
   deadline of 15 s. That is half the lease, so a refresh that returns has
   always returned inside its own lease.
4. **Write behind the fence**: encrypt the new session with the same
   context, then
   ```sql
   UPDATE glovo_links
      SET ciphertext = $c, key_version = $kv,
          version = nextval('glovo_link_version_seq'),
          lease_token = NULL, lease_holder = NULL, lease_until = NULL, ...
    WHERE pid = $1 AND link_id = $2 AND version = $3;
   ```
   Exactly one row means success. The fence is (`link_id`, `version`),
   **not** the lease token. Every successful writer (refresh, rewrap,
   relink, state change) takes a new `version`, so a write based on a
   superseded read can never land, even when its lease has expired. The
   lease only stops two holders from calling Glovo at once. Correctness
   does not depend on it.
5. Only after the write succeeds does the new session replace the
   in-memory client, under the new (`link_id`, `version`).
6. **Release** on any other exit:
   `UPDATE ... SET lease_* = NULL WHERE pid = $1 AND lease_token = $mine`.
   A crashed holder's lease simply expires after 30 s.

Failure cases, assuming the worst-case rotation from section 1:
- **The Glovo refresh fails** (4xx): the lease is released and
  `refresh_failures+1` is written behind the same fence. After a 401/403
  from `/oauth/refresh`, the state becomes `needs_relink` and the owner
  gets an alert (4.5). A 5xx or a proxy failure is transient: retry later,
  no state change.
- **The write affects zero rows.** Re-read the row: it is gone, or its
  `link_id` differs, which means an unlink or a relink won the race. In
  that case the new tokens belong to a link that no longer exists. They
  are discarded, not kept, and the event is audited. If `link_id` matches
  and only `version` moved, another writer persisted first. Its session is
  the live one, and ours is discarded.
- **The Glovo refresh succeeded, but the write failed or its result is
  unknown** (DB unreachable, connection lost on COMMIT): the new tokens
  exist only in this process. The client enters the **`unpersisted`**
  state for that `pid`:
  - it is **pinned**. The idle TTL does not evict it, and neither does a
    (`link_id`, `version`) mismatch in the 4.2 check, because its expected
    DB state is the *pre-refresh* pair it is trying to replace. Only four
    things end the pin: a successful write; a re-read showing the row gone
    or with another `link_id` (discard, as above); a re-read showing
    another `version` on the same `link_id` (another writer won; discard);
    or the kill switch (6), which drops every in-memory client, pinned
    ones included, and accepts the account loss;
  - the write is retried with backoff behind the same fence, and the
    first step after a reconnect is a re-read to learn whether the lost
    COMMIT actually landed;
  - tool calls for that `pid` are **refused** with `ErrUnavailable` until
    the pin ends. They are not served from the unpersisted client. This
    keeps 4.2's rule (a failed DB read is a refusal, never a cache
    fallback) and leaves exactly one owner of the new refresh token, the
    retry loop;
  - the alert `GlovoSessionUnpersisted` fires;
  - if the process dies while pinned, the account needs relinking. That
    loss is accepted and documented. The alternative would be writing
    plaintext somewhere durable, which is not acceptable.
- **A lease expires while its holder is still pinned**, and another
  replica takes the lease and refreshes with the old (now spent) refresh
  token. Glovo refuses it, the state goes to `needs_relink` with a new
  `version`, and the pinned write then loses the fence and is discarded.
  Accepted residual (T12): it needs a DB outage longer than the lease
  together with a second refresher. Running a single replica in
  C2-1/C2-2 rules out the second refresher.

C2-1 code change this implies: `refreshLocked` persists through
`SaveSession(c.sessionPath, ...)` today. The hosted client needs a
persistence hook instead (a `SessionSink` option called inside the store's
`Update`), so that no code path in the hosted service ever writes a
session file. A test asserts that the hosted constructor refuses a
session path.

## 3. Access

### 3.1 Identity that can decrypt

Only the mctl-glovo workload, through Vault Kubernetes auth. This is the
pattern the platform already uses for Backstage, mctl-api, vault-backup
and coolify-mcp (vault-config README).

- A dedicated ServiceAccount, `mctl-glovo`, in the mctl-glovo namespace.
  Never a shared `default` SA. The README's coolify-mcp section records
  why: `labs` shares `default` across 15+ deployments. **Unverified:**
  the namespace mctl-glovo will run in. It is a C2-2 decision; examples
  below write `<ns>`.
- The Kubernetes auth role is bound to exactly that SA name and namespace,
  with `token_no_default_policy=true`, `token_ttl=15m`,
  `token_max_ttl=1h`. Without Vault's `default` policy the token would
  lose `auth/token/renew-self` and `auth/token/lookup-self`, so the
  policy grants exactly those two paths and nothing else that `default`
  carries (cubbyhole, `sys/capabilities-self`, `sys/tools`, and so on).
  The client renews at 80% of the TTL. It logs in again with its SA token
  when a renewal is refused or when `token_max_ttl` is reached, which is
  the Backstage pattern in the vault-config README.
- The app logs in itself with its projected SA token and keeps the Vault
  token in memory only. **No ExternalSecret carries a Vault token or any
  transit material.** ESO cannot do transit anyway, and the cluster-wide
  `vault-backend` store must stay limited to `secret/data/platform/*`.
- Vault address: the in-cluster service only
  (`http://vault-active.vault.svc.cluster.local:8200`, the address
  mctl-api uses according to `vault-netpol.yaml`), **never
  `secrets.mctl.ai`**. `*.mctl.ai` is behind a proxied wildcard DNS record
  (mctl-gitops `deploy/staged/glovo-m1/README.md`). **Unverified:**
  whether `secrets.mctl.ai` itself is proxied by Cloudflare. If it is, a
  decrypt through it would hand plaintext sessions to Cloudflare's TLS
  termination.

Draft policy `mctl-glovo-transit` (HCL):

```hcl
# mctl-glovo: encrypt, decrypt and rewrap with the one derived key.
# Nothing else: no keys/* read, no rotate, no config, no export, no
# datakey, no hmac, no other key, no other mount. Policies deny by
# default; the denies below exist so that a later broad grant attached to
# the same token cannot widen this one by accident.
path "glovo-transit/encrypt/glovo-session" {
  capabilities = ["update"]
}

path "glovo-transit/decrypt/glovo-session" {
  capabilities = ["update"]
}

path "glovo-transit/rewrap/glovo-session" {
  capabilities = ["update"]
}

# The two token self-service paths the role needs, because
# token_no_default_policy=true drops the default policy.
path "auth/token/renew-self" {
  capabilities = ["update"]
}

path "auth/token/lookup-self" {
  capabilities = ["read"]
}

path "glovo-transit/keys/*" {
  capabilities = ["deny"]
}

path "glovo-transit/export/*" {
  capabilities = ["deny"]
}

path "glovo-transit/backup/*" {
  capabilities = ["deny"]
}

path "glovo-transit/datakey/*" {
  capabilities = ["deny"]
}
```

Role (owner applies, like the other roles in the README):

```bash
vault secrets enable -path=glovo-transit transit
# No `secrets tune -audit-non-hmac-request-keys=context`: the context stays HMAC'd (Q6).
vault write glovo-transit/keys/glovo-session \
  type=aes256-gcm96 derived=true exportable=false \
  allow_plaintext_backup=false
vault write glovo-transit/keys/glovo-session/config \
  auto_rotate_period=2160h deletion_allowed=false
vault policy write mctl-glovo-transit vault-policy-mctl-glovo-transit.hcl
vault write auth/kubernetes/role/mctl-glovo \
  bound_service_account_names=mctl-glovo \
  bound_service_account_namespaces=<ns> \
  policies=mctl-glovo-transit \
  token_no_default_policy=true token_ttl=15m token_max_ttl=1h
```

### 3.2 No operator or agent path to plaintext

Standing access:
- No human policy grants anything on `glovo-transit/`. The
  `human-tenant-*` policies (vault-human-auth-iac `groups.tf`) cover
  `secret/.../teams/<tenant>/*` only.
- No agent identity (mctl-agents, DevLoop, Argo workflows) gets the
  `mctl-glovo` role or the mctl-glovo database credentials.
- The service exposes no endpoint, MCP tool or debug route that returns a
  session or a token. `Store.Get` returns a value the MCP layer cannot
  serialise (3.3).

What we cannot prevent, only detect. The reviewer has to accept these
explicitly:
- The `admin` policy held by the `human-admins` group "is the owner's,
  written by hand" (`groups.tf`). Its content is not in the repository
  (**unverified**, presumably broad). A Vault admin can grant themselves
  decrypt. With the database credentials as well, that gives plaintext.
- `argo-workflow-provisioner` can create and update any policy named
  `tenant-*` / `eso-tenant-*`, with any content, and any Kubernetes auth
  role named `eso-tenant-*` / `argo-workflow-tenant-*`
  (`vault-policy-argo-workflow-provisioner.hcl`). Whoever controls that
  identity can mint a policy that grants `glovo-transit/decrypt/*` and
  bind it to any ServiceAccount. `vault-human-auth-iac` has the same
  shape for `human-tenant-*`. **Unverified:** which pods can assume the
  provisioner role.
- Cluster admins can read the mctl-glovo DB Secret, exec into the pod, and
  read `vault-unseal-keys` (3.4, row V).

Detection for all of them is the same alert: **any request to
`glovo-transit/*` whose `auth.metadata.role` is not `mctl-glovo`**, plus
any write to `sys/policies/acl/*` whose body mentions `glovo-transit`
(policy text is HMAC'd in the audit log, so the second one has to be
caught as "any policy write" and reviewed). It is a new rule in
`vm-rules/vault-audit-alerts.yaml`, built on the Promtail counters
pattern in `vault-audit.md`. Following the workspace rule on detectors,
the alert is accepted only once it is proven both ways: silent on the
real workload, firing on a deliberate decrypt by a test identity.

### 3.3 Decrypt bound to the `pid` of a verified token

Vault cannot enforce "this caller may decrypt only `pid` X". The policy
sees the path, not the context, and the workload identity is the service,
not the user. Context binding is therefore enforced in our code, and its
guarantee is narrower than it sounds:

- What the context gives: a ciphertext decrypts only under its own `pid`
  and `link_id`. DB tampering (swap, copy, restore of an old row) cannot
  make user A's call use user B's session.
- What it does not give: protection against our own process. Code that
  knows B's `pid` and link id can decrypt B's row. That is why 3.4 counts
  app RCE as "all linked accounts".

Code-level binding:
- The store takes a `store.PID`, a type with an unexported field. The only
  constructor is `store.PIDFrom(*authn.Principal)`, and the only source of
  a `Principal` is `authn.Verifier.Verify` (`internal/authn/verifier.go`,
  `pid` claim). A tool argument, header or query parameter cannot become
  a `PID` without that verified token. The one exception is the link
  upload (4.1), where the `pid` comes from the code row, never from the
  request.
- `Store.Get` returns a `*Lease`: an opaque handle that builds the
  per-`pid` `glovoapi.Client`. It has no exported session field.
  `String`, `GoString`, `Format`, `MarshalJSON` and `slog.LogValue` all
  render `[redacted]`.
- Required test (from §9.5): user A's token can never reach B's session,
  basket or history, including after B unlinks and relinks. Plus a
  DB-level test: swapping two rows' ciphertexts makes both decrypts fail
  with `integrity_error`.

### 3.4 What each attacker gets

| # | Attacker has | Gets | Does not get |
|---|---|---|---|
| D | Read access to the mctl-glovo database (SQL injection, leaked DB credentials, a `shared-pg` base backup or WAL from R2) | Ciphertexts; which `pid`s are linked and when; the audit table | Any plaintext. Decrypt needs the `mctl-glovo` Vault role. |
| DW | Write access to the database | Denial of service (delete or corrupt rows); setting `state=active` on a suspended row; flipping `glovo_control` | Cross-user use: a swapped ciphertext fails context checks. A forged ciphertext fails GCM authentication. |
| R | RCE in the mctl-glovo pod | **Every linked session**, for as long as the foothold lasts: the pod holds the DB credentials and can log in to Vault. It can also call Glovo through the proxy as any user. | Keys (no export or read). Persistence after the pod is gone, *except* for refresh tokens it exfiltrated, which keep working outside our control (section 1). |
| S | A stolen SA token or Vault token of mctl-glovo, used from another pod | Decrypt for up to `token_max_ttl` (1 h), if it can also reach Vault (the netpol allowlist) and has ciphertexts (row D) | Anything with only one of the two halves |
| P | A policy misconfiguration: another identity granted `glovo-transit/*`, for example through the provisioner path in 3.2 | Decrypt, once it also has ciphertexts (row D) | Plaintext without ciphertexts. The alert in 3.2 fires on its first request. |
| V | Vault itself: the `vault-unseal-keys` Secret in the `vault` namespace (used by `vault-auto-unseal.yaml`) plus a raft snapshot (R2) or a live pod | The transit key, so everything when combined with row D. The Vault snapshots and the `shared-pg` backups are in the **same R2 bucket** (`vault-backup.yaml`, `cnpg/shared/object-store.yaml`). | Nothing extra beyond R2 + unseal keys + the PG backup |
| N | The pod network between mctl-glovo and Vault | **Plaintext sessions on every encrypt and decrypt.** The Vault listener has `tls_disable = 1` (`vault.yaml`, "SOC F15 accepted residual"). | Anything on links that do not carry Vault traffic |
| X | The egress proxy on the Mac mini | The fact that a CONNECT to `api.glovoapp.com:443` happened, and when | Tokens or content: TLS is end to end (epic #11, E1). |
| L | Logs, traces, error messages, Loki | Nothing, if section 4.2 holds and its tests pass | — |

Row R is the one that matters. For C2-1/C2-2 there is one linked account
(the owner's), so R equals "the owner's own account", and the owner
accepts that. Before C2-3, the reviewer decides whether the MCP-facing
process (which parses untrusted model and tool input) may keep the Vault
role, or whether decrypt and Glovo calls move into a small separate
broker. The broker would expose typed Glovo operations only, never a
token (question Q1).

## 4. Lifecycle

### 4.1 Link (the one-time code flow of §9.4)

1. The user opens `/link` (behind api.mctl.ai login, `glovo:c2` grant
   checked as in §9.2). The page shows what linking allows and the consent
   text version. mctl-api records the consent (§9.4 step 1).
2. `Store.CreateLinkCode(pid, consentVersion)` generates 32 random bytes
   (`crypto/rand`), shows them once as base64url, and stores only
   `SHA-256(code)`, with the `pid`, the `consent_version` mctl-api
   recorded in step 1, and a 10-minute expiry. The consent version is
   taken from the server-side consent record, never from the request or
   the later upload body. A code cannot exist without consent. At most one live code per `pid`: a new
   code invalidates the previous one. Audit event `link_code_issued`.
3. The local helper (PR #4 pattern) reads the code from **stdin** (never
   argv or env), and calls `POST /link/preview` with the code. The answer
   is the *masked* MCTL identity the code belongs to (for example
   `a***@e***.com`). The helper asks "Link your Glovo account to this MCTL
   account?" before it captures anything. This defends against the
   phishing case in which an attacker sends the victim *the attacker's*
   code, so that the victim's Glovo account ends up linked to the
   attacker's `pid` (threat T10).
4. The helper captures the session locally and calls `POST /link/upload`
   over TLS: `{"schema":"glovo-link-upload/v1","code":...,"session":{...}}`.
   Validation, in order, every failure a refusal with a fixed message:
   - body at most 16 KiB, JSON, exact schema, no trailing data;
   - per-IP and global rate limits on both endpoints (code guesses are
     useless at 256 bits; the limit protects the Vault and Glovo budget);
   - the kill switch (section 6) is off and `glovo_control` is readable;
   - **atomic redemption**:
     `UPDATE glovo_link_codes SET used_at = now() WHERE code_hash = $1 AND
     used_at IS NULL AND expires_at > now() RETURNING pid,
     consent_version`. Zero rows means
     the code is unknown, used or expired, and the answer is one identical
     refusal for all three;
   - the session has string `accessToken` and `refreshToken`, a
     `deviceUrn`, and each field under its own size cap;
   - one validating call to Glovo through the proxy with the uploaded
     session (an authenticated read, for example the basket GET the
     keepalive uses). Failure means a refusal and nothing stored.
5. Mint a new `link_id`, encrypt with the new context, then upsert the row
   in one statement: `state=active`, the new `link_id`, `version =
   nextval('glovo_link_version_seq')`, `consent_version` from the redeemed
   code row, lease cleared. The version is **never reset**. The new
   `link_id` and the new `version` both differ from anything the previous
   link had, so every replica's cached client for the old link fails the
   4.2 check on its next call. An existing link for that `pid` is replaced,
   and its old ciphertext is gone with the overwritten row. A refresh in
   flight on the old link then loses the fence in 2.5, and its tokens are
   discarded. The upsert does not wait for a lease: a relink, like an
   unlink, always wins. Audit `link` with outcome.
6. The response says "linked" and nothing else. No session data, no Glovo
   account details.

A code is burned at step 4 even if validation fails later. A failed link
needs a new code. That is simpler than un-burning, and it is safe.

### 4.2 Use: memory only

- A decrypted session lives only inside the per-`pid` `glovoapi.Client`,
  in process memory. A cache keeps the client for an idle TTL (10 min).
  On a miss it calls `Store.Get`, which means one Vault decrypt.
- The cached client remembers the (`link_id`, `version`) pair it was built
  from (`Lease.LinkID()`, `Lease.Version()` in 10.1). Every tool call
  re-reads `state`, `link_id` and `version` from `glovo_links` (one
  indexed row, no decrypt). If the row is gone, not `active`, or either
  value of the pair differs, the cached client is dropped first. Because
  `version` comes from a global sequence and `link_id` is new on every
  link (2.3), a relink can never present the pair of an earlier link,
  even after an unlink deleted the row. That makes unlink, suspension and
  relink take effect at once on every replica. If the read fails, the
  call is refused (503). It never falls back to the cache.
- The one exception to "a mismatch drops the client" is a client in the
  `unpersisted` state (2.5). It is pinned, and its tool calls are refused
  while it is pinned, so it is never served and never evicted by this
  check. Only the rules in 2.5 end the pin.
- Never logged, traced or put in errors:
  - the `Lease` redaction above;
  - `glovoapi` errors already carry fixed text for auth failures
    (`AuthError`), and `APIError` carries method, path, status and a
    detail. A test drives every error path with a session whose token
    values are unique markers, then scans every log line, error string and
    panic message for them (mutation-verified: remove one redaction and
    the test fails);
  - mctl-glovo has no tracing exporter and no `net/http/pprof` today
    (`go.mod`, a grep of the repository). C2-1 adds a test that fails if
    either appears;
  - the audit line of a tool call carries `pid`, tool, outcome and
    duration (§9.5), never arguments or results.
- Go cannot reliably zero strings. Plaintext stays in the heap until the
  GC reuses it. The mitigations: no core dumps (the container runs
  non-root with `RLIMIT_CORE=0`), a read-only root filesystem, no shell in
  the image.

### 4.3 Unlink and revoke

- `Store.Unlink(pid)` deletes the `glovo_links` row (a hard `DELETE`, not
  a state flag), deletes the `pid`'s live link codes, drops the in-memory
  client, and writes the `unlink` audit event, all in one transaction.
  Unlink works **even when the kill switch is on** and never needs Vault:
  deletion must never depend on the thing that might be broken.
- Unlink does not take or wait for the refresh lease. A refresh in flight
  cannot resurrect the link: its write is fenced on (`link_id`,
  `version`) and hits zero rows, and the re-read then finds the row gone.
  That is the documented absence, so the new tokens are discarded, not
  pinned (2.5). A pinned `unpersisted` client for the `pid` is discarded
  by the same re-read.
- Triggers: the user (`/link`, Unlink), revocation of the `glovo:c2`
  grant (the next call sees no grant; a sweep unlinks `pid`s without the
  grant for over 24 h), an operator in a breach (section 7), and a data
  subject request (4.4).
- What we cannot do: revoke the refresh token at Glovo. The `/link` page
  and the unlink confirmation tell the user to sign out of other sessions
  in Glovo and, after a suspected breach, to change the password.
  **Unverified:** that Glovo offers "sign out everywhere", and that it
  kills refresh tokens.

### 4.4 Retention and deletion (R4, GDPR)

| Data | Retention |
|---|---|
| Session ciphertext (live) | While linked. Deleted at unlink. |
| Session ciphertext in Postgres backups | Until the backups age out. `shared-pg` keeps 14 days (`retentionPolicy: "14d"`, `cnpg/shared/object-store.yaml`). **Unverified:** that mctl-glovo's database lands on `shared-pg` (it would come from the provisioning workflow; C2-2). |
| Transit key | Not deleted per user (derived key). Vault snapshots keep the last 30 days (`vault-backup.yaml`). |
| Link codes | 24 h after expiry. |
| Audit rows | Target 400 days, the same target as mctl-api's `glovo_audit_events`. They hold a `subject_key`, not the `pid`, plus event, outcome and time, and no Glovo data. On erasure the `pid` is pseudonymised: the `glovo_audit_subjects` row is deleted, so no live data maps the `subject_key` back to the `pid` (owner decision Q8, section 5). |
| Store log lines (Loki) | 14 days (336 h, `vault-audit.md`). They carry the `subject_key` too. The tool-call audit line from §9.5 carries the `pid` and ages out on the same 14 days. |
| In-memory client | Idle TTL, and dropped at unlink, on the kill switch, at restart. |

Data subject requests (MCTL's role, controller or processor, is part of
the R4 legal read):
- **Erasure (Art. 17):** an unlink, done the same day. A restored backup
  younger than 14 days would contain the ciphertext, which still decrypts
  under the derived key if restored together with Vault and if the
  `link_id` row comes back too. The restore runbook therefore re-applies
  every `unlink` and `erasure` recorded in the live audit table after the
  backup's timestamp, before the service starts. Those rows name a
  `subject_key`. The restored `glovo_audit_subjects` table still maps it
  to the `pid` for the 14-day backup tail, so the restore can find the
  rows to delete. It then deletes the restored mapping rows again. The answer to the user states the backup
  window honestly. It does not claim crypto-erasure.
- **Access (Art. 15):** we hold the Glovo session, which contains an
  address and an account id, and audit metadata. We export the metadata
  and state which fields the session holds. We do not hand out the
  tokens: the user already has them in their own browser, and a
  token-export path would be the very thing 3.2 forbids.
- Timelines: answer within one month (Art. 12(3)), erasure "without undue
  delay". The process is owned by the owner until a DPA and a privacy
  notice exist (a C2-3 precondition, §8).

### 4.5 Keepalive and expiry alerts go to the account's owner

- A keepalive per linked `pid` (one authenticated read a day, the
  glovo-home pattern) runs through `Store.Update` when the access token is
  near expiry, so it takes the same lease and fence (2.5).
- It counts against that `pid`'s caps and stops under the kill switch.
  A long kill can therefore let sessions expire. That is accepted and
  stated on the status page.
- On `needs_relink`, the alert goes to **the user whose account it is**,
  with a link to `/link`. It contains no token and no Glovo data.
  **Unverified / open:** the channel (e-mail through mctl-api, or the MCTL
  Telegram bot). It must be one mctl-api owns, since mctl-glovo holds no
  contact data.
- The operator sees aggregate counts only (`needs_relink` total, refresh
  failure rate), never which user unless the user asks for support.

## 5. Audit

Every store event writes one row to `glovo_store_audit` and one
structured log line (`slog`, Loki) in the same code path:

| Field | Content |
|---|---|
| `id` | `gsa_<ULID>` |
| `at` | `clock_timestamp()` |
| `subject_key` | `gas_<random>` from `glovo_audit_subjects`, created on the `pid`'s first event. The audit table never stores the `pid` itself. |
| `link_id` | when one exists |
| `event` | `link_code_issued`, `link_preview`, `link`, `link_refused`, `decrypt`, `refresh`, `persist_retry`, `persist_discarded`, `rewrap`, `unlink`, `erasure`, `suspend`, `resume`, `kill_on`, `kill_off`, `integrity_error`, `cap_exceeded` |
| `outcome` | `ok`, `refused`, `error` |
| `reason` | a fixed code (`code_invalid`, `vault_unavailable`, `glovo_401`, `busy`, ...), never free text from a dependency |
| `actor_kind` | `user`, `service`, `operator` |
| `request_id` | correlation with the tool-call audit line |

Never in it: tokens, ciphertexts, Glovo customer ids, addresses, request
or response bodies, IP addresses of the uploader (**open**: the reviewer
may want the upload IP for abuse cases; then it becomes a hashed field
with its own retention).

Pseudonymisation on erasure (owner decision Q8). The audit table stays
strictly append-only, and erasure does not rewrite it:
- every audit row and store log line carries `subject_key`, never `pid`.
  The only link between the two is `glovo_audit_subjects`;
- a data subject erasure unlinks (4.3), writes one `erasure` event under
  the `subject_key`, then deletes the `glovo_audit_subjects` row in the
  same transaction. From then on, the audit history of that user is a
  pseudonymous sequence that nothing in mctl-glovo can tie back to a
  `pid`;
- a new link by the same `pid` later gets a fresh `subject_key`;
- this is pseudonymisation, not anonymisation. mctl-api still knows the
  `pid`, and the restored backups of the 14-day tail still hold the
  mapping (4.4). The answer to the user says so.

Where it lives:
- **The table is the record.** Insert-only: the app role has `INSERT` and
  `SELECT` and nothing else, and a trigger refuses `UPDATE`, `DELETE` and
  `TRUNCATE` for every role. That is the same mechanism as mctl-api's
  `glovo_audit_events` (`docs/glovo-connections.md`). A retention sweep
  after 400 days is a separate design with its own role, as it is there.
- **The log line feeds alerts.** Loki keeps general streams for 14 days
  (336 h, `vault-audit.md`), which is too short to be the record.
- **Vault's own audit is the independent witness.** Each `decrypt` row
  must correspond to one `glovo-transit/decrypt` entry by role
  `mctl-glovo`. An hourly reconciliation compares the two counts per
  window. More Vault decrypts than audit rows means decrypts outside the
  audited path (RCE calling Vault directly), and that alerts. In line
  with the workspace detector rule, a window in which either side could
  not be read is reported as `unknown` and alerts on its own. It never
  counts as "equal".

## 6. Kill switch and caps

Two layers, both fail closed:
- **Deploy-time flag** `GLOVO_C2_ENABLED` (env, from gitops, default
  `false`). Off: the store is not constructed, no Vault login happens,
  every C2 tool and `/link` route refuses. Changing it takes a gitops PR
  and a rollout (minutes).
- **Runtime switch** `glovo_control.c2_killed` (one row), read on every
  call, with no cache longer than 5 s and no negative or error cache. An
  unreadable control row means killed. Flipping it is an operator action
  through an mctl-api admin route (mctl-api already owns the `GLOVO_ENABLED`
  kill switch for the synthetic service, `docs/glovo-connections.md`).
  **Open:** whether the runtime switch should live in mctl-api and be read
  through the grant check rather than in mctl-glovo's DB.

What "killed" stops, inside the store, not just at the tool layer:
- `Store.Get` and `Store.Update` refuse **before** calling Vault. A global
  stop therefore also stops decrypts, and the Vault audit shows zero
  decrypts during a kill, which is itself checkable;
- every in-memory `glovoapi.Client` is dropped within the 5 s cache
  window, including pinned `unpersisted` ones (2.5). Their refresh tokens
  are lost, and those accounts need relinking after the kill;
- keepalives, refreshes and the rewrap sweep stop;
- `Link` and `CreateLinkCode` refuse;
- `Unlink` keeps working (4.3).

Per `pid`: `state=suspended` gives the same behaviour for one user
(operator or automatic, see below). `resume` is an operator action.

Caps (numbers are proposals for owner-only operation, to be tuned):

| Cap | Scope | Value | On breach |
|---|---|---|---|
| Glovo requests | per `pid` | 60/min, 2,000/day | refusal `rate_limited` |
| Glovo requests | global | 120/min | refusal; the 403/429 alert from the epic |
| Decrypts (cache misses) | per `pid` | 30/hour | `suspended` automatically, alert |
| Decrypts | global | 100/hour | **runtime kill**, alert: a burst of decrypts across many `pid`s is the exfiltration pattern of row R |
| Link attempts | per IP / global | 10/hour / 50/hour | refusal |
| Refresh failures | per `pid` | 3 in a row | `needs_relink` |

Counters are in-process for a single replica (C2-1/C2-2 run one
replica). Before more than one replica, the counters move to Postgres or
are split per replica with a documented factor. That is a C2-2 item.

## 7. Breach runbook

Detection signals:
- the 3.2 alert: a `glovo-transit/*` request by any role but `mctl-glovo`;
- the 5 reconciliation: Vault decrypts without matching audit rows, or
  `unknown`;
- the global decrypt cap tripping (6);
- `integrity_error` events (tampered rows);
- Glovo 403/429 spikes (the epic's alert), or users reporting orders they
  did not place;
- a Vault policy write or a new Kubernetes auth role nobody announced;
- a leaked image, DB backup or R2 credential reported anywhere.

Containment, in order. Each step is reversible unless marked, and each is
recorded with a timestamp in the incident issue:

1. **Runtime kill** (`c2_killed = true`). Stops decrypts, Glovo calls and
   links within 5 s.
2. **Cut Vault access for the workload**: rewrite `mctl-glovo-transit` to
   an empty policy (`vault policy write mctl-glovo-transit /dev/null`-style
   empty file). Policies are evaluated per request, so this applies to
   tokens already issued. Then `vault delete auth/kubernetes/role/mctl-glovo`.
3. **Lock the key** if anyone else may have decrypt (rows P, S): rotate
   `glovo-session`, then set `min_decryption_version` to the new latest.
   Every existing ciphertext becomes undecryptable for everyone. This is
   reversible by lowering the value again, which allows forensics or a
   controlled restore.
4. **Scale mctl-glovo to zero** and keep the pod and node for forensics
   if RCE is suspected. Rotate the mctl-glovo DB credentials and the
   Tailscale auth key (E3).
5. **Delete all ciphertexts** (`DELETE FROM glovo_links`), once forensics
   no longer need them. **Irreversible.** Every user has to link again.
   Do this at once if the attacker may have had both ciphertexts and
   decrypt.
6. **Find the scope**: the store audit and the Vault audit
   (`vault-audit.md` queries, filtered on `glovo-transit/` and on the
   attacker's role or entity) give how many decrypts happened, by whom,
   and when. The context is HMAC'd (Q6), so the Vault line does not name
   the `pid`. To map entries to users, an admin hashes each linked row's
   base64 context with `sys/audit-hash/stdout` and matches the results.
   The store audit adds the `subject_key` of every audited decrypt.
7. **Treat every exposed refresh token as live in the attacker's hands.**
   Step 3 does not help with tokens that were already decrypted: they
   work against Glovo without us.

Notification:
- **Users** whose `pid` was decrypted, or all linked users if the scope
  is unknown: without undue delay (Art. 34; a full-account credential with
  payment ability is "high risk"). The message says to sign out of all
  Glovo sessions, change the Glovo password, and check orders and saved
  cards.
- **The supervisory authority**: within **72 hours** of becoming aware
  (Art. 33), with a partial report allowed and completed later. The clock
  starts at awareness, not at confirmation, so step 6 must not delay it.
  **Open:** which authority (it depends on MCTL's establishment; part of
  the R4 legal read).
- **Glovo**: whether to notify Glovo is a legal question (R1). The
  runbook asks it rather than presuming an answer.

Recovery: fix the root cause, apply a new key or a lower
`min_decryption_version` only if the ciphertexts were kept, re-enable,
and have users link again. Postmortem in the epic.

A tabletop run of steps 1-3 against the real Vault and a test row is a
go criterion (9.2).

## 8. Threat model

| # | Threat | Mitigation | Residual |
|---|---|---|---|
| T1 | DB dump or backup leak (R2 bucket, SQL injection, credentials) | Ciphertext only; no Glovo data in clear in any column | Linked `pid`s and timestamps are visible. Low. |
| T2 | DB tampering: swap or copy ciphertexts across users, restore old rows | Context = `pid` + `link_id`; GCM authentication; `integrity_error` audit | Denial of service. Low. |
| T3 | App RCE | Minimal image, non-root, read-only rootfs; egress netpol (Vault, DB, mctl-api, Tailscale sidecar only); decrypt caps leading to an automatic kill; Vault-vs-audit reconciliation; single replica | **All linked sessions for the foothold's duration, and exfiltrated tokens stay valid.** High for C2-3; for C2-1/C2-2, "the owner's own account", accepted by the owner. Q1. |
| T4 | Stolen SA or Vault token | 15 min / 1 h TTL; Vault ingress netpol allowlist; needs ciphertexts too | Up to 1 h of decrypt for a holder of both. Medium. |
| T5 | Another identity granted `glovo-transit/*` (misconfiguration, provisioner `tenant-*` policy escalation, a human admin) | Dedicated mount; no human or agent policy on it; alert on any non-`mctl-glovo` role; policy files in gitops with tests | The provisioner and `admin` paths exist and stay possible. Detection only. Medium. Q4. |
| T6 | Vault compromise: unseal keys Secret + snapshot, or cluster-admin | Platform controls (outside this design) | Everything. Platform-level residual, shared with every Vault secret. Q5. |
| T7 | Sniffing east-west Vault traffic (plaintext listener, F15) | NetworkPolicy; in-cluster address only, never the public hostname | Plaintext on the pod network for anyone with node or CNI-level access. Q2. |
| T8 | Leak through logs, traces, errors or panics | `Lease` redaction; fixed error texts; marker-scan test; no tracing or pprof | A future dependency that logs request bodies. Low, guarded by the tests. |
| T9 | Cross-user access via a tool argument | `store.PID` only from a verified token; A/B isolation test incl. relink | Low. |
| T10 | Link phishing: the victim uploads their session with the attacker's code | `/link/preview` shows the target account's masked identity before capture; 10 min TTL; single use | A user who confirms without reading. Medium. Q9. |
| T11 | Code guessing or replay | 256-bit codes; hash at rest; atomic single redemption; rate limits | Negligible. |
| T12 | Lost refresh on a concurrent or failed write | Lease against concurrent Glovo refreshes; (`link_id`, `version`) CAS fence on every write; pinned `unpersisted` client with retry and alert (2.5) | A process death while pinned, or a DB outage longer than the lease together with a second refresher, loses that account (needs relink). Low impact. |
| T13 | Operator or agent reads plaintext "to debug" | No endpoint, no policy, no DB credentials for agents; workspace rule against reading Secret data | Cluster admins can still do it; detected by T5's alert only if they use a non-`mctl-glovo` identity. Medium. |
| T14 | Prompt injection makes the model exfiltrate the token | No tool accepts or returns session material; the store has no MCP surface | Low. |
| T15 | The proxy (the owner's Mac mini) is compromised | TLS end to end; the mini sees only CONNECT to `api.glovoapp.com` | Traffic metadata and denial of service. Low. |
| T16 | The tailnet sidecar reaches more than the proxy | The tailnet ACL grant for `tag:glovo-egress` (E2, **still open** per the epic comment) | Open until the ACL exists. A go criterion for C2-2. |
| T17 | Glovo-side: the tokens cannot be revoked by us | User guidance at unlink and in the breach notice | Permanent property of R2. |

## 9. For the security reviewer

### 9.1 Questions to answer

All ten were decided by the owner on 2026-11-15 (9.3). The questions
stay here as they were asked, so the decisions can be read against them.

- **Q1. App RCE (T3).** For owner-only C2-1/C2-2, is "RCE = the owner's
  account" acceptable? Before C2-3, must decrypt and Glovo calls move out
  of the MCP-facing process into a broker with no token-export surface,
  or is a single process with caps, kill and reconciliation enough?
- **Q2. Plaintext east-west Vault (T7, SOC F15).** Accept it for transit
  traffic in C2-1/C2-2? Is a TLS listener on Vault, or a mesh/encrypted
  CNI, a precondition for C2-3?
- **Q3. Derived key vs per-`pid` keys.** Is the recommendation (one
  derived key, a narrow policy, erasure by row deletion with a 14-day
  backup tail) acceptable against R4, or is per-`pid` key deletion
  required despite the wider policy and the 30-day snapshot tail?
- **Q4. Escalation paths (T5).** Accept that `admin` and the
  `argo-workflow-provisioner` / `vault-human-auth-iac` policy-writing
  identities can grant themselves decrypt, with detection only? Or should
  their policy-name globs get explicit denies against content covering
  `glovo-transit/` (Vault OSS cannot inspect policy content, so this would
  need a change to how those identities work)?
- **Q5. Unseal keys in a Kubernetes Secret (T6).** Is the platform
  residual acceptable for holding third-party full-account credentials,
  or does C2-3 need a different unseal (auto-unseal through an external
  KMS)?
- **Q6.** May `pid`s appear in the Vault audit log
  (`audit_non_hmac_request_keys=context`), which every Grafana viewer can
  query?
- **Q7.** A transaction-scoped advisory lock held across the Glovo
  refresh, or a lease with a CAS fence?
- **Q8.** Audit retention of `pid` after erasure: 400 days, or pseudonymise
  on erasure? (Overlaps the R4 legal read.)
- **Q9.** Is the masked-identity confirmation enough against link phishing
  (T10), or must linking also require a fresh MCTL login on the helper's
  side (the helper performs an MCTL OAuth device flow itself)?
- **Q10.** Should a keyed fingerprint of the Glovo `customerId` (transit
  `hmac`) be stored, to refuse one Glovo account being linked to two
  `pid`s? It would add a capability and a quasi-identifier.

### 9.2 Go / no-go for C2-1

**Go** when all of these hold:
1. The reviewer has answered Q1-Q10, and the answers are recorded in this
   document. **Holds** since 2026-11-15: the owner's decisions on all ten
   are in 9.3.
2. Vault objects from 3.1 are applied, and proven both ways on the live
   Vault 1.17.2 with a token holding only `mctl-glovo-transit`: encrypt,
   decrypt and rewrap work; `auth/token/renew-self` and `lookup-self` work;
   decrypt with a wrong context fails; read, rotate, config, export, backup
   and datakey on the key, every other key and every other mount return
   403; `glovo-transit/keys/glovo-session/config` reads back
   `auto_rotate_period` and `deletion_allowed=false` (admin token); the
   mount's `audit_non_hmac_request_keys` does not contain `context`.
3. The 3.2 alert and the 5 reconciliation exist and are proven both ways
   (silent on the workload; firing on a test identity's decrypt, and on an
   unreadable window).
4. The NetworkPolicies from 10.3 are in place, including the entry in
   `vault-netpol.yaml`.
5. The kill switch (both layers) and the caps from section 6 are
   implemented and tested **before the first link**. That includes "killed
   means zero Vault decrypts", checked against the Vault audit.
6. The redaction marker-scan, the A/B isolation test (with relink), the
   ciphertext-swap test, the lease test (two replicas or two goroutines on
   one `pid`, exactly one Glovo refresh observed, the loser gets `ErrBusy`
   or the fresh session), the fence tests (a write based on a superseded
   (`link_id`, `version`) lands zero rows; an unlink or relink during an
   in-flight refresh discards its tokens; a relink invalidates another
   replica's cached client even when no refresh happened in between),
   the `unpersisted` pin test (DB down after a successful refresh: the
   client is neither served nor evicted, and it is written once the DB
   returns), the erasure pseudonymisation test, and the audit
   immutability test all pass, each mutation-verified.
7. The breach runbook steps 1-3 were rehearsed against the real Vault with
   a test key and a test row.
8. The `glovo:c2` grant exists for the owner's `pid` only, enforced by
   mctl-api (§8, §9.2).
9. The tailnet ACL for `tag:glovo-egress` (E2) exists, because the link
   validation call in 4.1 already goes through the proxy.

**No-go** if any of these is true:
- a code path writes a session to disk, a log, a trace or an error;
- any identity other than the `mctl-glovo` role has a grant on
  `glovo-transit/` by design;
- the store can be constructed without the kill switch, or a failed read
  of the control row counts as "not killed";
- Vault traffic goes through `secrets.mctl.ai` or any address outside the
  cluster.

### 9.3 Review decisions (owner, 2026-11-15)

Recorded on PR #14 (issue comment of 2026-11-15). Each one is accepted
**for owner-only use**, meaning C2-1 and C2-2 while the owner is the only
linked user. Each one adds a go criterion for C2-3 (9.4).

| Question | Decision | Residual it accepts |
|---|---|---|
| Q1, app RCE exposes every linked session | Accepted while the owner is the only linked user | T3 |
| Q2, plaintext pod-to-Vault traffic (SOC F15) | Accepted for C2-1 and C2-2 | T7 |
| Q4/Q5, platform residuals: the `admin` policy, the policy-writing identities, unseal keys in a Kubernetes Secret | Detection only (the 3.2 alert on any `glovo-transit/` request not made by the `mctl-glovo` role) is accepted for the owner alone | T5, T6, T13 |

The remaining questions were decided in a second PR #14 comment of
2026-11-15. They are design choices rather than accepted residuals, and
the sections named in the table already follow them:

| Question | Decision | Where |
|---|---|---|
| Q3, derived key vs per-`pid` keys | One derived transit key. Erasure means deleting the row, with a 14-day backup tail. To be revisited together with the R4 legal read before C2-3. | 2.1, 2.2, 4.4 |
| Q6, `pid`s in the Vault audit log | No. The transit context stays HMAC'd, and `audit_non_hmac_request_keys` does not include `context`. | 2.2, 3.1, 7 step 6 |
| Q7, advisory lock vs lease | A lease with a compare-and-swap fence. No DB lock or transaction is held across the Glovo refresh. | 2.5, 10.1 |
| Q8, `pid` in audit after erasure | Pseudonymised on erasure, through the `subject_key` mapping. | 2.3, 4.4, 5 |
| Q9, link phishing | The masked-identity confirmation is enough for owner-only C2-1/C2-2. A fresh MCTL login in the helper is a C2-3 criterion. | 4.1, 9.4 |
| Q10, `customerId` fingerprint | None now. Decided at C2-3. | — |

With both sets of decisions, go criterion 1 in 9.2 holds.

### 9.4 Go criteria for C2-3

These join the legal read on R1 and R4 and the new owner decision that
§8 of the parent design already requires. External users stay behind the
flag until every one of them holds:

1. **Broker (from Q1).** Decryption and Glovo calls run in a separate
   broker process, outside the MCP-facing process. Only the broker holds
   the `mctl-glovo` Vault role and the store's database credentials. It
   exposes typed Glovo operations and has no surface that returns a
   session or a token.
2. **Vault TLS listener (from Q2).** Vault serves TLS on its listener.
   Clients use the in-cluster address only, never `secrets.mctl.ai`, and
   the no-go rule in 9.2 still applies.
3. **Unseal keys and provisioner policy writes (from Q4/Q5).** The unseal
   keys are moved out of Kubernetes (no `vault-unseal-keys` Secret in the
   cluster). The `argo-workflow-provisioner` identity can no longer write
   `tenant-*` / `eso-tenant-*` policies with arbitrary content.
4. **Fresh MCTL login in the helper (from Q9).** Before it uploads, the
   local helper requires a fresh MCTL login (an OAuth device flow against
   api.mctl.ai). The upload is accepted only when the code's `pid` equals
   the `pid` of that fresh login. A code the victim did not mint
   themselves can then no longer link their Glovo account to someone
   else's MCTL account.

Q3 (derived key) and Q10 (fingerprint) are revisited at C2-3 as well.
They are reopened decisions, not go criteria.

## 10. Implementation sketch (after review; no code in this PR)

### 10.1 Go interface (`internal/tokenstore`)

```go
// PID is a principal id taken from a verified mctl-api token. The only
// constructor is PIDFrom; a string from a tool argument cannot become one.
type PID struct{ id string }

func PIDFrom(p *authn.Principal) PID

// Lease is a decrypted session. It never renders, marshals or logs its
// contents; it only builds the per-pid Glovo client.
type Lease struct{ /* unexported */ }

func (l *Lease) Client(opts ...glovoapi.Option) (*glovoapi.Client, error)
// LinkID and Version are the pair the lease was decrypted from; a cached
// client is valid only while Status reports the same pair (4.2).
func (l *Lease) LinkID() string
func (l *Lease) Version() int64

// Status is the cheap per-call check: no decrypt, no Vault.
type Status struct {
	State   State  // Active, Suspended, NeedsRelink
	LinkID  string
	Version int64  // from glovo_link_version_seq; never reused
}

type Store interface {
	// CreateLinkCode mints a one-time code for pid and returns it once.
	// consentVersion comes from the consent record mctl-api wrote, never
	// from the request; it is stored with the code and copied into the link.
	CreateLinkCode(ctx context.Context, pid PID, consentVersion string) (code string, expires time.Time, err error)
	// PreviewLink returns the masked identity a code belongs to, without burning it.
	PreviewLink(ctx context.Context, code string) (maskedIdentity string, err error)
	// Link burns code and stores sess for the code's pid, replacing any
	// previous link with a new link_id and a new sequence version. It does
	// not wait for a refresh lease.
	Link(ctx context.Context, code string, sess glovoapi.Session) error
	// Get decrypts pid's session. ErrNotLinked only for a documented absence (no row).
	Get(ctx context.Context, pid PID) (*Lease, error)
	// Update takes pid's refresh lease (ErrBusy if another holder has it),
	// decrypts, runs fn with no DB transaction open, and writes the result
	// fenced on (link_id, version). A lost fence returns ErrSuperseded and
	// the result is discarded. A failed or unknown write pins the result
	// as unpersisted and retries (2.5).
	Update(ctx context.Context, pid PID, fn func(context.Context, glovoapi.Session) (glovoapi.Session, error)) error
	// Unlink deletes pid's link. It works with the kill switch on, needs no
	// Vault, and never waits for a lease.
	Unlink(ctx context.Context, pid PID, reason string) error
	// Erase is Unlink plus the erasure audit event and deletion of pid's
	// glovo_audit_subjects row, in one transaction (5).
	Erase(ctx context.Context, pid PID) error
	// Status reports state, link_id and version without decrypting.
	Status(ctx context.Context, pid PID) (Status, error)
}

var (
	ErrNotLinked    = errors.New("glovo account not linked")      // row absent
	ErrUnavailable  = errors.New("token store unavailable")        // DB or Vault failed; 503
	ErrKilled       = errors.New("glovo c2 is switched off")
	ErrSuspended    = errors.New("glovo link suspended")
	ErrNeedsRelink  = errors.New("glovo session expired; link again")
	ErrBusy         = errors.New("glovo link busy; retry")              // lease held
	ErrSuperseded   = errors.New("glovo link changed during the write")  // fence lost
	ErrIntegrity    = errors.New("glovo link failed integrity check")
	ErrCodeRefused  = errors.New("link code invalid, used or expired")
)
```

`ErrNotLinked` and `ErrUnavailable` are never conflated. A failed read is
`ErrUnavailable`, never "not linked" (the workspace's "could not observe
is never observed absent" rule).

Implementations: `pgvault` (Postgres + transit over HTTP, standard
library client, no Vault SDK unless the reviewer prefers one) and an
in-memory fake for tests that applies the same context binding with a
local AES-GCM key.

### 10.2 Tables

As in 2.3, plus:
- schema migration with a version row checked at start (fail closed on a
  mismatch, the mctl-api `glovo_schema_version` pattern);
- `glovo_store_audit` with the immutability trigger and `INSERT, SELECT`
  only for the app role;
- `glovo_audit_subjects`: `INSERT, SELECT, DELETE` for the app role (DELETE
  is the erasure path, 5);
- `glovo_link_version_seq` owned by the migration role, `USAGE` for the
  app role, never reset by any migration;
- a CHECK that `lease_token`, `lease_until` are both NULL or both set;
- an owner role for migrations, separate from the app role (the
  `glovo_migrator` pattern in mctl-api).

### 10.3 gitops pieces (mctl-gitops)

| Piece | Where | Notes |
|---|---|---|
| Transit mount, key, mount tune | `infrastructure/k3s-preview/cluster-bootstrap/vault-config/README.md`, a new section | Applied by the owner by hand, like every other Vault object there |
| Policy `vault-policy-mctl-glovo-transit.hcl` | same directory | Plus entries in `scripts/vault-policy-expectations.json` and `tests/test_vault_policy_files.py` |
| Kubernetes auth role `mctl-glovo` | README commands | Bound to the dedicated SA only |
| Vault ingress | `platform-gitops/bootstrap/templates/core-infra/vault-netpol.yaml` | A namespace + pod selector entry for mctl-glovo |
| App NetworkPolicy | the service's values | Egress to Vault 8200, Postgres, mctl-api, DNS, and the Tailscale sidecar's needs; nothing else. Ingress from Traefik only. |
| ExternalSecret | the service's values, `tenant-store` SecretStore | **Database credentials only** (from the provisioning workflow's `.../database` path) and the Tailscale auth key (E3). No Vault token, no transit material. |
| ServiceAccount | the service's values | `serviceAccount: {create: true}`, named `mctl-glovo` |
| Alerts | `platform-gitops/infra-components/observability/vm-rules/` | The non-`mctl-glovo` transit access rule, reconciliation `unknown`/mismatch, decrypt cap kill, `GlovoSessionUnpersisted`, `needs_relink` rate |
| Leak guard | `deploy/staged/glovo-m1` README describes a guard that refuses Glovo resources in reconciled paths | The C2-2 PR must extend it deliberately rather than bypass it (**unverified:** how the guard matches) |

## 11. Sources read

mctl-glovo (this repository, `main` at the branch point):
- `CLAUDE.md`; `internal/glovoapi/session.go`, `client.go`, `doc.go`;
  `internal/authn/verifier.go`; `scripts/glovo-relogin.sh`;
  `deploy/mac-mini/README.md`; `go.mod`.
- Epic #11 and its comment; design doc on PR #2
  (`docs/design/real-glovo-accounts.md` on `docs/design-real-glovo-accounts`);
  PR #4 description.

mctl-gitops (`main`):
- `infrastructure/k3s-preview/cluster-bootstrap/vault-config/`:
  `README.md`, `cluster-secret-store.yaml`,
  `vault-policy-external-secrets-read.hcl`, `vault-policy-coolify-mcp.hcl`,
  `vault-policy-mctl-api-erpact-deployer-read.hcl`,
  `vault-policy-argo-workflow-provisioner.hcl`,
  `vault-policy-argo-workflow-teams-rw.hcl`, `vault-policy-developers.hcl`,
  `vault-policy-control.hcl`.
- `platform-gitops/bootstrap/templates/core-infra/vault.yaml`,
  `vault-netpol.yaml`, `vault-auto-unseal.yaml`, `vault-backup.yaml`.
- `platform-gitops/helm-charts/vault-human-auth-iac/iac/audit.tf`,
  `groups.tf`; `docs/runbooks/vault-audit.md`.
- `platform-gitops/infra-components/data/cnpg/shared/object-store.yaml`,
  `scheduled-backup.yaml`.
- `deploy/staged/glovo-m1/README.md`, `secrets-map.md`.
- `docs/soc2/control-matrix.md` (F15).
- Searches: no `transit` secrets engine in mctl-gitops (code search for
  `transit` returned only docs and unrelated files); no Terraform
  `vault_mount` or `vault_kubernetes_auth_backend_role` (Vault objects are
  applied by hand, per the README).

mctl-api (`main`):
- `internal/auth/refreshstore/store.go`, `postgres.go`: refresh tokens
  are stored as SHA-256 hashes (`token_hash BYTEA`) with family-based
  reuse detection and `FOR UPDATE` rotation. That works because mctl-api
  is the *issuer* and only needs to recognise a token. mctl-glovo must
  *present* Glovo's token, so hashing is not an option, and the family
  pattern does not carry over. Its rotation discipline (atomic, refusal
  before commit) does.
- `docs/glovo-connections.md`: the synthetic Glovo connection domain with
  `secret_ref` (`gsr_`), the immutable audit trigger, the 400-day audit
  target, `GLOVO_ENABLED`, and the explicit refusal to claim
  crypto-shredding.

Unverified items, collected:
- Glovo refresh-token rotation and reuse behaviour; refresh-token
  lifetime; whether sign-out or a password change revokes refresh tokens;
  whether Glovo offers "sign out everywhere".
- The mctl-glovo namespace, and whether its database lands on `shared-pg`.
- The content of the hand-written `admin` Vault policy.
- Which pods can assume `argo-workflow-provisioner`.
- Whether `secrets.mctl.ai` is proxied by Cloudflare.
- The channel for user-facing expiry alerts.
- How the glovo-m1 leak guard matches resources.
- Every Vault behaviour named in 2.2-3.1 on the live 1.17.2 (covered by
  go criterion 2).
