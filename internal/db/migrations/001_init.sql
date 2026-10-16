-- Calendar Dynamics schema.
-- Every tenant-owned table carries business_id, and every query in
-- internal/store filters on it. Timestamps are fixed-width UTC text.

CREATE TABLE businesses (
    id                   TEXT PRIMARY KEY,
    name                 TEXT NOT NULL,
    trade                TEXT NOT NULL DEFAULT '',
    timezone             TEXT NOT NULL DEFAULT 'America/Chicago',
    status               TEXT NOT NULL DEFAULT 'onboarding'
                         CHECK (status IN ('onboarding', 'active', 'suspended', 'deleted')),
    plan                 TEXT NOT NULL DEFAULT 'solo',
    deployment           TEXT NOT NULL DEFAULT 'shared',      -- 'shared' or 'dedicated'
    quiet_start          INTEGER NOT NULL DEFAULT 20,          -- no automated texts from 8pm...
    quiet_end            INTEGER NOT NULL DEFAULT 8,           -- ...until 8am, business local time
    owner_name           TEXT NOT NULL DEFAULT '',
    owner_email          TEXT NOT NULL DEFAULT '',
    owner_phone          TEXT NOT NULL DEFAULT '',
    sms_from             TEXT NOT NULL DEFAULT '',             -- business's own number once A2P is approved
    a2p_status           TEXT NOT NULL DEFAULT 'not_started'
                         CHECK (a2p_status IN ('not_started', 'submitted', 'approved', 'rejected')),
    owner_verified_at    TEXT,
    verify_code_hash     TEXT,
    verify_expires_at    TEXT,
    payment_link_sent_at TEXT,
    wrapped_key          BLOB,                                 -- per-business data key; NULL once deleted (crypto-shred)
    created_at           TEXT NOT NULL,
    deleted_at           TEXT
);
CREATE UNIQUE INDEX businesses_sms_from ON businesses (sms_from) WHERE sms_from <> '';

CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    business_id   TEXT NOT NULL REFERENCES businesses (id),
    email         TEXT NOT NULL UNIQUE COLLATE NOCASE,
    name          TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'owner' CHECK (role IN ('owner', 'member')),
    created_at    TEXT NOT NULL
);

CREATE TABLE sessions (
    token_hash  TEXT PRIMARY KEY,                 -- sha256 of the cookie value; raw tokens are never stored
    user_id     TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    business_id TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

CREATE TABLE invites (
    token_hash  TEXT PRIMARY KEY,
    business_id TEXT NOT NULL REFERENCES businesses (id),
    email       TEXT NOT NULL,
    name        TEXT NOT NULL DEFAULT '',
    created_by  TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    used_at     TEXT,
    created_at  TEXT NOT NULL
);

CREATE TABLE contacts (
    id                 TEXT PRIMARY KEY,
    business_id        TEXT NOT NULL REFERENCES businesses (id),
    name               TEXT NOT NULL,
    phone              TEXT NOT NULL DEFAULT '',     -- E.164, e.g. +18475550142
    email              TEXT NOT NULL DEFAULT '',
    address            TEXT NOT NULL DEFAULT '',
    sms_consent_at     TEXT,                          -- when the customer agreed to receive texts
    sms_consent_source TEXT NOT NULL DEFAULT '',
    opted_out_at       TEXT,                          -- replied STOP
    created_at         TEXT NOT NULL
);
CREATE INDEX contacts_business_name ON contacts (business_id, name COLLATE NOCASE);
CREATE INDEX contacts_phone ON contacts (phone) WHERE phone <> '';

CREATE TABLE events (
    id            TEXT PRIMARY KEY,
    business_id   TEXT NOT NULL REFERENCES businesses (id),
    contact_id    TEXT NOT NULL REFERENCES contacts (id),
    type          TEXT NOT NULL CHECK (type IN ('job', 'estimate', 'followup', 'call', 'note')),
    title         TEXT NOT NULL,
    start_at      TEXT NOT NULL,
    duration_mins INTEGER NOT NULL DEFAULT 60,
    notes_enc     BLOB,                               -- encrypted with the business's key
    channel       TEXT NOT NULL DEFAULT '' CHECK (channel IN ('', 'sms', 'email')),
    message_enc   BLOB,                               -- follow-up message body, encrypted
    send_status   TEXT NOT NULL DEFAULT ''
                  CHECK (send_status IN ('', 'scheduled', 'sent', 'skipped', 'failed')),
    send_note     TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);
CREATE INDEX events_business_start ON events (business_id, start_at);
CREATE INDEX events_business_contact ON events (business_id, contact_id, start_at);

CREATE TABLE messages (
    id              TEXT PRIMARY KEY,
    business_id     TEXT NOT NULL REFERENCES businesses (id),
    contact_id      TEXT NOT NULL DEFAULT '',
    event_id        TEXT NOT NULL DEFAULT '',
    channel         TEXT NOT NULL CHECK (channel IN ('sms', 'email')),
    direction       TEXT NOT NULL CHECK (direction IN ('out', 'in')),
    body_enc        BLOB,
    provider_id     TEXT NOT NULL DEFAULT '',
    -- pending -> sending -> sent; 'unknown' means the process died mid-send,
    -- so we deliberately do not resend (no duplicate texts).
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'sending', 'sent', 'failed', 'unknown', 'received')),
    error           TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT UNIQUE,
    created_at      TEXT NOT NULL
);
CREATE INDEX messages_business_contact ON messages (business_id, contact_id, created_at);

