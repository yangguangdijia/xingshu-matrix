-- 命令过滤规则（动作为 approve）的审批等待超时（分钟）：1–1440，0 表示使用堡垒机全局默认（settings: bastion.settings）。
ALTER TABLE bastion_filters ADD COLUMN IF NOT EXISTS approval_timeout_minutes INT NOT NULL DEFAULT 0;
ALTER TABLE bastion_filters DROP CONSTRAINT IF EXISTS bastion_filters_approval_timeout_check;
ALTER TABLE bastion_filters ADD CONSTRAINT bastion_filters_approval_timeout_check CHECK (approval_timeout_minutes BETWEEN 0 AND 1440);
