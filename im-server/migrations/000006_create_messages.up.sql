-- 单聊会话（FR-10、FR-11）。一对用户只有一个会话：user_a 存较小的用户 ID，(user_a, user_b) 唯一。
-- last_seq 是会话内最后一条消息的序号，发消息时在事务里自增分配（见 message 模块 repository）。
CREATE TABLE conversations (
    id              BIGSERIAL PRIMARY KEY,
    user_a          BIGINT      NOT NULL REFERENCES users (id),
    user_b          BIGINT      NOT NULL REFERENCES users (id),
    last_seq        BIGINT      NOT NULL DEFAULT 0,
    last_message_at TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT conversations_user_order CHECK (user_a < user_b),
    CONSTRAINT uq_conversations_pair UNIQUE (user_a, user_b)
);

-- 会话成员。read_seq 是该成员已读到的序号，未读数 = last_seq - read_seq（FR-15，M5 使用）。
-- 单聊每个会话两条；以后群聊可以直接复用这张表。
CREATE TABLE conversation_members (
    conversation_id BIGINT NOT NULL REFERENCES conversations (id),
    user_id         BIGINT NOT NULL REFERENCES users (id),
    read_seq        BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (conversation_id, user_id)
);

-- 查"我参与的会话"（M5 会话列表）
CREATE INDEX idx_conversation_members_user ON conversation_members (user_id);

-- 消息。永不物理删除（FR-21）。
--   (conversation_id, seq) 唯一：会话内有序，用于增量同步和分页；
--   (sender_id, client_msg_id) 唯一：客户端超时重发时按它去重（FR-11）。
-- content 的长度上限 1500 字符（FR-13），char_length 按字符计数，和 Go 里的 utf8.RuneCountInString 一致。
CREATE TABLE messages (
    id              BIGSERIAL PRIMARY KEY,
    conversation_id BIGINT      NOT NULL REFERENCES conversations (id),
    seq             BIGINT      NOT NULL,
    sender_id       BIGINT      NOT NULL REFERENCES users (id),
    client_msg_id   VARCHAR(64) NOT NULL,
    type            TEXT        NOT NULL DEFAULT 'text'
                    CONSTRAINT messages_type_check CHECK (type IN ('text')),
    content         TEXT        NOT NULL
                    CONSTRAINT messages_content_len CHECK (char_length(content) BETWEEN 1 AND 1500),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_messages_conversation_seq UNIQUE (conversation_id, seq),
    CONSTRAINT uq_messages_sender_client_msg UNIQUE (sender_id, client_msg_id)
);
