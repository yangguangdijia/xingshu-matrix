-- v1.30 审计链：audit_logs 哈希链字段、链段登记、审计归档清单与平台备份记录。

-- 哈希链：hash = SHA-256(规范化(prev_hash, 行内容))；空串表示尚未入链（由后台链接器补齐）。
ALTER TABLE audit_logs ADD COLUMN IF NOT EXISTS prev_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_logs ADD COLUMN IF NOT EXISTS hash      TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS audit_logs_unchained ON audit_logs (id) WHERE hash = '';

-- 链段：live 为当前持续追加的链；legacy 为启用前历史行的一次性封存段。
CREATE TABLE IF NOT EXISTS audit_chain_segments (
    id          BIGSERIAL   PRIMARY KEY,
    kind        TEXT        NOT NULL UNIQUE CHECK (kind IN ('live','legacy')),
    status      TEXT        NOT NULL DEFAULT 'active',
    start_id    BIGINT,
    end_id      BIGINT,
    anchor_hash TEXT        NOT NULL DEFAULT repeat('0', 64),
    head_id     BIGINT,
    head_hash   TEXT        NOT NULL DEFAULT '',
    row_count   BIGINT      NOT NULL DEFAULT 0,
    note        TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO audit_chain_segments (kind, status) VALUES ('live', 'active')
ON CONFLICT (kind) DO NOTHING;

-- 归档清单：每个导出到对象存储/本地的审计批次一行，记录首尾 id 与哈希以便跨归档续链校验。
CREATE TABLE IF NOT EXISTS audit_archives (
    id              BIGSERIAL   PRIMARY KEY,
    source          TEXT        NOT NULL CHECK (source IN ('audit_logs','bastion_commands')),
    first_id        BIGINT      NOT NULL,
    last_id         BIGINT      NOT NULL,
    row_count       BIGINT      NOT NULL DEFAULT 0,
    first_prev_hash TEXT        NOT NULL DEFAULT '',
    last_hash       TEXT        NOT NULL DEFAULT '',
    min_created_at  TIMESTAMPTZ,
    max_created_at  TIMESTAMPTZ,
    store_kind      TEXT        NOT NULL DEFAULT '',
    object_key      TEXT        NOT NULL DEFAULT '',
    location        TEXT        NOT NULL DEFAULT '',
    size            BIGINT      NOT NULL DEFAULT 0,
    sha256          TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS audit_archives_source ON audit_archives (source, last_id DESC);

-- 平台备份记录：schedule_slot 唯一，保证同一调度时段只生成一次定时备份。
CREATE TABLE IF NOT EXISTS platform_backups (
    id            BIGSERIAL   PRIMARY KEY,
    kind          TEXT        NOT NULL CHECK (kind IN ('manual','scheduled','pre_migrate')),
    status        TEXT        NOT NULL DEFAULT 'running' CHECK (status IN ('running','succeeded','failed')),
    file          TEXT        NOT NULL DEFAULT '',
    size          BIGINT      NOT NULL DEFAULT 0,
    sha256        TEXT        NOT NULL DEFAULT '',
    schedule_slot TEXT        UNIQUE,
    error         TEXT        NOT NULL DEFAULT '',
    created_by    BIGINT      REFERENCES users(id) ON DELETE SET NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS platform_backups_created ON platform_backups (created_at DESC);
