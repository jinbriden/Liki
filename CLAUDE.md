# Liki 即时通讯项目

Web 端即时通讯软件，个人学习项目（Go 后端 + React 前端 + PostgreSQL）。
完整需求、架构、数据模型和里程碑见：@docs/requirements.md

## 目录结构

- `docker-compose.yml`：本地 PostgreSQL，监听 127.0.0.1:5432，用户 / 密码 / 库名 = im / im123 / im
- `im-server/`：Go 后端，模块名 `im-server`
  - `cmd/server/main.go`：程序入口，在这里手动组装依赖（repository → service → handler）
  - `cmd/admin/main.go`：服务器上的管理命令行 `im-admin`（注册审核、邀请码），直接用 user 模块的 `ReviewService` / `InviteService`，不需要 JWT_SECRET
  - `internal/<模块>/`：user、friend、message 按 `model.go`、`repository.go`、`service.go`、`handler.go` 分层；需要实时推送的模块另有 `notify.go`（把 service 的 `Notifier` 接口实现为 WebSocket 推送）
  - `internal/gateway/`：WebSocket 网关，只做票据鉴权、连接管理、心跳、按 `type` 分发数据包和按用户推送（`Push`），不写业务逻辑；业务模块用 `Handle(type, fn)` 注册处理函数
  - `internal/pkg/response/`：统一响应格式 `{code, msg, data}` 和错误码常量（常量名以代码为准）
  - `internal/pkg/auth/`：JWT 签发/校验（`JWTManager`）和鉴权中间件；handler 用 `auth.UserID(c)` 取当前用户
  - `internal/pkg/logger/`：slog 初始化（控制台 + JSON 文件）、请求 ID / 访问日志 / panic 恢复中间件、`logger.WithAttrs(ctx, ...)` 把字段挂到 ctx 上
  - `internal/pkg/reqctx/`：写请求（非 GET）和 WebSocket 数据包处理改用 `context.WithoutCancel` 派生的 ctx（保留日志字段、30 秒超时兜底），客户端中途断开时已开始的写操作照样完成并推送
  - `internal/pkg/ratelimit/`：限流（NFR-05）。`Limiter` 接口 + 内存令牌桶（`golang.org/x/time/rate`）；规则集中在 `main.go`，按"方法 路由模板"挂在路由组上，启动时核对规则里的路由都存在
  - `migrations/`：golang-migrate 的 SQL 迁移文件
- `web/`：React + TS + Vite 前端（antd 6、React Router 8、Zustand 5），结构见 `web/README.md`
  - Node 用 WSL 内 nvm 安装的版本（不要用 PATH 里 Windows 的 npm）
  - 开发时 Vite 把 `/api` 代理到 8080，后端不配 CORS
  - 令牌存 localStorage（键 `liki.token`），只通过 `src/stores/auth.ts` 读写；api 层遇到 10003 只清登录状态，由路由守卫负责跳转
  - UI 风格：克制、现代的深色极简风；不要赛博朋克元素（霓虹色、发光阴影、渐变、扫描线等），也不要 QQ / 微信式的厚重布局。配色改 `src/theme.ts` 的 palette，并同步 `src/index.css` 的 `--liki-*` 变量
    - 三层背景靠明度差区分：导航栏 `#0E1015` < 会话列表 `#14171F` < 聊天画布 `#181B23` < 卡片 / 对方气泡 / 浮层 `#222632`（输入框 `#20242F`）；分割线统一 `rgba(255,255,255,0.06)`
    - 文字：主 `#ECEFF4`、次级 `#8A93A6`、弱提示 `#5C6475`
    - 主强调色淡黄 `#F3E38A`（悬停 `#F7EB9F`）配深色字 `#14171F`，只用于核心操作（主按钮、发送按钮、链接）和自己的消息气泡；点缀色只用于极小元素：青色 `#5EEAD4`（在线状态点、导航激活指示条）、粉色 `#F472B6`（未读角标、"以下为新消息"）
    - 消息流和输入框限宽 920px 居中；同一发送者 3 分钟内的连续消息分组；对方的消息只在组的第一条显示头像，自己的消息不显示头像

