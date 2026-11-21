DROP INDEX IF EXISTS idx_users_username_lower;

-- 注意：如果表里已经存在重名用户，恢复唯一约束会失败，需要先手动处理重名数据
ALTER TABLE users ADD CONSTRAINT users_username_key UNIQUE (username);
