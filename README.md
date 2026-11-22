# Liki

Web 端即时通讯软件。后端 Go，前端 React + TypeScript，数据库 PostgreSQL。

第一阶段（M1～M6）已全部完成，打通了"账号 → 好友 → 一对一文字聊天"的完整流程：消息不丢、不重、有序，支持离线消息、未读数、本地缓存、多端同步和仅对自己生效的消息删除。

- 完整需求、架构、数据模型和里程碑见 [docs/requirements.md](docs/requirements.md)
- 前端目录结构见 [web/README.md](web/README.md)

## 功能概览

| 模块 | 功能 |
| --- | --- |
| 账号 | 用户名 + 密码注册，系统分配随机 10 位用户 ID；用户 ID + 密码登录，JWT 有效期 3 天；退出登录时删除本地缓存；同一账号可多处同时在线 |
| 好友 | 按用户名（完全匹配、不区分大小写）或用户 ID 搜索；发送申请（附验证信息）、同意、拒绝；删除好友（双向解除，聊天记录保留）；申请和处理结果实时通知 |
| 单聊 | 文字消息实时收发、离线消息；客户端消息 ID + ACK + 超时重发 + 服务端去重；会话内递增 seq 保证有序；单条最多 1500 字 |
| 非好友限制 | 非好友之间，对方回复前只能发一条消息，对方回复一条后可以再发一条；可以从搜索结果直接给陌生人发消息 |
| 会话列表 | 只列出有消息的会话，按最后一条消息时间倒序；未读数（实际看到、发送消息、标记已读三种方式推进）；"以下为新消息"分隔线、跳转到第一条未读 |
| 历史与缓存 | IndexedDB 按用户分库缓存；断线重连增量同步；滚动到顶部按需分页加载；用"已同步 seq 区间"处理缓存缺口 |
| 删除 | 逐条删除、多选批量删除（一次最多 30 条）、清空聊天记录；只对自己生效，对方仍能看到；同步到自己登录的所有设备；服务端不物理删除 |
| 安全 | bcrypt 存密码；JWT 鉴权；WebSocket 一次性票据建连；只能读取自己参与的会话；参数化查询；前端不使用 `dangerouslySetInnerHTML`；接口限流 |

## 技术栈

| 层次 | 选型 |
| --- | --- |
| 后端 | Go 1.26、Gin、gorilla/websocket、GORM、golang-migrate、golang-jwt、bcrypt、标准库 slog、golang.org/x/time/rate |
| 数据库 | PostgreSQL（Docker Compose 启动） |
| 前端 | React 19、TypeScript 7、Vite 8、Ant Design 6、React Router 8、Zustand 5、Dexie 4（IndexedDB） |

## 快速开始

```bash
# 1. 数据库（仓库根目录）
docker compose up -d

# 2. 后端（im-server 目录）
migrate -path migrations -database "postgres://im:im123@localhost:5432/im?sslmode=disable" up
export JWT_SECRET="$(openssl rand -base64 48)"   # 必填，至少 32 字节；固定使用，换密钥会让已签发的令牌失效
go run ./cmd/server                              # 监听 127.0.0.1:8080

# 3. 前端（web 目录）
npm install
npm run dev                                      # http://127.0.0.1:5173，/api 代理到 8080
```

测试：

```bash
# 在 im-server 目录。数据库相关测试需要独立的测试库 im_test，没设置 TEST_DATABASE_DSN 时自动跳过
go test ./...
TEST_DATABASE_DSN="host=localhost user=im password=im123 dbname=im_test port=5432 sslmode=disable" go test -race ./...

# 在 web 目录
npm run build   # 类型检查 + 打包
npm run lint
```

可选的环境变量：`DATABASE_DSN`、`LOG_LEVEL`（debug / info / warn / error）、`LOG_FORMAT`（text / json）、`LOG_FILE`、`LOG_SQL`。

## 架构

```
浏览器（React）
  ├─ HTTP  /api/v1/*      注册登录、好友操作、历史拉取、删除等"请求-响应"类操作
  ├─ WS    /api/v1/ws     发消息、接收推送
  └─ IndexedDB            每个用户一个库：消息、已同步区间、待发送消息、会话列表快照、删除同步游标
        │
Go 服务（模块化单体）
  ├─ gateway   WebSocket 网关：票据鉴权、连接管理、心跳、按 type 分发数据包、按用户推送；不写业务逻辑
  ├─ user      账号
  ├─ friend    好友
  ├─ message   会话、消息、已读、删除
  └─ pkg       统一响应与错误码、JWT、日志、限流、reqctx
        │
PostgreSQL   users、friend_requests、friendships、conversations、conversation_members、messages、message_deletions
```

