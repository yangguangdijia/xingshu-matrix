-- 回滚 0004：删除价值流关联表，并把 devcenter_repo_links 恢复为 0003 的 devcenter_links。
-- 只有在 devcenter_repo_links 存在（即 0004 已执行）时才动表，重复执行或在 0004 之前执行均无副作用。
DO $$
DECLARE r record;
BEGIN
    IF to_regclass('devcenter_repo_links') IS NOT NULL THEN
        DROP TABLE IF EXISTS devcenter_links;
        FOR r IN SELECT conname FROM pg_constraint
                 WHERE conrelid = to_regclass('devcenter_repo_links') AND conname LIKE 'devcenter\_repo\_links\_%' LOOP
            EXECUTE format('ALTER TABLE devcenter_repo_links RENAME CONSTRAINT %I TO %I',
                           r.conname, 'devcenter_links_' || substr(r.conname, length('devcenter_repo_links_') + 1));
        END LOOP;
        ALTER TABLE devcenter_repo_links RENAME TO devcenter_links;
        ALTER INDEX IF EXISTS devcenter_repo_links_ref_idx RENAME TO devcenter_links_ref_idx;
    END IF;
END $$;
