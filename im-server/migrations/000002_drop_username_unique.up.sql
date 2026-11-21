-- 用户名不再全局唯一，账号由 id 唯一标识（登录使用 id）
ALTER TABLE users DROP CONSTRAINT users_username_key;

-- 普通索引（非唯一），供按用户名大小写不敏感搜索使用：WHERE lower(username) = lower(?)
CREATE INDEX idx_users_username_lower ON users (lower(username));
