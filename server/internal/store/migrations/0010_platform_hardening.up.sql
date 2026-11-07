-- v1.29 平台加固：WebSocket/SSE 一次性票据与写请求幂等键。

-- 一次性连接票据：只存票据 SHA-256 摘要；绑定用户与会话；30 秒有效，兑换即删除。
-- token_enc 为签发时调用方访问令牌的加密密文（AES-GCM，平台主密钥），用于兑换时复用原会话鉴权。
CREATE TABLE IF NOT EXISTS ws_tickets (
    ticket_hash TEXT        PRIMARY KEY,
    user_id     BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    session_id  TEXT        NOT NULL,
    token_enc   TEXT        NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS ws_tickets_expires ON ws_tickets (expires_at);

-- 幂等键：按 (调用方, 方法+路径, 键) 唯一；处理中 state='processing'，完成后保存首次响应，24 小时过期。
CREATE TABLE IF NOT EXISTS idempotency_keys (
    scope           TEXT        NOT NULL,
    route           TEXT        NOT NULL,
    idem_key        TEXT        NOT NULL,
    request_hash    TEXT        NOT NULL,
    state           TEXT        NOT NULL DEFAULT 'processing' CHECK (state IN ('processing','done')),
    response_status INTEGER     NOT NULL DEFAULT 0,
    content_type    TEXT        NOT NULL DEFAULT '',
    response_body   BYTEA,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (scope, route, idem_key)
);
CREATE INDEX IF NOT EXISTS idempotency_keys_expires ON idempotency_keys (expires_at);