## 代码约定

- handler 只负责 JSON 绑定和写响应；service 写业务逻辑；repository 只访问数据库。
- 参数校验：handler 不写 `binding` 校验规则，业务规则（长度、字符集等）在 service 校验并返回哨兵错误（如 `ErrInvalidPassword`）。请求体无法解析 → `CodeBadRequest`(10002)；业务规则不满足 → `CodeInvalidParam`(10001)。
- 数据库错误（如唯一约束冲突）在 repository 层翻译成业务哨兵错误，service 和 handler 不 import gorm。
- GORM 已开启 `TranslateError: true`，但它只对 Create / Update 等方法生效：手写 SQL（`Raw(...).Scan`）拿到的是原始的 `*pgconn.PgError`，需要判断唯一约束 / 外键冲突时先手动翻译（见 `user` 模块 repository 的 `translate`）。
- 接口返回专门定义的 resp 结构体，不直接返回 model，避免泄露 `password_hash` 等字段。
- 表结构变更只通过 `migrations/` 下的迁移文件，不使用 GORM AutoMigrate，不手动改表。
- 服务开发阶段只监听 `127.0.0.1:8080`。
- 需要登录的接口注册到 `RegisterRoutes(public, protected)` 的 protected 分组；当前用户 ID 一律取 `auth.UserID(c)`，不信任请求里的用户 ID。
- 401 分两种错误码：`CodeUnauthorized`(10003) 表示令牌缺失/无效/过期，前端应跳转登录；`CodeInvalidCredentials`(20001) 表示登录失败，前端不跳转。
- 日志：
  - 一律用 `slog.XxxContext(ctx, ...)`，ctx 上已挂好 `request_id`、`user_id`（HTTP）或 `conn_id`、`user_id`（WebSocket），不要手动重复记录这几个字段。
  - 业务事件（注册、登录、好友申请、消息写库等）在 service 层记 Info；正常但频繁的情况（如消息重发去重）记 Debug；handler 只在返回 50000 时记 Error。
  - 日志里不能出现密码、令牌、WebSocket 票据和消息正文（消息只记长度）；访问日志只记路径，不记查询参数。
- 时间字段（如 `created_at`）由数据库 `now()` 生成：model 上用 `gorm:"autoCreateTime:false;default:now()"`，GORM 插入时通过 `RETURNING` 取回，保证返回值和库里一致（微秒精度）。不要用 Go 的 `time.Now()` 写入。
- 接口输出的时间一律转成 UTC（`.UTC()`，JSON 形如 `...Z`）。
- handler 一律从 `c.Request.Context()` 取 ctx（`reqctx` 中间件替换的就是它），不要直接把 `*gin.Context` 当 ctx 传。
- 限流：按 IP / 用户的规则加在 `main.go` 的 `ipRules` / `userRules`；需要业务信息才能判断的（按登录的目标账号、发消息要排除去重命中）放在 service，用函数式选项注入（`user.WithLoginLimiter`、`message.WithSendLimiter`）。
- `gin` 设置了 `SetTrustedProxies(nil)`：`c.ClientIP()` 取 TCP 对端地址，开发时经过 Vite 代理都是 127.0.0.1；部署到反向代理后面时要改成信任代理地址。

## 常用命令

