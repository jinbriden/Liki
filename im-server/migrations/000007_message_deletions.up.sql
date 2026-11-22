-- 消息删除（FR-19、FR-20）：只对自己生效，服务端的消息本身永不物理删除（FR-21）。

-- 清空聊天记录的位置：seq <= cleared_seq 的消息对该成员不可见。只增不减。
-- 清空不逐条写删除记录（聊过几千条也只改这一个数），多设备通过会话列表里的 clearedSeq 同步。
ALTER TABLE conversation_members ADD COLUMN cleared_seq BIGINT NOT NULL DEFAULT 0;

-- 逐条 / 批量删除的记录。
--   主键 (user_id, message_id)：同一条消息重复删除不产生新记录（幂等），拉取历史时按它过滤；
--   id 自增：作为多设备增量同步删除记录的游标（GET /message-deletions?after=<id>），
--     离线期间缓存着旧消息的浏览器，下次上线按游标补齐这期间的删除。
CREATE TABLE message_deletions (
    id         BIGSERIAL   NOT NULL,
    user_id    BIGINT      NOT NULL REFERENCES users (id),
    message_id BIGINT      NOT NULL REFERENCES messages (id),
    deleted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, message_id),
    CONSTRAINT uq_message_deletions_id UNIQUE (id)
);

-- 按游标同步：WHERE user_id = ? AND id > ? ORDER BY id
CREATE INDEX idx_message_deletions_user_cursor ON message_deletions (user_id, id);
