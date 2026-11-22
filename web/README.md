# Liki Web

React + TypeScript + Vite 前端。技术选型和需求见仓库根目录 `docs/requirements.md`。

```bash
npm install
npm run dev     # http://127.0.0.1:5173，/api 代理到 127.0.0.1:8080（需先启动 im-server）
npm run build   # 类型检查 + 打包
npm run lint
```

## 目录

- `src/theme.ts` + `src/index.css`：黑色系主题。antd 组件用 `theme.ts` 的 token，自定义布局用 `index.css` 的 `--liki-*` 变量，两边配色保持一致
- `src/api/`：`client.ts` 封装 fetch，按业务 code 抛出 `ApiError`，遇到 10003 时清除登录状态；`auth.ts`、`friend.ts` 是各模块的接口函数和类型（对应后端 resp 结构体）
- `src/stores/`：Zustand 全局状态。`auth.ts` 是令牌读写的唯一入口（localStorage 键 `liki.token`）；`friend.ts` 存好友、申请列表，令牌变化时自动清空
- `src/routes/guards.tsx`：`RequireAuth`（需要登录）、`GuestOnly`（仅限未登录）
- `src/layouts/MainLayout.tsx`：登录后的主框架（左侧导航：聊天 / 好友 / 我的账号）
- `src/pages/`：页面组件。`friends/` 下是好友页（列表、详情、新的朋友、添加好友弹窗）；`ChatPage.tsx` 暂为占位
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
| `/chat`、`/chat/:peerId` | 聊天（占位，M4 实现） |
