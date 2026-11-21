-- 恢复自增序列。注意：已分配的随机 ID 不会还原成原来的值，新用户从当前最大 ID + 1 开始自增。
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_id_10_digits;

CREATE SEQUENCE users_id_seq OWNED BY users.id;
SELECT setval('users_id_seq', COALESCE((SELECT max(id) FROM users), 0) + 1, false);
ALTER TABLE users ALTER COLUMN id SET DEFAULT nextval('users_id_seq');
