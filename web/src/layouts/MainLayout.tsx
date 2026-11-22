import { LogoutOutlined, MessageOutlined, TeamOutlined } from '@ant-design/icons'
import { Badge, Button, Modal, Popover, Tooltip, Typography } from 'antd'
import { useEffect, type ReactNode } from 'react'
import { NavLink, Outlet } from 'react-router'
import { UserAvatar } from '../components/UserAvatar'
import { useAuthStore } from '../stores/auth'
import { useFriendStore } from '../stores/friend'

// MainLayout 登录后的主框架：左侧导航栏 + 右侧内容区。
// 好友页和聊天页分开（FR-09），都通过左侧导航切换。
export default function MainLayout() {
  const loadIncoming = useFriendStore((s) => s.loadIncoming)
  const pendingCount = useFriendStore((s) => s.incoming.length)

  // 进入主框架时拉一次待处理申请，让导航栏"好友"上的角标在任何页面都能显示。
  // M4 之前没有实时推送，新申请要等刷新或进入好友页才能看到。
  useEffect(() => {
    loadIncoming().catch(() => {}) // 角标拉取失败不打扰用户，好友页会再拉一次并显示错误
  }, [loadIncoming])

  return (
    <div className="app-shell">
      <nav className="app-nav">
        <div className="app-logo">L</div>
        <NavItem to="/chat" label="聊天" icon={<MessageOutlined />} />
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

// UserMenu 导航栏底部自己的头像，点击显示账号信息和退出按钮
function UserMenu() {
  // RequireAuth 保证渲染到这里时 user 一定存在
  const user = useAuthStore((s) => s.user)!
  const clear = useAuthStore((s) => s.clear)

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
      {/* 退出登录（FR-03）：只清除本地令牌，服务端没有 logout 接口。M4 起还要断开 WebSocket */}
      <Button block icon={<LogoutOutlined />} onClick={clear}>
        退出登录
      </Button>
    </div>
  )

  return (
    <Popover content={content} trigger="click" placement="rightBottom">
      <button
        type="button"
        aria-label="我的账号"
        style={{ background: 'none', border: 'none', padding: 0, cursor: 'pointer', borderRadius: '50%' }}
      >
        <UserAvatar user={user} size={36} />
      </button>
    </Popover>
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