```bash
# 在仓库根目录
docker compose up -d

# 在 im-server 目录
migrate -path migrations -database "postgres://im:im123@localhost:5432/im?sslmode=disable" up
# JWT_SECRET 必填（至少 32 字节），没有默认值，缺失时服务拒绝启动。
# 生成一次后固定使用（例如写进 ~/.bashrc），每次换密钥会让已签发的令牌全部失效：
#   openssl rand -base64 48
export JWT_SECRET='<你的密钥>'
# DATABASE_DSN 也必填，没有默认值（默认值里只能写开发库密码，线上漏配会悄悄用上）：
export DATABASE_DSN="host=localhost user=im password=im123 dbname=im port=5432 sslmode=disable"
# ADMIN_USER_ID 可选：管理员账号的用户 ID（必须已存在且已通过审核，否则拒绝启动）。不设置时没有人能生成邀请码，也就没人能注册
# 可选：LISTEN_ADDR（默认 127.0.0.1:8080）、TRUSTED_PROXIES（逗号分隔，线上在 Nginx 后面设为 127.0.0.1）
go run ./cmd/server
# 管理命令行（读同样的 DATABASE_DSN、ADMIN_USER_ID；PUBLIC_BASE_URL 可选，用于打印邀请链接）
go run ./cmd/admin help
go run ./cmd/admin list                  # 待审核的账号（list rejected 看已拒绝的）
go run ./cmd/admin approve <用户ID>       # reject <用户ID> 拒绝
go run ./cmd/admin invite create -note 备注  # 还有 invite list | show <ID>（含验证记录）| revoke <ID>
# 日志：控制台为 text，同时以 JSON 追加写入 logs/im-server.log（已被 .gitignore 忽略），按 request_id / conn_id 检索：
#   jq 'select(.request_id=="<X-Request-ID 响应头>")' logs/im-server.log
# 可选环境变量：LOG_LEVEL=debug|info|warn|error（默认 debug）、LOG_FORMAT=text|json、
#   LOG_FILE=<路径>（设为空字符串则不写文件）、LOG_SQL=true（记录所有 SQL，不含参数值）
# WSL 里有 http_proxy 时，curl 测本地接口要加 --noproxy '*'
go build ./...
go test ./...
# 消息模块的数据库测试需要独立的测试库 im_test（不要用开发库），没设置 TEST_DATABASE_DSN 时自动跳过：
#   docker exec im-postgres psql -U im -d im -c "CREATE DATABASE im_test"
#   migrate -path migrations -database "postgres://im:im123@localhost:5432/im_test?sslmode=disable" up
TEST_DATABASE_DSN="host=localhost user=im password=im123 dbname=im_test port=5432 sslmode=disable" go test ./...

# 在 web 目录（需先启动后端）
npm run dev     # http://127.0.0.1:5173
npm run build   # 类型检查 + 打包
npm run lint
```

## 当前进度

- M1 项目骨架：已完成（健康检查 `GET /health`）。
- M2 账号：后端已完成。
  - `POST /api/v1/auth/register`：用户名不唯一、兼作昵称（迁移 000004 删除了 nickname 列）；用户 ID 是 `crypto/rand` 生成的随机 10 位数，主键冲突时重试（迁移 000003）。
  - `POST /api/v1/auth/login`：用户 ID + 密码，签发 HS256 JWT，有效期 3 天。用户不存在时也做一次假 bcrypt 比较，使响应时间与密码错误一致。
  - `GET /api/v1/me`：受保护接口，未登录返回 401（M2 完成标准）。
  - 退出登录（FR-03）由前端删除令牌并断开长连接，服务端不提供 logout 接口。
  - 前端已完成：登录页、注册页（注册成功后自动登录，主框架弹窗提示记住用户 ID）；退出登录在左侧导航底部的账号菜单里。
- M3 好友：后端已完成（迁移 000005、`internal/friend/`、错误码 3xxxx），service 单元测试覆盖申请/处理/删除/搜索；已用真实库走通"互加好友再删除"。
  - 接口：`GET /friends/search?q=`、`GET /friends`、`DELETE /friends/:friendId`、`POST /friend-requests`、`GET /friend-requests/incoming|outgoing`、`POST /friend-requests/:id/accept|reject`（都在 `/api/v1` 下，需登录）；列表统一返回 `{items: [...]}`。
  - 两人之间最多一条待处理申请，由唯一索引 `uq_friend_requests_pending_pair` 兜底；repository 命中时返回 `ErrPendingExists`，service 判断方向（同方向幂等返回，反方向 30003）。
  - 好友模块通过自己定义的 `UserDirectory` 接口使用 `*user.Service`（`GetProfiles` / `SearchProfiles`），不直接查 users 表。
  - 前端已完成：好友页（列表、详情、删除、新的朋友、搜索并申请），黑色系主题；聊天页暂为占位。M4 之前没有实时推送，进入好友页或点刷新时拉取。
  - 实时通知（新申请、申请被处理）在 M4 接入。