- **模块化单体**：每个业务模块按 `model → repository → service → handler` 分层。模块之间只通过使用方定义的接口调用，例如消息模块通过自己的 `FriendChecker`、`UserDirectory` 接口使用好友和用户模块，不跨模块查表；依赖在 `cmd/server/main.go` 里手动组装。以后需要拆分服务时，按模块边界拆。
- **网关与业务分离**：业务模块用 `Handle(type, fn)` 注册数据包处理函数，通过 `Push(userID, type, data)` 推送。在线状态、票据存储都是接口 + 内存实现，以后可以换成 Redis。

## 后端要点

### 消息可靠性

1. 客户端为每条消息生成 `clientMsgId`，通过 WebSocket 发送 `message.send`。
2. 服务端按 `(sender_id, client_msg_id)` 去重：重复提交直接返回已有的消息，不重复写库。
3. 新消息在一个事务里完成三件事：
   - `UPDATE conversations SET last_seq = last_seq + 1 ... RETURNING last_seq` 分配 seq。这条语句会给会话行加锁，同一会话的并发发送在这里排队，所以 seq 不会重复；冲突回滚时 seq 也一起回滚，不留空洞。
   - 插入消息。
   - 把发送方的已读位置推进到这条消息（发送即已读）。
4. 写库后回复 `message.ack`（带 seq），并向会话双方的所有在线连接推送 `message.new`（包括发送方自己的其他标签页）。

### 非好友限制

- 判断"会话里上一条消息是不是自己发的"，放在分配 seq 的事务里、插入消息之后：
  - 持有会话行锁，同一个人在两个标签页同时发，只有一条能通过。
  - 插入在前，非好友重发一条已经写入的消息时，会先命中去重约束按重复处理，不会被误判成"第二条"。

### 已读与未读

- 未读数 = `last_seq − read_seq`，`read_seq` 只增不减、不超过 `last_seq`，用条件 UPDATE 保证。
- 已读位置真的变了才推送 `chat.read` 给自己的所有连接；不推给对方（本阶段不做已读回执）。

### 删除

- **逐条和批量删除**：记录在 `message_deletions` 表里。
  - 主键 `(user_id, message_id)` 保证重复删除是幂等的。
  - 自增 `id` 作为多设备同步的游标。
  - 删除事务先执行 `pg_advisory_xact_lock(user_id)`，让同一用户的删除串行执行。这样删除记录的 id 顺序就等于提交顺序，按游标同步不会漏掉记录。
- **清空聊天记录**：不逐条写记录，只推进 `conversation_members.cleared_seq`，同时把已读位置推进到同一位置。
- **拉取过滤**：拉历史和会话列表时，过滤掉自己删除和清空的消息。会话列表用 LATERAL 子查询取"自己能看到的最后一条"作为摘要。
- 服务端的消息数据永不物理删除。

### 稳定性与安全

- **WebSocket 鉴权**：先用 JWT 调 `POST /ws/ticket` 换一次性票据（30 秒有效、用一次即作废），再用票据建连，令牌本身不出现在 URL 里。
- **心跳**：
  - 客户端每 25 秒发应用层 ping。
  - 服务端每 30 秒发协议层 ping。这种 ping 由浏览器网络栈自动回复，不受后台标签页定时器节流的影响。
  - 服务端 60 秒收不到任何数据就断开连接。
- **reqctx**：写请求和 WebSocket 数据包处理使用 `context.WithoutCancel` 派生的 ctx。客户端中途断开时，已经开始的写操作照样完成并推送，不会出现"数据库已提交、接口却返回 500、推送被跳过"。
- **限流**：令牌桶算法，`Limiter` 是接口，第一阶段用内存实现。
  - 规则集中在 `main.go`，启动时核对规则对应的路由都存在。
  - 设置了 `SetTrustedProxies(nil)`，防止客户端伪造 `X-Forwarded-For` 绕过按 IP 的限流。
