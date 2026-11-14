-- 回滚 0007（迁移器只执行 *.up.sql，本文件供人工回滚使用）
DROP TABLE IF EXISTS bastion_acl_exemptions;
UPDATE bastion_login_acls SET action='deny' WHERE action='review';
ALTER TABLE bastion_login_acls DROP CONSTRAINT IF EXISTS bastion_login_acls_action_check;
ALTER TABLE bastion_login_acls ADD CONSTRAINT bastion_login_acls_action_check CHECK (action IN ('allow','deny'));
ALTER TABLE bastion_login_acls DROP CONSTRAINT IF EXISTS bastion_login_acls_exempt_minutes_check;
ALTER TABLE bastion_login_acls DROP COLUMN IF EXISTS exempt_minutes;
