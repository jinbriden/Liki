# Liki 即时通讯项目

Web 端即时通讯软件，个人学习项目（Go 后端 + React 前端 + PostgreSQL）。
完整需求、架构、数据模型和里程碑见：@docs/requirements.md

## 目录结构

- `docker-compose.yml`：本地 PostgreSQL，监听 127.0.0.1:5432，用户 / 密码 / 库名 = im / im123 / im
- `im-server/`：Go 后端，模块名 `im-server`
  - `cmd/server/main.go`：程序入口，在这里手动组装依赖（repository → service → handler）
  - `internal/<模块>/`：user、friend、message 按 `model.go`、`repository.go`、`service.go`、`handler.go` 分层；需要实时推送的模块另有 `notify.go`（把 service 的 `Notifier` 接口实现为 WebSocket 推送）
  - `internal/gateway/`：WebSocket 网关，只做票据鉴权、连接管理、心跳、按 `type` 分发数据包和按用户推送（`Push`），不写业务逻辑；业务模块用 `Handle(type, fn)` 注册处理函数
  - `internal/pkg/response/`：统一响应格式 `{code, msg, data}` 和错误码常量（常量名以代码为准）
  - `internal/pkg/auth/`：JWT 签发/校验（`JWTManager`）和鉴权中间件；handler 用 `auth.UserID(c)` 取当前用户
  - `internal/pkg/logger/`：slog 初始化（控制台 + JSON 文件）、请求 ID / 访问日志 / panic 恢复中间件、`logger.WithAttrs(ctx, ...)` 把字段挂到 ctx 上
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
- GORM 已开启 `TranslateError: true`。
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
go run ./cmd/server
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
  - M4 阶段非好友一律不能发消息（40003），FR-12 的"最多一条"放到 M6。
  - 好友实时通知：`friend.request.received`（给被申请方）、`friend.request.handled`（给申请方）、`friend.sync`（给操作者自己的其他连接）。
  - 前端：`src/realtime/socket.ts`（连接、心跳、指数退避重连）、`src/stores/chat.ts`（发送中队列、5 秒未 ACK 重发、30 秒标记失败可重试、seq 不连续时补拉、超长消息只在本地显示为失败）、聊天页。消息暂存内存，IndexedDB 缓存在 M5。
- M5 会话与历史：已完成并验收（完成标准"清除缓存后重新登录，历史可以按需分页恢复"已用端到端测试验证）。需求见 requirements.md（FR-14～FR-18）：只列有消息的会话 + 临时项；未读 = last_seq − read_seq，read_seq 只增不减，由"实际看到（连续推进）"、"发送即已读"、"标记为已读"三种方式推进，参照 Discord 提供跳转到第一条未读；IndexedDB 按用户分库，主动退出时删除；用"已同步 seq 区间"处理缓存缺口；滚到顶部自动翻页。
  - 后端已完成（无新迁移）：`GET /api/v1/chats` 返回 `{items: [{peer, lastMessage, lastSeq, readSeq, unread, lastMessageAt}]}`，只含 last_seq > 0 的会话，按 last_message_at 倒序、不分页；`POST /api/v1/chats/:peerId/read {seq}` 返回 `{readSeq}`，用条件 UPDATE 保证只增不减、不超过 last_seq，没聊过返回 0；read_seq 真的变了才推 `chat.read {peerId, readSeq}` 给自己的所有连接（含发起请求的标签页，客户端取较大值），不推给对方。发消息的事务里同时推进发送方 read_seq，不额外推送。消息模块通过自己的 `UserDirectory` 接口用 `*user.Service` 取对方资料。service 单元测试 + `im_test` 库测试覆盖。
  - 前端已完成并用 Playwright 端到端验证：`src/db/chatCache.ts`（Dexie，库名 `liki-<用户 ID>`）、`src/utils/ranges.ts`（已同步区间）、`src/stores/chatList.ts`（会话列表、未读）、`src/stores/chat.ts`（消息窗口、已读推进、跳转）、`src/stores/session.ts`（主动退出删缓存）、聊天页改为会话列表。验证覆盖：离线未读 → 横条 → 跳转 → 逐步已读、停在底部自动已读、连发多条时被跳过的不算已读、标记为已读、发送即已读、多标签页同步、快照显示、落后 100 条以上的缺口补齐（翻到顶 seq 连续无遗漏、第二次全部读本地）、退出删库后重新登录历史完整恢复。
  - 顶部"N 条新消息"横条的条件是"未读 > 0 且看到过的最大 seq > readSeq"（有未读被跳过），"跳转"目标是当前 readSeq + 1；"以下为新消息"分隔线固定在打开时的位置，且只在窗口包含它时才画。
  - 消息区设了 `overflow-anchor: none`：向上翻页的滚动补偿由代码按第一条消息的 DOM 节点位移来做，不能再叠加 Chrome 的自动滚动锚定，否则会多滚一截、中间的消息被跳过而永远不算已读。
- 待办：登录、注册限流（NFR-05，M6）；连接建立后令牌过期不会主动断开 WebSocket（重连时换票据会失败，届时跳转登录）。

## 协作方式

- 这是我用来学习 Go 的项目：优先帮我解释代码、排查报错、审查我写的代码；核心逻辑由我自己写。修改代码前先说明方案，等我确认。
- 用中文交流。我熟悉 Java，讲解时可以类比 Java / Spring。
