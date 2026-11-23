-- 注意：回滚会丢掉审核状态、邀请关系和验证记录。回滚后的旧代码不检查状态，待审核 / 已拒绝的账号将能直接登录。
-- 删除列时，列上的索引和外键约束一并删除。
ALTER TABLE users DROP COLUMN IF EXISTS invite_code_id;
ALTER TABLE users DROP COLUMN IF EXISTS status;
DROP TABLE IF EXISTS invite_verifications;
DROP TABLE IF EXISTS invite_codes;
