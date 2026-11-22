-- 研发中心「交付价值流」：需求 / 缺陷与提交、合并请求、构建、发布、告警的关联（devcenter_links）。
-- 0003 已有同名表 devcenter_links（需求 / 缺陷 ↔ 代码仓库(应用) / 流水线，ref_type app|pipeline），
-- 这里先把它更名为 devcenter_repo_links（数据、约束、序列原样保留），再按价值流契约新建 devcenter_links。
-- 新表与旧表共用序列 devcenter_links_id_seq，两表 id 全局不重复，DELETE /devcenter/links/:id 可无歧义地定位到其中一张表。
DO $$
DECLARE r record;
BEGIN
    IF to_regclass('devcenter_repo_links') IS NULL
       AND EXISTS (SELECT 1 FROM information_schema.columns
                   WHERE table_schema = current_schema() AND table_name = 'devcenter_links' AND column_name = 'ref_type') THEN
        ALTER TABLE devcenter_links RENAME TO devcenter_repo_links;
        ALTER INDEX IF EXISTS devcenter_links_ref_idx RENAME TO devcenter_repo_links_ref_idx;
    END IF;
    -- 主键 / 唯一 / 检查约束（及其索引）随表改名，避免与新表默认约束名 devcenter_links_pkey 冲突。
    IF to_regclass('devcenter_repo_links') IS NOT NULL THEN
        FOR r IN SELECT conname FROM pg_constraint
                 WHERE conrelid = to_regclass('devcenter_repo_links') AND conname LIKE 'devcenter\_links\_%' LOOP
            EXECUTE format('ALTER TABLE devcenter_repo_links RENAME CONSTRAINT %I TO %I',
                           r.conname, 'devcenter_repo_links_' || substr(r.conname, length('devcenter_links_') + 1));
        END LOOP;
    END IF;
END $$;

CREATE SEQUENCE IF NOT EXISTS devcenter_links_id_seq;

CREATE TABLE IF NOT EXISTS devcenter_links (
    id          BIGINT PRIMARY KEY DEFAULT nextval('devcenter_links_id_seq'),
    item_type   TEXT NOT NULL,                      -- requirement | bug
    item_id     BIGINT NOT NULL,
    kind        TEXT NOT NULL,                      -- commit | mr | run | release | alert
    ref         TEXT NOT NULL,                      -- 提交 sha / 合并请求编号 / cicd_runs.id / cicd_releases.id / 告警 id
    app_id      BIGINT,
    title       TEXT NOT NULL DEFAULT '',
    url         TEXT NOT NULL DEFAULT '',
    author      TEXT NOT NULL DEFAULT '',
    env         TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT '',
    occurred_at TIMESTAMPTZ,
    source      TEXT NOT NULL DEFAULT 'auto',       -- auto | manual
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (item_type, item_id, kind, ref)
);
CREATE INDEX IF NOT EXISTS devcenter_links_item_idx ON devcenter_links (item_type, item_id);
CREATE INDEX IF NOT EXISTS devcenter_links_kind_ref_idx ON devcenter_links (kind, ref);