- 日志（NFR-13）：已补齐。`gin.New()` + `logger.Middleware()`（请求 ID 写入响应头 `X-Request-ID`、访问日志）+ `logger.Recovery()`；GORM 日志也走 slog（默认只记慢查询和出错的 SQL，唯一约束/外键冲突不算错误）。
- M4 实时单聊：已完成并验收（迁移 000006、`internal/gateway/`、`internal/message/`、错误码 4xxxx）。端到端脚本验证了票据鉴权、重发去重、多标签页同步、1500 字边界、断线期间的消息重连后补齐（seq 连续）；浏览器里验证了断网发送后自动补发、超长消息本地标记失败。
  - 心跳：客户端每 25 秒发应用层 ping；服务端每 30 秒发协议层 ping（浏览器网络栈自动回 pong，不受后台标签页定时器节流影响），60 秒收不到任何数据就断开。
  - 建连：`POST /api/v1/ws/ticket`（需登录）换一次性票据（30 秒、用一次即作废），再连 `GET /api/v1/ws?ticket=`；票据无效时握手阶段返回 401。票据存储、在线状态都是接口 + 内存实现（NFR-10）。
  - 协议：`{type, version: 1, data}`。
  - 发消息：`message.send {clientMsgId, toUserId, content}` → 服务端回 `message.ack {clientMsgId, code, msg, message?}`，并向双方所有连接（含发送方自己的其他标签页）推 `message.new {message}`。
  - 可靠性：按 `(sender_id, client_msg_id)` 去重，重复提交返回已有消息；seq 在事务里 `UPDATE conversations ... RETURNING last_seq` 分配，冲突回滚不留空洞。单元测试 + `im_test` 库上的并发测试覆盖。
  - 拉取：`GET /api/v1/chats/:peerId/messages?afterSeq=`（增量同步）或 `?beforeSeq=`（向前翻页，都不带为最新一页），返回 `{items, hasMore}`，按 seq 升序。
  - 非好友发消息的规则见 M6（FR-12，40004）；M4、M5 阶段的 40003 已废弃。
  - 好友实时通知：`friend.request.received`（给被申请方）、`friend.request.handled`（给申请方）、`friend.sync`（给操作者自己的其他连接）。
  - 前端：`src/realtime/socket.ts`（连接、心跳、指数退避重连）、`src/stores/chat.ts`（发送中队列、5 秒未 ACK 重发、30 秒标记失败可重试、seq 不连续时补拉、超长消息只在本地显示为失败）、聊天页。消息暂存内存，IndexedDB 缓存在 M5。
- M5 会话与历史：已完成并验收（完成标准"清除缓存后重新登录，历史可以按需分页恢复"已用端到端测试验证）。需求见 requirements.md（FR-14～FR-18）：只列有消息的会话 + 临时项；未读 = last_seq − read_seq，read_seq 只增不减，由"实际看到（连续推进）"、"发送即已读"、"标记为已读"三种方式推进，参照 Discord 提供跳转到第一条未读；IndexedDB 按用户分库，主动退出时删除；用"已同步 seq 区间"处理缓存缺口；滚到顶部自动翻页。
  - 后端已完成（无新迁移）：`GET /api/v1/chats` 返回 `{items: [{peer, lastMessage, lastSeq, readSeq, unread, lastMessageAt}]}`，只含 last_seq > 0 的会话，按 last_message_at 倒序、不分页；`POST /api/v1/chats/:peerId/read {seq}` 返回 `{readSeq}`，用条件 UPDATE 保证只增不减、不超过 last_seq，没聊过返回 0；read_seq 真的变了才推 `chat.read {peerId, readSeq}` 给自己的所有连接（含发起请求的标签页，客户端取较大值），不推给对方。发消息的事务里同时推进发送方 read_seq，不额外推送。消息模块通过自己的 `UserDirectory` 接口用 `*user.Service` 取对方资料。service 单元测试 + `im_test` 库测试覆盖。
  - 前端已完成并用 Playwright 端到端验证：`src/db/chatCache.ts`（Dexie，库名 `liki-<用户 ID>`）、`src/utils/ranges.ts`（已同步区间）、`src/stores/chatList.ts`（会话列表、未读）、`src/stores/chat.ts`（消息窗口、已读推进、跳转）、`src/stores/session.ts`（主动退出删缓存）、聊天页改为会话列表。验证覆盖：离线未读 → 横条 → 跳转 → 逐步已读、停在底部自动已读、连发多条时被跳过的不算已读、标记为已读、发送即已读、多标签页同步、快照显示、落后 100 条以上的缺口补齐（翻到顶 seq 连续无遗漏、第二次全部读本地）、退出删库后重新登录历史完整恢复。
  - 顶部"N 条新消息"横条的条件是"未读 > 0 且看到过的最大 seq > readSeq"（有未读被跳过），"跳转"目标是当前 readSeq + 1；"以下为新消息"分隔线固定在打开时的位置，且只在窗口包含它时才画。
  - 消息区设了 `overflow-anchor: none`：向上翻页的滚动补偿由代码按第一条消息的 DOM 节点位移来做，不能再叠加 Chrome 的自动滚动锚定，否则会多滚一截、中间的消息被跳过而永远不算已读。
