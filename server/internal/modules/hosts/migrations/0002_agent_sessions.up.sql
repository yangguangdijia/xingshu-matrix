-- Multi-replica host agents: which server replica holds each agent's
-- WebSocket (lease renewed by heartbeat), plus the HMAC key replicas use to
-- authenticate internal relay requests to each other.
CREATE TABLE IF NOT EXISTS hosts_agent_sessions (
    host_id    BIGINT PRIMARY KEY REFERENCES hosts_hosts(id) ON DELETE CASCADE,
    session_id VARCHAR(64)  NOT NULL,
    owner_addr VARCHAR(256) NOT NULL DEFAULT '',
    claimed_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
    seen_at    TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS hosts_agent_sessions_seen_idx ON hosts_agent_sessions (seen_at);

CREATE TABLE IF NOT EXISTS hosts_relay_keys (
    name       VARCHAR(32) PRIMARY KEY,
    secret_enc TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
