-- 研发中心补充字段：处理人、扩展数据（验收标准、起止日期、执行结果等）、最后修改人。
ALTER TABLE devcenter_iterations   ADD COLUMN IF NOT EXISTS assignee TEXT NOT NULL DEFAULT '', ADD COLUMN IF NOT EXISTS data JSONB NOT NULL DEFAULT '{}'::jsonb, ADD COLUMN IF NOT EXISTS updated_by TEXT NOT NULL DEFAULT '';
ALTER TABLE devcenter_requirements ADD COLUMN IF NOT EXISTS assignee TEXT NOT NULL DEFAULT '', ADD COLUMN IF NOT EXISTS data JSONB NOT NULL DEFAULT '{}'::jsonb, ADD COLUMN IF NOT EXISTS updated_by TEXT NOT NULL DEFAULT '';
ALTER TABLE devcenter_bugs         ADD COLUMN IF NOT EXISTS assignee TEXT NOT NULL DEFAULT '', ADD COLUMN IF NOT EXISTS data JSONB NOT NULL DEFAULT '{}'::jsonb, ADD COLUMN IF NOT EXISTS updated_by TEXT NOT NULL DEFAULT '';
ALTER TABLE devcenter_tests        ADD COLUMN IF NOT EXISTS assignee TEXT NOT NULL DEFAULT '', ADD COLUMN IF NOT EXISTS data JSONB NOT NULL DEFAULT '{}'::jsonb, ADD COLUMN IF NOT EXISTS updated_by TEXT NOT NULL DEFAULT '';
-- 旧版默认状态 todo 在各类型中都保留为第一个状态，无需迁移数据
CREATE INDEX IF NOT EXISTS devcenter_req_iter_idx ON devcenter_requirements(iteration_id);
CREATE INDEX IF NOT EXISTS devcenter_bug_iter_idx ON devcenter_bugs(iteration_id);