- M6 删除与限制：已完成并验收（迁移 000007；完成标准"删除的消息换一个浏览器登录后仍不出现"已用端到端测试验证）。需求按讨论结果更新了 requirements.md（FR-12、FR-14、FR-19、FR-20、NFR-05，第 8 节两条已勾选）。
  - 非好友限制（FR-12）：会话里上一条消息也是自己发的就拒绝（40004），对方回复一条可再发一条；好友不受限。"上一条是谁发的"在 `Create` 事务里分配 seq（持有会话行锁）**并插入之后**检查——放在插入之后，非好友重发已写入的消息会先撞上去重约束，不会被误判成第二条。前端：非好友时输入框上方提示、最后一条是自己的就锁输入框、头部"非好友"标记和"添加好友"、搜索结果可直接"发消息"（`chatList.peers` 暂存陌生人资料）；收到 40004 时刷新好友列表（对方可能删了我）。
  - 删除（FR-19～21）：逐条 / 多选批量（一次 1～30 条）/ 清空聊天记录，只对自己生效，服务端不物理删除。
    - 表：`message_deletions(id, user_id, message_id, deleted_at)`，主键 `(user_id, message_id)` 保证幂等，自增 `id` 作为多设备同步游标；`conversation_members.cleared_seq` 记清空位置（清空不逐条写记录），清空同时把 read_seq 推进到同一位置。
    - 接口：`POST /chats/:peerId/messages/delete {seqs}`；`POST /chats/:peerId/clear {upToSeq}`（客户端点击时已知的最新 seq，之后到的消息不受影响）返回 `{clearedSeq, readSeq}`；`GET /message-deletions?after=` 按游标分页。拉消息的接口过滤自己删除 / 清空的，并多返回 `lastSeq`（最后几条被删时客户端据此把已同步区间补到最新，否则会反复加载）；`GET /chats` 多返回 `clearedSeq`，`lastMessage` 是自己能看到的最后一条（LATERAL 子查询），全部删光时为 null、会话仍在列表中。
    - 删除事务里先 `pg_advisory_xact_lock(user_id)`：同一用户的删除串行，删除记录的 id 顺序 = 提交顺序，按游标同步不会漏。
    - 多设备同步：在线的靠推送 `message.deleted {peerId, seqs}`、`chat.cleared {peerId, clearedSeq, readSeq}`（只推给自己）；离线的在登录、重连时按游标拉删除记录（游标存 IndexedDB 的 `meta` 表，Dexie version 2），会话列表的 clearedSeq 变大时删除本地缓存。被删的 seq 留在已同步区间里，不当成缺口。
    - 前端：消息悬停出现"⋯"菜单（多选 / 删除），多选模式下输入框换成"已选 N / 30"操作条，头部"⋯"菜单里有"清空聊天记录"，都要二次确认；发送中的不能删，发送失败 / 超长的只删本地。
  - 限流（NFR-05）：注册每 IP 每小时 10 次、登录每 IP 每分钟 20 次 + 每个目标账号每分钟 5 次、好友申请每用户每小时 30 次、换票据每用户每分钟 30 次、发消息每用户每秒 5 条（突发 10 条，去重命中的重发不计数）；超出返回 10004 / HTTP 429。前端被限流的消息标记为"发送太频繁，点击重试"，不自动重发。
  - 顺带修复：客户端在写请求途中断开时，数据库已提交但驱动报 context canceled，接口返回 500、提交后的推送被跳过（例如同意好友申请后申请方收不到通知）。用 `reqctx` 让写操作不随客户端断开而取消。
