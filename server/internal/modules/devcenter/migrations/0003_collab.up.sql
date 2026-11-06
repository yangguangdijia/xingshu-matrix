-- 研发中心协作：评论、变更记录（状态 / 指派 / 迭代）、代码仓库与流水线关联、需求 AI 开发任务。
-- 评论 / 记录 / 关联按 (kind,item_id) 指向四类对象之一，删除对象时由服务端在同一事务内清理。
CREATE TABLE IF NOT EXISTS devcenter_comments (
    id         BIGSERIAL PRIMARY KEY,
    kind       TEXT NOT NULL,
    item_id    BIGINT NOT NULL,
    body       TEXT NOT NULL,
    author_id  BIGINT NOT NULL DEFAULT 0,
    author     TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS devcenter_comments_item_idx ON devcenter_comments(kind, item_id, id);
CREATE INDEX IF NOT EXISTS devcenter_comments_author_idx ON devcenter_comments(author, kind);

CREATE TABLE IF NOT EXISTS devcenter_activities (
    id         BIGSERIAL PRIMARY KEY,
    kind       TEXT NOT NULL,
    item_id    BIGINT NOT NULL,
    actor      TEXT NOT NULL DEFAULT '',
    action     TEXT NOT NULL,              -- create | status | assign | iteration
    from_value TEXT NOT NULL DEFAULT '',
    to_value   TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS devcenter_activities_item_idx ON devcenter_activities(kind, item_id, id);
CREATE INDEX IF NOT EXISTS devcenter_activities_flow_idx ON devcenter_activities(kind, action, created_at);
CREATE INDEX IF NOT EXISTS devcenter_activities_actor_idx ON devcenter_activities(actor, kind);

CREATE TABLE IF NOT EXISTS devcenter_links (
    id         BIGSERIAL PRIMARY KEY,
    kind       TEXT NOT NULL,
    item_id    BIGINT NOT NULL,
    ref_type   TEXT NOT NULL CHECK (ref_type IN ('app', 'pipeline')),
    ref_id     BIGINT NOT NULL,
    ref_name   TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (kind, item_id, ref_type, ref_id)
);
CREATE INDEX IF NOT EXISTS devcenter_links_ref_idx ON devcenter_links(ref_type, ref_id);

CREATE TABLE IF NOT EXISTS devcenter_ai_tasks (
    id             BIGSERIAL PRIMARY KEY,
    requirement_id BIGINT NOT NULL REFERENCES devcenter_requirements(id) ON DELETE CASCADE,
    model_id       BIGINT NOT NULL DEFAULT 0,
    model_name     TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL DEFAULT 'running',  -- running | succeeded | failed
    note           TEXT NOT NULL DEFAULT '',
    result         JSONB NOT NULL DEFAULT '{}'::jsonb,
    error          TEXT NOT NULL DEFAULT '',
    applied        JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_by     TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS devcenter_ai_tasks_req_idx ON devcenter_ai_tasks(requirement_id, id DESC);

-- 历史数据基线：已有需求 / 缺陷补一条创建记录；当前不在初始状态的再补一条状态记录（时间取最后更新时间，统计为近似值）。
INSERT INTO devcenter_activities(kind, item_id, actor, action, to_value, created_at)
    SELECT 'requirements', id, updated_by, 'create', 'todo', created_at FROM devcenter_requirements;
INSERT INTO devcenter_activities(kind, item_id, actor, action, from_value, to_value, created_at)
    SELECT 'requirements', id, updated_by, 'status', 'todo', status, updated_at FROM devcenter_requirements WHERE status <> 'todo';
INSERT INTO devcenter_activities(kind, item_id, actor, action, to_value, created_at)
    SELECT 'bugs', id, updated_by, 'create', 'todo', created_at FROM devcenter_bugs;
INSERT INTO devcenter_activities(kind, item_id, actor, action, from_value, to_value, created_at)
    SELECT 'bugs', id, updated_by, 'status', 'todo', status, updated_at FROM devcenter_bugs WHERE status <> 'todo';
