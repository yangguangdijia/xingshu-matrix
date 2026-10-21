-- 协议专属参数（资产组、标签、域名、TLS、库名、只读、跳板、来源）。
-- 只存非敏感配置；密码 / 私钥 / passphrase / kubeconfig / token 由 assetIn.check 拒绝写入本列。
ALTER TABLE bastion_assets ADD COLUMN IF NOT EXISTS options JSONB NOT NULL DEFAULT '{}'::jsonb;
