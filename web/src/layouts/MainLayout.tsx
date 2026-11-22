import { LogoutOutlined, MessageOutlined, TeamOutlined } from '@ant-design/icons'
import { App, Badge, Button, Modal, Popover, Tooltip, Typography } from 'antd'
import { useEffect, type ReactNode } from 'react'
import { NavLink, Outlet } from 'react-router'
import type { FriendRequest } from '../api/friend'
import { UserAvatar } from '../components/UserAvatar'
import { socket } from '../realtime/socket'
import { useAuthStore } from '../stores/auth'
// 导入聊天 store 时它就订阅了推送，不在聊天页时收到的消息也不会丢
import { initChat, useChatStore } from '../stores/chat'
import { selectTotalUnread, useChatListStore } from '../stores/chatList'
import { useFriendStore } from '../stores/friend'
import { logout } from '../stores/session'

// MainLayout 登录后的主框架：左侧导航栏 + 右侧内容区。
// 好友页和聊天页分开（FR-09），都通过左侧导航切换。
export default function MainLayout() {
  const userId = useAuthStore((s) => s.user?.id)
  const loadIncoming = useFriendStore((s) => s.loadIncoming)
  const pendingCount = useFriendStore((s) => s.incoming.length)
  const totalUnread = useChatListStore(selectTotalUnread)

  // 进入主框架时拉一次待处理申请，让导航栏"好友"上的角标在任何页面都能显示；之后的新申请靠实时推送
  useEffect(() => {
    loadIncoming().catch(() => {}) // 角标拉取失败不打扰用户，好友页会再拉一次并显示错误
  }, [loadIncoming])

  // 登录后打开本地缓存、恢复会话列表，并建立 WebSocket 长连接；离开主框架（退出登录）时断开（FR-03）
  useEffect(() => {
    if (!userId) return
    void initChat(userId)
    socket.start()
    return () => socket.stop()
  }, [userId])

  useFriendNotifications()

  return (
    <div className="app-shell">
      <nav className="app-nav">
        <div className="app-logo" aria-label="Liki">
          <LogoMark />
        </div>
        <NavItem to="/chat" label="聊天" icon={<MessageOutlined />} badge={totalUnread} />
        <NavItem to="/friends" label="好友" icon={<TeamOutlined />} badge={pendingCount} />
        <div className="app-nav-spacer" />
        <UserMenu />
      </nav>
      <main className="app-main">
        <Outlet />
      </main>
      <NewUserModal />
    </div>
  )
}

function NavItem({ to, label, icon, badge = 0 }: { to: string; label: string; icon: ReactNode; badge?: number }) {
  return (
    <Tooltip title={label} placement="right">
      {/* NavLink 在当前路由匹配 to（含子路由）时，className 回调的 isActive 为 true */}
      <NavLink to={to} aria-label={label} className={({ isActive }) => 'app-nav-item' + (isActive ? ' active' : '')}>
        <Badge count={badge} size="small" offset={[4, -2]}>
          <span style={{ color: 'inherit', fontSize: 20 }}>{icon}</span>
        </Badge>
      </NavLink>
    </Tooltip>
  )
}

// useFriendNotifications 收到好友申请、申请被处理时弹出通知（FR-07）。数据更新在 friend store 里，这里只负责提示
function useFriendNotifications() {
  const { notification } = App.useApp()

  useEffect(() => {
    const offReceived = socket.on('friend.request.received', (data) => {
      const req = data as FriendRequest
      notification.info({
        title: '新的好友申请',
        description: `${req.user.username}（ID ${req.user.id}）请求添加你为好友${req.message ? `：${req.message}` : ''}`,
      })
    })
    const offHandled = socket.on('friend.request.handled', (data) => {
      const req = data as FriendRequest
      const accepted = req.status === 'accepted'
      notification[accepted ? 'success' : 'info']({
        title: accepted ? '好友申请已通过' : '好友申请被拒绝',
        description: accepted ? `${req.user.username} 已同意你的好友申请` : `${req.user.username} 拒绝了你的好友申请`,
      })
    })
    return () => {
      offReceived()
      offHandled()
    }
  }, [notification])
}

// UserMenu 导航栏底部自己的头像，点击显示账号信息和退出按钮
function UserMenu() {
  // RequireAuth 保证渲染到这里时 user 一定存在
  const user = useAuthStore((s) => s.user)!

  const content = (
    <div style={{ width: 220 }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 12 }}>
        <UserAvatar user={user} size={44} />
        <div style={{ minWidth: 0 }}>
          <Typography.Text strong ellipsis style={{ display: 'block' }}>
            {user.username}
          </Typography.Text>
          <Typography.Text type="secondary" copyable={{ text: String(user.id) }} style={{ fontSize: 12 }}>
            ID {user.id}
          </Typography.Text>
        </div>
      </div>
      {/* 退出登录（FR-03）：清除本地令牌和本地缓存，服务端没有 logout 接口；令牌清除后 WebSocket 随之断开 */}
      <Button block icon={<LogoutOutlined />} onClick={() => void logout()}>
        退出登录
      </Button>
    </div>
  )

  return (
    <Popover content={content} trigger="click" placement="rightBottom">
      <button type="button" aria-label="我的账号" className="app-nav-me">
        <UserAvatar user={user} size={34} />
        <PresenceDot />
      </button>
    </Popover>
  )
}

// PresenceDot 头像右下角的在线状态点：WebSocket 已连接为青色，连接中 / 断线重连中为灰色
function PresenceDot() {
  const connection = useChatStore((s) => s.connection)
  const online = connection === 'open'
  return (
    <Tooltip title={online ? '在线' : '连接中…'} placement="right">
      <span className={'presence-dot' + (online ? ' online' : '')} />
    </Tooltip>
  )
}

// LogoMark 应用图标：单色线条的对话气泡，跟随文字颜色
function LogoMark() {
  return (
    <svg width="26" height="26" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" aria-hidden>
      <path
        d="M5 5.5h14a1.5 1.5 0 0 1 1.5 1.5v8.5A1.5 1.5 0 0 1 19 17h-8.2L7 20v-3H5a1.5 1.5 0 0 1-1.5-1.5V7A1.5 1.5 0 0 1 5 5.5Z"
        strokeLinejoin="round"
      />
      <path d="M9 9.5v4h3.5" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  )
}

// NewUserModal 注册后自动登录进来时，提醒用户记住系统分配的用户 ID
function NewUserModal() {
  const newUserId = useAuthStore((s) => s.newUserId)
  const setNewUserId = useAuthStore((s) => s.setNewUserId)

  return (
    <Modal
      open={newUserId !== null}
      title="注册成功"
      okText="我记住了"
      cancelButtonProps={{ style: { display: 'none' } }}
      closable={false}
      mask={{ closable: false }}
      keyboard={false}
      onOk={() => setNewUserId(null)}
    >
      <p>你的用户 ID 是：</p>
      <Typography.Title level={3} copyable style={{ marginTop: 0 }}>
        {String(newUserId)}
      </Typography.Title>
      <p>登录时需要使用用户 ID（不是用户名），请复制保存好。别人也可以通过这个 ID 搜索到你。</p>
    </Modal>
  )
}
