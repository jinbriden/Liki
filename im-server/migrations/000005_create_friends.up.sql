-- 好友申请（FR-06、FR-07）。处理后的申请保留，供申请方查看结果。
CREATE TABLE friend_requests (
    id           BIGSERIAL PRIMARY KEY,
    from_user_id BIGINT       NOT NULL REFERENCES users (id),
    to_user_id   BIGINT       NOT NULL REFERENCES users (id),
    message      VARCHAR(100) NOT NULL DEFAULT '', -- PostgreSQL 的 VARCHAR(n) 按字符计数，中文也算 1 个
    status       TEXT         NOT NULL DEFAULT 'pending'
                 CONSTRAINT friend_requests_status_check CHECK (status IN ('pending', 'accepted', 'rejected')),
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    handled_at   TIMESTAMPTZ,
    CONSTRAINT friend_requests_not_self CHECK (from_user_id <> to_user_id)
);

-- 两人之间（不分方向）最多一条待处理申请：
--   A→B 待处理时，A 不能重复申请，B 也不能反向申请（只能处理 A 的申请）。
-- LEAST/GREATEST 把一对用户排成固定顺序，A→B 和 B→A 落在同一个索引键上；
-- 同时互相申请的并发请求只有一条能插入成功。
CREATE UNIQUE INDEX uq_friend_requests_pending_pair
    ON friend_requests (LEAST(from_user_id, to_user_id), GREATEST(from_user_id, to_user_id))
    WHERE status = 'pending';

-- 我收到的待处理申请
CREATE INDEX idx_friend_requests_incoming
    ON friend_requests (to_user_id, created_at DESC)
    WHERE status = 'pending';

-- 我发出的申请（含已处理，申请方离线时靠它看到结果）
CREATE INDEX idx_friend_requests_outgoing
    ON friend_requests (from_user_id, created_at DESC);

-- 好友关系（FR-07、FR-08）。双向各存一条，删除好友时两条一起删（物理删除，不涉及消息数据）。
-- 主键 (user_id, friend_id) 同时覆盖"查我的好友"查询。
CREATE TABLE friendships (
    user_id    BIGINT      NOT NULL REFERENCES users (id),
    friend_id  BIGINT      NOT NULL REFERENCES users (id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, friend_id),
    CONSTRAINT friendships_not_self CHECK (user_id <> friend_id)
);