- M7 审核与上线：进行中（迁移 000008；需求 FR-22、FR-23；错误码 10005、20002～20007）。
  - 邀请码（FR-23）：6 位 0-9A-Z，明文存储（穷举空间小，哈希没有保护作用），一码一用（提交注册申请即失效，与审核结果无关；`users.invite_code_id` 部分唯一索引兜底），默认 3 天过期，`invite_codes.created_by` 溯源（命令行生成的也记在管理员名下）。
  - 注册分两步：`POST /auth/invite/verify` 验证通过即**冻结**（租约 3 分钟，返回 128 位冻结令牌，库里存 SHA-256），注册页每 30 秒 `renew`，站内离开 `release` 立即解冻，刷新 / 关闭标签页（`pagehide`）用 sendBeacon 软释放（只保留 15 秒，刷新后续期即恢复），最长冻结 30 分钟；`POST /auth/register` 必须带令牌，事务里条件 UPDATE 写 `used_at` + 插入用户，ID 冲突重试时整体回滚。冻结不刷新有效期。
  - 验证频率存在 `invite_verifications`（也是溯源记录：时间、IP、结果）：每 IP 24 小时 10 次（不论码是否有效，`pg_advisory_xact_lock` 让同一 IP 的验证串行）、每码 1 分钟 1 次 / 24 小时 10 次（只数成功的）。`ReserveInvite` 对业务失败也提交事务（失败的尝试要计数），结果通过外层变量带出。
  - 审核（FR-22）：`users.status` pending / active / rejected，迁移前的账号为 active、之后默认 pending。登录在密码校验通过后才判断状态（20002 / 20003，HTTP 403）；搜索只返回 active。审核只在命令行（`ReviewService`），已通过的不能改成拒绝。
  - 权限：`InviteService.CanInvite` 判断谁能生成邀请码（现在 = `ADMIN_USER_ID`），`/me` 和登录返回 `canInvite`，前端据此在账号菜单显示"邀请注册"；生成邀请码每用户每小时 20 次。
  - 前端：`src/stores/inviteReservation.ts` 管冻结的生命周期（续期定时器、离开时解冻、sessionStorage 恢复、StrictMode 下"卸载再挂载"不误解冻）；注册页先显示邀请码输入（`/register#invite=XXXXXX` 自动验证后从地址栏去掉），冻结后才显示注册表单，提交成功显示用户 ID 和"等待审核"、不自动登录；`src/pages/InviteModal.tsx` 生成、复制链接、作废，查看使用者和验证记录。
  - 生产配置：`DATABASE_DSN` 必填、`LISTEN_ADDR`、`TRUSTED_PROXIES`（线上必须设，否则所有人共用 127.0.0.1 的验证次数）、`ADMIN_USER_ID`。
  - 已用 `im_test` 起真实服务验证了验证 / 续期 / text/plain 软释放 / 每码频率；浏览器里的注册流程还没测（要调注册接口，先问用户）。
  - 待做：Nginx / systemd / 备份 / logrotate 配置和 `docs/deploy.md`。
- 待办：连接建立后令牌过期不会主动断开 WebSocket（重连时换票据会失败，届时跳转登录）；个人资料页（requirements.md 第 8 节未定）。

## 协作方式

- 这是我用来学习 Go 的项目：优先帮我解释代码、排查报错、审查我写的代码；核心逻辑由我自己写。修改代码前先说明方案，等我确认。
- 用中文交流。我熟悉 Java，讲解时可以类比 Java / Spring。