- **登录**：用户不存在时也做一次假的 bcrypt 比较，让响应时间和"密码错误"一致，不暴露账号是否存在。
- **日志**：结构化日志（slog），同时输出到控制台和 JSON 文件。每条日志自动带上 `request_id`、`user_id`（WebSocket 是 `conn_id`）。日志里不记录密码、令牌、票据和消息正文。
- **数据库**：
  - 表结构只通过迁移文件变更。
  - 数据库错误在 repository 层翻译成业务哨兵错误，service 不依赖 GORM。
  - 时间由数据库 `now()` 生成，接口一律输出 UTC。

## 前端要点

- **连接**：`realtime/socket.ts` 管理全局唯一的 WebSocket 连接，负责换票据、心跳、断线后指数退避重连（1s、2s、4s…，最多 30s），网络恢复时立即重连。
- **发送可靠性**：
  - 消息先显示为"发送中"，5 秒没收到 ACK 就用同一个 `clientMsgId` 重发，30 秒仍未确认则标记为失败，可以点击重试。
  - 收到推送时发现 seq 跳号，就补拉中间缺的消息。
  - 刷新页面时还没确认的消息，显示为"发送失败，点击重试"。
- **消息窗口与缓存**：
  - 对话框显示的是一段 seq 区间 `[winLo, winHi]`。
  - 本地缓存记录"已同步的 seq 区间"：区间内的消息读本地；遇到缺口或超出本地范围时，才向服务端分页拉取。
  - 被删除的 seq 留在已同步区间里，不会被当成缺口反复拉取。
- **已读**：
  - 用 IntersectionObserver 检测消息的底边是否真正出现在可视范围内，从当前已读位置往后连续推进，300 毫秒内的多次推进合并后再上报。
  - 打开会话时插入"以下为新消息"分隔线；有未读被跳过时，顶部显示"N 条新消息"，提供"跳转"和"标记为已读"。
- **多端同步**：
  - 在线的标签页和设备，通过推送同步新消息、已读、删除、清空和好友变化。
  - 离线后重新上线时，刷新会话列表、增量同步打开着的会话，并按游标拉取离线期间的删除记录。
- **状态管理**：Zustand 的各个 store 在令牌变化时自动清空。主动退出登录时删除 IndexedDB；令牌过期导致的被动登出保留缓存。
- **UI**：克制的深色极简风格。三层背景靠明度差区分，淡黄强调色只用于核心操作和自己的消息气泡。消息流限宽 920px 居中，同一发送者 3 分钟内的连续消息分为一组。

## 接口

所有 HTTP 接口都在 `/api/v1` 下，统一返回 `{code, msg, data}`，`code` 为 0 表示成功。除注册、登录外都需要请求头 `Authorization: Bearer <token>`。列表统一返回 `{items: [...]}`。

### 账号

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/auth/register` | `{username, password}` → `{id, username, createdAt}`。用户名 3～32 位字母或数字，可以重复；密码 8～24 位，需同时包含大写字母、小写字母和数字 |
| POST | `/auth/login` | `{userId, password}` → `{token, expiresAt, user}` |
| GET | `/me` | 当前用户 |

### 好友

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/friends/search?q=` | 按用户名或用户 ID 搜索，最多 20 条，每条带 `relation`：`none` / `friend` / `pending_out` / `pending_in` |
| GET | `/friends` | 好友列表 `{id, username, avatarUrl, since}` |
| DELETE | `/friends/:friendId` | 删除好友（双向解除） |
| POST | `/friend-requests` | `{toUserId, message}` 发送申请；对同一人已有待处理申请时，直接返回那条申请 |
| GET | `/friend-requests/incoming` | 收到的待处理申请 |
| GET | `/friend-requests/outgoing` | 我发出的申请（包括已处理的） |
| POST | `/friend-requests/:id/accept` | 同意 |
| POST | `/friend-requests/:id/reject` | 拒绝 |

