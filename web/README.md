# Liki Web

React + TypeScript + Vite 前端。技术选型和需求见仓库根目录 `docs/requirements.md`。

```bash
npm install
npm run dev     # http://127.0.0.1:5173，/api（含 WebSocket /api/v1/ws）代理到 127.0.0.1:8080（需先启动 im-server）
npm run build   # 类型检查 + 打包
npm run lint
```

## 目录

- `src/theme.ts` + `src/index.css`：深色极简主题（三层背景明度差、淡黄强调色、青 / 粉点缀，规范见根目录 CLAUDE.md）。antd 组件用 `theme.ts` 的 token，自定义布局用 `index.css` 的 `--liki-*` 变量，两边配色保持一致
- `src/api/`：`client.ts` 封装 fetch，按业务 code 抛出 `ApiError`，遇到 10003 时清除登录状态；`auth.ts`、`friend.ts`、`message.ts`、`ws.ts` 是各模块的接口函数和类型（对应后端 resp 结构体）
- `src/realtime/socket.ts`：全局唯一的 WebSocket 连接。先调 `/ws/ticket` 换一次性票据再建连；25 秒心跳；断线按 1s、2s、4s…最多 30s 退避重连，网络恢复时立即重连；`socket.on(type, fn)` 订阅推送，`socket.onOpen` 在每次（重）连上时通知各 store 补齐数据。由 `MainLayout` 启动，令牌变化时断开
- `src/stores/`：Zustand 全局状态，令牌变化时都会自动清空
  - `auth.ts` 是令牌读写的唯一入口（localStorage 键 `liki.token`）；`session.ts` 的 `logout()` 是主动退出：清令牌 + 删本地缓存（令牌过期只清令牌）
  - `friend.ts` 存好友、申请列表
  - `chatList.ts` 存会话列表和未读数（`GET /chats` + 推送 + 本地快照，lastSeq / readSeq / clearedSeq 只增不减地合并），导航栏总未读也从这里算；`peers` 暂存从搜索结果进入的陌生人资料（还没有会话时用来显示用户名）
  - `chat.ts` 存各会话正在显示的消息窗口 `[winLo, winHi]`、本地待发送的消息（ACK 超时重发、失败重试，被限流的标记为可重试）、已读推进（`reportSeen` 连续推进 + 300ms 合并上报）、跳转到第一条未读 / 回到最新；删除和清空（先请求服务端，成功后从窗口和缓存中去掉），以及多设备同步（`message.deleted` / `chat.cleared` 推送，登录和重连时按游标拉取删除记录，会话列表的 clearedSeq 变大时清理缓存）
- `src/db/chatCache.ts`：IndexedDB 本地缓存（Dexie），每个用户一个库 `liki-<用户 ID>`：已确认的消息、已同步的 seq 区间、待发送的消息、会话列表快照、删除记录的同步游标（`meta` 表，version 2 新增）。读写失败一律退化为只用内存
- `src/utils/ranges.ts`：已同步 seq 区间的合并与查找；窗口扩展时区间内读本地，碰到缺口才请求服务端
- `src/routes/guards.tsx`：`RequireAuth`（需要登录）、`GuestOnly`（仅限未登录）
- `src/layouts/MainLayout.tsx`：登录后的主框架（左侧导航：聊天 / 好友 / 我的账号）
- `src/pages/`：页面组件。`friends/` 下是好友页（列表、详情、新的朋友、添加好友弹窗，搜索结果可以直接"发消息"）；`ChatPage.tsx` 是聊天页（左侧会话列表，右侧对话：滚到顶部 / 底部自动加载，IntersectionObserver 检测消息是否真正被看到，"以下为新消息"分隔线、顶部"N 条新消息"横条、底部"回到最新"按钮；消息悬停"⋯"菜单的删除 / 多选、头部"⋯"菜单的清空聊天记录；非好友时的发送限制提示、"非好友"标记和"添加好友"入口）
- `src/components/UserAvatar.tsx`：默认头像（首字母 + 按用户 ID 固定的底色）
- `src/utils/`：校验规则、时间格式化

## 路由

| 路径 | 页面 |
| --- | --- |
| `/login`、`/register` | 登录、注册（已登录访问会跳回首页） |
| `/` | 重定向到 `/friends` |
| `/friends` | 好友页，右侧为空状态 |
| `/friends/requests` | 新的朋友：收到的申请、我发出的申请 |
| `/friends/:friendId` | 好友详情 |
| `/chat`、`/chat/:peerId` | 聊天：左侧会话列表，右侧与 peerId 的对话（还没有消息时在列表顶部临时显示一项） |
