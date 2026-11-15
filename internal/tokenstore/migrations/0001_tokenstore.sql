-- Token store schema, version 1 (docs/design/token-store.md 2.3, 5, 6, 10.2).
-- Applied by the migration owner role, never by the app role. Postgres
-- holds ciphertext and metadata only: no column holds a token, a Glovo
-- customer id, an address or any other Glovo data in clear.

CREATE TABLE glovo_schema_version (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    version   integer NOT NULL
);

-- version in glovo_links comes from this sequence on every write. It is
-- therefore monotonic per pid across relinks and across unlink + relink
-- (the row is deleted at unlink, the sequence is not), so a cached
-- (link_id, version) pair can never match a later link.
CREATE SEQUENCE glovo_link_version_seq AS bigint START 1 NO CYCLE;

CREATE TABLE glovo_links (
    pid                  text PRIMARY KEY CHECK (pid ~ '^prn_[A-Za-z0-9_-]{1,120}$'),
    link_id              text NOT NULL UNIQUE CHECK (link_id ~ '^gln_[0-9A-Z]{26}$'),
    state                text NOT NULL CHECK (state IN ('active', 'suspended', 'needs_relink')),
    ciphertext           text NOT NULL CHECK (ciphertext ~ '^vault:v[1-9][0-9]*:'),
    key_version          integer NOT NULL CHECK (key_version > 0),
    version              bigint NOT NULL,
    write_id             text NOT NULL,
    consent_version      text NOT NULL CHECK (consent_version <> ''),
    linked_at            timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    last_used_at         timestamptz,
    last_refresh_at      timestamptz,
    access_expires_at    timestamptz,
    refresh_failures     integer NOT NULL DEFAULT 0 CHECK (refresh_failures >= 0),
    -- Q7: a refresh lease taken in a short transaction. The CAS on version
    -- plus the holder check at commit is the fence; nothing holds a
    -- transaction or a lock across the Glovo call.
    lease_token text,
    lease_until  timestamptz,
    CHECK ((lease_token IS NULL) = (lease_until IS NULL))
);

CREATE TABLE glovo_link_codes (
    code_hash       bytea PRIMARY KEY CHECK (octet_length(code_hash) = 32),
    pid             text NOT NULL,
    consent_version text NOT NULL CHECK (consent_version <> ''),
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    used_at         timestamptz
);
CREATE INDEX glovo_link_codes_live ON glovo_link_codes (pid) WHERE used_at IS NULL;

-- One row. Ships killed: an operator turns C2 on deliberately.
CREATE TABLE glovo_control (
    singleton  boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    c2_killed  boolean NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    updated_by text NOT NULL
);
INSERT INTO glovo_control (c2_killed, updated_by) VALUES (true, 'migration');

-- Q8: audit rows reference a subject, not a pid. Erasure clears the pid
-- here, which pseudonymises every audit row of that subject without
-- touching the insert-only audit table.
CREATE TABLE glovo_audit_subjects (
    subject_id text PRIMARY KEY CHECK (subject_id ~ '^gas_[0-9A-Z]{26}$'),
    pid        text UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    erased_at  timestamptz,
    CHECK ((pid IS NULL) = (erased_at IS NOT NULL))
);

CREATE TABLE glovo_store_audit (
    id         text PRIMARY KEY CHECK (id ~ '^gsa_[0-9A-Z]{26}$'),
    at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    subject_id text REFERENCES glovo_audit_subjects (subject_id),
    link_id    text,
    event      text NOT NULL CHECK (event IN (
        'link_code_issued', 'link_preview', 'link', 'link_refused', 'decrypt',
        'refresh', 'persist_retry', 'persist_discarded', 'rewrap', 'unlink', 'erasure', 'suspend',
        'resume', 'kill_on', 'kill_off', 'integrity_error', 'cap_exceeded')),
    outcome    text NOT NULL CHECK (outcome IN ('ok', 'refused', 'error')),
    reason     text NOT NULL CHECK (reason ~ '^[a-z0-9_]{1,64}$'),
    actor_kind text NOT NULL CHECK (actor_kind IN ('user', 'service', 'operator')),
    request_id text
);

CREATE FUNCTION glovo_store_audit_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'glovo_store_audit is insert-only';
END;
$$;

CREATE TRIGGER glovo_store_audit_no_update_delete
    BEFORE UPDATE OR DELETE ON glovo_store_audit
    FOR EACH ROW EXECUTE FUNCTION glovo_store_audit_immutable();

CREATE TRIGGER glovo_store_audit_no_truncate
    BEFORE TRUNCATE ON glovo_store_audit
    FOR EACH STATEMENT EXECUTE FUNCTION glovo_store_audit_immutable();

REVOKE ALL ON glovo_links, glovo_link_codes, glovo_control, glovo_audit_subjects,
    glovo_store_audit, glovo_schema_version FROM PUBLIC;

-- Grants for the app role are applied with the role itself (C2-2):
--   GRANT SELECT, INSERT, UPDATE, DELETE ON glovo_links, glovo_link_codes TO <app>;
--   GRANT SELECT ON glovo_control, glovo_schema_version TO <app>;
--   GRANT UPDATE (c2_killed, updated_at, updated_by) ON glovo_control TO <app>;
--   GRANT SELECT, INSERT, UPDATE (pid, erased_at) ON glovo_audit_subjects TO <app>;
--   GRANT SELECT, INSERT ON glovo_store_audit TO <app>;
--   GRANT USAGE ON SEQUENCE glovo_link_version_seq TO <app>;

INSERT INTO glovo_schema_version (version) VALUES (1);
