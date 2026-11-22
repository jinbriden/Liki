# Liki Web

React + TypeScript + Vite 前端。技术选型和需求见仓库根目录 `docs/requirements.md`。

```bash
npm install
npm run dev     # http://127.0.0.1:5173，/api（含 WebSocket /api/v1/ws）代理到 127.0.0.1:8080（需先启动 im-server）
npm run build   # 类型检查 + 打包
npm run lint
```

## 目录

- `src/theme.ts` + `src/index.css`：黑色系主题。antd 组件用 `theme.ts` 的 token，自定义布局用 `index.css` 的 `--liki-*` 变量，两边配色保持一致
- `src/api/`：`client.ts` 封装 fetch，按业务 code 抛出 `ApiError`，遇到 10003 时清除登录状态；`auth.ts`、`friend.ts`、`message.ts`、`ws.ts` 是各模块的接口函数和类型（对应后端 resp 结构体）
- `src/realtime/socket.ts`：全局唯一的 WebSocket 连接。先调 `/ws/ticket` 换一次性票据再建连；25 秒心跳；断线按 1s、2s、4s…最多 30s 退避重连，网络恢复时立即重连；`socket.on(type, fn)` 订阅推送，`socket.onOpen` 在每次（重）连上时通知各 store 补齐数据。由 `MainLayout` 启动，令牌变化时断开
- `src/stores/`：Zustand 全局状态。`auth.ts` 是令牌读写的唯一入口（localStorage 键 `liki.token`）；`friend.ts` 存好友、申请列表；`chat.ts` 存各会话的消息和发送中的消息（ACK 超时重发、失败重试、按 seq 合并去重）。令牌变化时都会自动清空
- `src/routes/guards.tsx`：`RequireAuth`（需要登录）、`GuestOnly`（仅限未登录）
- `src/layouts/MainLayout.tsx`：登录后的主框架（左侧导航：聊天 / 好友 / 我的账号）
- `src/pages/`：页面组件。`friends/` 下是好友页（列表、详情、新的朋友、添加好友弹窗）；`ChatPage.tsx` 是聊天页（左侧好友列表，右侧对话）
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
| `/chat`、`/chat/:peerId` | 聊天：与 peerId 的对话。M5 起左侧改为会话列表 |
