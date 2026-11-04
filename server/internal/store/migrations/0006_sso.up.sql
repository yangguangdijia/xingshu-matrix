-- 企业 SSO（OIDC 授权码 + PKCE）：一次性 state 与一次性登录票据。
-- 都放数据库而不是进程内存，多副本部署时回调可以落到任意副本。
CREATE TABLE IF NOT EXISTS sso_states (
    state_hash  TEXT        PRIMARY KEY,
    verifier    TEXT        NOT NULL,
    nonce       TEXT        NOT NULL,
    redirect    TEXT        NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL
);
CREATE TABLE IF NOT EXISTS sso_tickets (
    ticket_hash TEXT        PRIMARY KEY,
    user_id     BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    ip          TEXT        NOT NULL DEFAULT '',
    user_agent  TEXT        NOT NULL DEFAULT '',
    expires_at  TIMESTAMPTZ NOT NULL
);
