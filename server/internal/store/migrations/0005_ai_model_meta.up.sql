-- 模型补充元数据：用途标签与备注，便于同一供应商下区分多个模型配置。
ALTER TABLE ai_models ADD COLUMN IF NOT EXISTS tags TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE ai_models ADD COLUMN IF NOT EXISTS note TEXT NOT NULL DEFAULT '';
