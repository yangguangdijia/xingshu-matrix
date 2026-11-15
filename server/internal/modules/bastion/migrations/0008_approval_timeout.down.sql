-- 回滚 0008（迁移器只执行 *.up.sql，本文件供人工回滚使用）
ALTER TABLE bastion_filters DROP CONSTRAINT IF EXISTS bastion_filters_approval_timeout_check;
ALTER TABLE bastion_filters DROP COLUMN IF EXISTS approval_timeout_minutes;