### 会话与消息

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/chats` | 会话列表：`{peer, lastMessage, lastSeq, readSeq, clearedSeq, unread, lastMessageAt}`；`lastMessage` 是自己能看到的最后一条，全部删除后为 null |
| GET | `/chats/:peerId/messages` | `?afterSeq=`（增量同步）或 `?beforeSeq=`（向前翻页），两个都不带时返回最新一页；`limit` 默认 50、最多 100 → `{items, hasMore, lastSeq}`，按 seq 升序 |
| POST | `/chats/:peerId/read` | `{seq}` → `{readSeq}`，只增不减 |
| POST | `/chats/:peerId/messages/delete` | `{seqs}` 删除 1～30 条消息，只对自己生效 |
| POST | `/chats/:peerId/clear` | `{upToSeq}` 清空到点击时已知的最新 seq，同时视为已读 → `{clearedSeq, readSeq}` |
| GET | `/message-deletions?after=` | 按游标增量同步自己的删除记录 → `{items: [{id, peerId, seq}], hasMore}` |

### WebSocket

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/ws/ticket` | 换一次性票据 → `{ticket, expiresAt}` |
| GET | `/ws?ticket=` | 升级为 WebSocket；票据无效时在握手阶段返回 401 |

数据包格式统一为 `{type, version: 1, data}`：

| type | 方向 | data / 说明 |
| --- | --- | --- |
| `ping` / `pong` | 双向 | 应用层心跳 |
| `message.send` | 客户端 → 服务端 | `{clientMsgId, toUserId, content}` |
| `message.ack` | 服务端 → 发送消息的那个连接 | `{clientMsgId, code, msg, message?}` |
| `message.new` | 服务端 → 会话双方的所有连接 | `{message}` |
| `chat.read` | 服务端 → 自己的所有连接 | `{peerId, readSeq}` |
| `message.deleted` | 服务端 → 自己的所有连接 | `{peerId, seqs}` |
| `chat.cleared` | 服务端 → 自己的所有连接 | `{peerId, clearedSeq, readSeq}` |
| `friend.request.received` | 服务端 → 被申请方 | 新的好友申请 |
| `friend.request.handled` | 服务端 → 申请方 | 申请被同意或拒绝 |
| `friend.sync` | 服务端 → 操作者自己的其他连接 | 好友数据有变化，客户端重新拉取 |
| `error` | 服务端 → 客户端 | `{code, msg, type}`，数据包格式错误、类型未知等 |

### 错误码

| code | 含义 |
| --- | --- |
| 10001 | 参数不符合业务规则（提示文字可以直接展示） |
| 10002 | 请求体无法解析 |
| 10003 | 未登录或令牌无效 / 过期（HTTP 401，前端跳转登录页） |
| 10004 | 请求太频繁，被限流（HTTP 429） |
| 20001 | 用户 ID 或密码错误（HTTP 401，前端不跳转） |
| 30001～30006 | 好友：不能添加自己、已是好友、对方已向你申请、申请不存在、申请已被处理、目标用户不存在 |
| 40001 / 40002 | 消息为空 / 超过 1500 字 |
| 40004 | 非好友之间，对方回复或成为好友前只能发一条消息 |
| 50000 | 服务器内部错误 |

### 限流规则

| 接口 | 维度 | 规则 |
| --- | --- | --- |
| 注册 | IP | 每小时 10 次 |
| 登录 | IP / 目标账号 | 每分钟 20 次 / 每分钟 5 次 |
| 好友申请 | 用户 | 每小时 30 次 |
| 换 WebSocket 票据 | 用户 | 每分钟 30 次 |
| 发消息 | 用户 | 每秒 5 条，突发 10 条（重发命中去重的不计数） |

## 目录结构

```
docker-compose.yml        本地 PostgreSQL
docs/requirements.md      需求说明书（功能需求、非功能需求、数据模型、里程碑）
im-server/                Go 后端
  cmd/server/main.go      入口：配置、依赖组装、路由、限流规则、优雅关闭
  internal/user/          账号
  internal/friend/        好友
  internal/message/       会话、消息、已读、删除
  internal/gateway/       WebSocket 网关
  internal/pkg/           response（统一响应、错误码）、auth（JWT）、logger、ratelimit、reqctx
  migrations/             golang-migrate 迁移文件（000001～000007）
web/                      React 前端，结构见 web/README.md
```

## 后续计划

- 连接建立后令牌过期时主动断开 WebSocket（目前要等重连换票据失败才跳转登录）
- 个人资料页（修改用户名、头像），需要文件存储
- 后续阶段：群聊、图片和文件消息、已读回执、Redis 在线状态和多实例部署、压测
