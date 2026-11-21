-- 用户 ID 改为应用层生成的随机 10 位数（1000000000 ~ 9999999999），不再使用自增序列。
-- 唯一性由主键约束保证，应用层遇到主键冲突会换一个 ID 重试。
ALTER TABLE users ALTER COLUMN id DROP DEFAULT;
DROP SEQUENCE IF EXISTS users_id_seq;

-- 已有数据（开发阶段的测试账号）重新分配随机 10 位 ID。
-- 新 ID 都 >= 1000000000，不会和旧的小 ID 冲突；新 ID 之间冲突的概率可以忽略。
UPDATE users SET id = 1000000000 + floor(random() * 9000000000)::bigint;

ALTER TABLE users ADD CONSTRAINT users_id_10_digits CHECK (id BETWEEN 1000000000 AND 9999999999);
