-- v1.29 后台任务队列：PostgreSQL 表 + FOR UPDATE SKIP LOCKED 认领 + 租约超时回收。
-- 状态机：queued →(认领) running →(成功) succeeded
--                                  →(失败且未用尽次数) failed →(到 run_at 再认领) running
--                                  →(失败且用尽次数 / 永久错误) dead
--         failed / dead →(人工重试) queued；running 租约过期由回收器转 failed 或 dead。
CREATE TABLE IF NOT EXISTS platform_jobs (
    id           BIGSERIAL   PRIMARY KEY,
    type         TEXT        NOT NULL CHECK (type <> '' AND length(type) <= 128),
    payload      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    status       TEXT        NOT NULL DEFAULT 'queued'
                 CHECK (status IN ('queued','running','succeeded','failed','dead')),
    priority     INT         NOT NULL DEFAULT 0,
    attempts     INT         NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts INT         NOT NULL DEFAULT 5 CHECK (max_attempts >= 1),
    run_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_by    TEXT        NOT NULL DEFAULT '',
    locked_until TIMESTAMPTZ,
    last_error   TEXT        NOT NULL DEFAULT '',
    dedupe_key   TEXT,
    created_by   BIGINT      REFERENCES users(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at   TIMESTAMPTZ,
    finished_at  TIMESTAMPTZ
);

-- 认领：只扫描可运行行。
CREATE INDEX IF NOT EXISTS platform_jobs_ready ON platform_jobs (priority DESC, run_at, id)
    WHERE status IN ('queued','failed');
-- 回收：运行中且租约到期。
CREATE INDEX IF NOT EXISTS platform_jobs_lease ON platform_jobs (locked_until) WHERE status = 'running';
-- 列表筛选。
CREATE INDEX IF NOT EXISTS platform_jobs_status_type ON platform_jobs (status, type, id DESC);
-- 去重：同一 dedupe_key 同时只允许一个未结束任务（queued/running/failed）。
CREATE UNIQUE INDEX IF NOT EXISTS platform_jobs_dedupe ON platform_jobs (dedupe_key)
    WHERE dedupe_key IS NOT NULL AND status IN ('queued','running','failed');
