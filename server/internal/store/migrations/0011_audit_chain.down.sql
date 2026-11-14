-- 回滚 v1.30 审计链：逆序删除备份记录、归档清单、链段表与 audit_logs 链字段（哈希可由数据重算）。
DROP TABLE IF EXISTS platform_backups;
DROP TABLE IF EXISTS audit_archives;
DROP TABLE IF EXISTS audit_chain_segments;
DROP INDEX IF EXISTS audit_logs_unchained;
ALTER TABLE audit_logs DROP COLUMN IF EXISTS hash;
ALTER TABLE audit_logs DROP COLUMN IF EXISTS prev_hash;
