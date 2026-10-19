-- 模型增加「最大上下文」字段；列可能已由运维手工补齐，因此用 IF NOT EXISTS 保证幂等。
ALTER TABLE ai_models ADD COLUMN IF NOT EXISTS max_context INT NOT NULL DEFAULT 0;
