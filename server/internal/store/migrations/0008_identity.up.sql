-- v1.29 身份与令牌：服务账号、API 访问令牌、TOTP 两步验证。

-- 服务账号是 users 里 account_type='service' 的行：无密码、不能交互登录，归属创建人（owner_id）。
ALTER TABLE users ADD COLUMN IF NOT EXISTS account_type TEXT NOT NULL DEFAULT 'human';
ALTER TABLE users ADD COLUMN IF NOT EXISTS owner_id BIGINT REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE users ADD COLUMN IF NOT EXISTS description TEXT NOT NULL DEFAULT '';
DO $$ BEGIN
    ALTER TABLE users ADD CONSTRAINT users_account_type_chk CHECK (account_type IN ('human','service'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- 个人访问令牌 / 服务账号令牌：只存 SHA-256 摘要；prefix（xsm_ + 8 位）用于界面识别。
-- scopes 为空表示继承所有者的全部权限；非空时生效权限 = 所有者当前权限 ∩ scopes。
CREATE TABLE IF NOT EXISTS api_tokens (
    id           BIGSERIAL   PRIMARY KEY,
    user_id      BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT        NOT NULL,
    prefix       TEXT        NOT NULL,
    token_hash   TEXT        NOT NULL UNIQUE,
    scopes       TEXT[]      NOT NULL DEFAULT '{}',
    expires_at   TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    last_used_ip TEXT        NOT NULL DEFAULT '',
    created_by   BIGINT      REFERENCES users(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at   TIMESTAMPTZ,
    revoked_by   BIGINT
);
CREATE INDEX IF NOT EXISTS api_tokens_user ON api_tokens (user_id, created_at DESC);

-- TOTP 两步验证：密钥 AES-GCM 加密；last_step 防同一时间窗重放；恢复码只存摘要。
CREATE TABLE IF NOT EXISTS user_mfa (
    user_id         BIGINT      PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    secret_enc      TEXT        NOT NULL,
    enabled         BOOLEAN     NOT NULL DEFAULT FALSE,
    last_step       BIGINT      NOT NULL DEFAULT 0,
    recovery_hashes TEXT[]      NOT NULL DEFAULT '{}',
    enabled_at      TIMESTAMPTZ,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 密码校验通过后的短期两步验证票据（多副本共享，只存摘要，5 分钟有效，最多 5 次尝试）。
CREATE TABLE IF NOT EXISTS mfa_challenges (
    ticket_hash    TEXT        PRIMARY KEY,
    user_id        BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    purpose        TEXT        NOT NULL CHECK (purpose IN ('verify','enroll')),
    pending_secret TEXT        NOT NULL DEFAULT '',
    attempts       INTEGER     NOT NULL DEFAULT 0,
    ip             TEXT        NOT NULL DEFAULT '',
    user_agent     TEXT        NOT NULL DEFAULT '',
    expires_at     TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS mfa_challenges_expires ON mfa_challenges (expires_at);