-- Durable job queue (see internal/jobs).
CREATE TABLE jobs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    queue        TEXT NOT NULL,
    kind         TEXT NOT NULL,
    args         TEXT NOT NULL,
    run_at       TEXT NOT NULL,
    attempts     INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 8,
    state        TEXT NOT NULL DEFAULT 'available'
                 CHECK (state IN ('available', 'running', 'completed', 'failed', 'cancelled')),
    unique_key   TEXT,
    last_error   TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    finished_at  TEXT
);
CREATE UNIQUE INDEX jobs_unique_active ON jobs (unique_key)
    WHERE unique_key IS NOT NULL AND state IN ('available', 'running');
CREATE INDEX jobs_ready ON jobs (queue, state, run_at);

-- Staff (Calendar Dynamics employees) are completely separate from customer users.
CREATE TABLE staff_users (
    id            TEXT PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE COLLATE NOCASE,
    name          TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL CHECK (role IN ('owner', 'onboarding', 'support', 'billing')),
    created_at    TEXT NOT NULL
);

CREATE TABLE staff_sessions (
    token_hash TEXT PRIMARY KEY,
    staff_id   TEXT NOT NULL REFERENCES staff_users (id) ON DELETE CASCADE,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL
);

-- Time-limited, reasoned access by staff to a business's customer data.
CREATE TABLE support_access (
    id          TEXT PRIMARY KEY,
    business_id TEXT NOT NULL REFERENCES businesses (id),
    staff_id    TEXT NOT NULL REFERENCES staff_users (id),
    staff_name  TEXT NOT NULL,
    reason      TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

CREATE TABLE audit_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    at          TEXT NOT NULL,
    actor_type  TEXT NOT NULL CHECK (actor_type IN ('user', 'staff', 'system')),
    actor_id    TEXT NOT NULL DEFAULT '',
    business_id TEXT NOT NULL DEFAULT '',
    action      TEXT NOT NULL,
    detail      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX audit_log_business ON audit_log (business_id, at);

CREATE TRIGGER audit_log_no_update BEFORE UPDATE ON audit_log
BEGIN SELECT RAISE(ABORT, 'audit_log is append-only'); END;
CREATE TRIGGER audit_log_no_delete BEFORE DELETE ON audit_log
BEGIN SELECT RAISE(ABORT, 'audit_log is append-only'); END;
