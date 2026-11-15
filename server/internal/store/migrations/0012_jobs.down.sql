-- 回滚 v1.29 后台任务队列（任务记录随表一并删除）。
DROP INDEX IF EXISTS platform_jobs_dedupe;
DROP INDEX IF EXISTS platform_jobs_status_type;
DROP INDEX IF EXISTS platform_jobs_lease;
DROP INDEX IF EXISTS platform_jobs_ready;
DROP TABLE IF EXISTS platform_jobs;
