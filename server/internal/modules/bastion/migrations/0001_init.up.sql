CREATE TABLE IF NOT EXISTS bastion_assets (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    host        TEXT NOT NULL,
    port        INT  NOT NULL,
    protocol    TEXT NOT NULL CHECK (protocol IN ('ssh','rdp','vnc','mysql','postgres')),
    env         TEXT NOT NULL DEFAULT 'dev',
    description TEXT NOT NULL DEFAULT '',
    host_key    TEXT NOT NULL DEFAULT '',          -- SSH host key (authorized_keys format), TOFU
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS bastion_accounts (
    id          BIGSERIAL PRIMARY KEY,
    asset_id    BIGINT NOT NULL REFERENCES bastion_assets(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    username    TEXT NOT NULL,
    auth_type   TEXT NOT NULL CHECK (auth_type IN ('password','private_key')),
    secret_enc  TEXT NOT NULL,                     -- d.Crypto envelope (password or PEM key)
    passphrase_enc TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (asset_id, name)
);

CREATE TABLE IF NOT EXISTS bastion_rules (
    id           BIGSERIAL PRIMARY KEY,
    name         TEXT NOT NULL,
    subject_type TEXT NOT NULL CHECK (subject_type IN ('user','role')),
    subject      TEXT NOT NULL,                    -- user id (as text) or role name
    asset_ids    BIGINT[] NOT NULL DEFAULT '{}',
    account_ids  BIGINT[] NOT NULL DEFAULT '{}',   -- empty = all accounts of the assets
    expires_at   TIMESTAMPTZ,
    enabled      BOOLEAN NOT NULL DEFAULT true,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS bastion_filters (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL,
    pattern    TEXT NOT NULL,
    action     TEXT NOT NULL CHECK (action IN ('deny','confirm')),
    priority   INT NOT NULL DEFAULT 100,
    enabled    BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS bastion_sessions (
    id             BIGSERIAL PRIMARY KEY,
    user_id        BIGINT NOT NULL,
    username       TEXT NOT NULL,
    asset_id       BIGINT NOT NULL,
    asset_name     TEXT NOT NULL,
    account_id     BIGINT NOT NULL,
    account_user   TEXT NOT NULL,
    protocol       TEXT NOT NULL,
    client_ip      TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL DEFAULT 'active',  -- active|closed|killed|failed
    error          TEXT NOT NULL DEFAULT '',
    recording_path TEXT NOT NULL DEFAULT '',
    started_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at       TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS bastion_sessions_started_idx ON bastion_sessions (started_at DESC);

CREATE TABLE IF NOT EXISTS bastion_commands (
    id         BIGSERIAL PRIMARY KEY,
    session_id BIGINT NOT NULL REFERENCES bastion_sessions(id) ON DELETE CASCADE,
    command    TEXT NOT NULL,
    action     TEXT NOT NULL,                       -- allow|deny|confirmed|cancelled
    rule_id    BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS bastion_commands_session_idx ON bastion_commands (session_id, id);
