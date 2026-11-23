-- 邀请码（FR-23）与注册审核（FR-22）。

-- 邀请码。一个码只能提交一次注册申请，提交即失效（与审核结果无关）。记录永不删除，保证任何账号都能追溯到邀请它的码和生成者。
--   code：6 位数字 + 大写字母，存明文。穷举空间只有 36^6 ≈ 21.8 亿，哈希挡不住离线穷举，存了也没有保护作用；
--     有效期短、一码一用、注册后仍需审核，泄露的影响有限。存明文的好处是列表里能重新复制邀请链接。
--     唯一约束覆盖所有历史记录，同一个码不会被复用；生成时撞上已有的码由应用层重新生成。应用层负责把输入转成大写再查。
--   created_by：生成它的账号，溯源只看这一列。命令行生成的也记在管理员账号（ADMIN_USER_ID）名下。
--   expires_at：必填，所有邀请码都会过期（默认 3 天）。冻结、解冻都不改它。
--   used_at：提交注册申请的时间，非 NULL 即已使用（永久失效）。
--   reserved_*：冻结（验证通过、正在填写注册表单）。冻结是一份租约：页面开着时续期，
--     reserved_until 过了就自动恢复可用（浏览器崩溃、断网时服务端收不到"离开"的通知，只能靠到期）。
--     reservation_hash 是冻结令牌的 SHA-256，只有持有令牌的页面能续期和提交（令牌是 128 位随机数，SHA-256 足够）。
--     三个字段同时为 NULL（没冻结过 / 已解冻）或同时非 NULL。
CREATE TABLE invite_codes (
    id               BIGSERIAL   PRIMARY KEY,
    code             VARCHAR(6)  NOT NULL
                     CONSTRAINT invite_codes_code_format CHECK (code ~ '^[0-9A-Z]{6}$'),
    created_by       BIGINT      NOT NULL REFERENCES users (id),
    note             VARCHAR(50) NOT NULL DEFAULT '', -- 按字符计数，中文也算 1 个
    expires_at       TIMESTAMPTZ NOT NULL,
    used_at          TIMESTAMPTZ,
    revoked_at       TIMESTAMPTZ,
    reserved_at      TIMESTAMPTZ,
    reserved_until   TIMESTAMPTZ,
    reservation_hash VARCHAR(64),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_invite_codes_code UNIQUE (code),
    CONSTRAINT invite_codes_reservation_all_or_none CHECK (
        (reserved_at IS NULL) = (reserved_until IS NULL) AND (reserved_until IS NULL) = (reservation_hash IS NULL)
    )
);

-- 我生成的邀请码，按时间倒序（GET /invites）
CREATE INDEX idx_invite_codes_creator ON invite_codes (created_by, created_at DESC);

-- 邀请码的验证记录。既是限流的计数依据（存在库里，重启不清零），也是溯源记录（谁、什么时候、从哪个 IP 验证过）。
--   每个 IP 24 小时内最多验证 10 次（不论码是否有效，防止暴力猜码）；
--   每个码每分钟最多验证成功 1 次、24 小时内最多 10 次（只数 result = 'reserved'）。
--   invite_code_id：输入的码存在时记录是哪个码；码不存在时为 NULL（不记录对方猜的字符串）。
--   result：reserved = 验证成功并冻结；invalid = 码不存在、过期、已使用或已作废；
--     in_use = 码正被别人冻结；limited = 超出这个码的验证频率。超出 IP 限制的请求不记录。
CREATE TABLE invite_verifications (
    id             BIGSERIAL   PRIMARY KEY,
    ip             INET        NOT NULL,
    invite_code_id BIGINT      REFERENCES invite_codes (id),
    result         TEXT        NOT NULL
                   CONSTRAINT invite_verifications_result_check CHECK (result IN ('reserved', 'invalid', 'in_use', 'limited')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 按 IP 统计最近 24 小时的验证次数
CREATE INDEX idx_invite_verifications_ip ON invite_verifications (ip, created_at);
-- 按码查验证记录（溯源）、统计验证成功的次数
CREATE INDEX idx_invite_verifications_code ON invite_verifications (invite_code_id, created_at)
    WHERE invite_code_id IS NOT NULL;

-- 账号审核状态。
-- 先以 'active' 为默认值加列：已有账号（含测试账号）全部视为已通过；
-- 再把默认值改成 'pending'：之后注册的账号即使代码忘了写状态，也是待审核，不会被直接放行。
-- （PostgreSQL 11+ 加带常量默认值的列只改元数据，不重写整张表）
ALTER TABLE users ADD COLUMN status TEXT NOT NULL DEFAULT 'active'
    CONSTRAINT users_status_check CHECK (status IN ('pending', 'active', 'rejected'));
ALTER TABLE users ALTER COLUMN status SET DEFAULT 'pending';

-- 注册时使用的邀请码。引入邀请码之前的账号为 NULL。
ALTER TABLE users ADD COLUMN invite_code_id BIGINT REFERENCES invite_codes (id);

-- 一个邀请码最多对应一个账号。应用层在事务里用条件 UPDATE 消耗邀请码，已经保证了这一点；
-- 唯一索引是数据库层的兜底：代码写错了也不可能出现两个账号用同一个码
CREATE UNIQUE INDEX uq_users_invite_code ON users (invite_code_id) WHERE invite_code_id IS NOT NULL;
