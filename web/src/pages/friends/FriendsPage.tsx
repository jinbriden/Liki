import { ReloadOutlined, SearchOutlined, SmileOutlined, UserAddOutlined } from '@ant-design/icons'
import { Alert, Badge, Button, Empty, Input, Spin, Tooltip } from 'antd'
import { useEffect, useState } from 'react'
import { NavLink, Outlet } from 'react-router'
import { UserAvatar } from '../../components/UserAvatar'
import { useFriendStore } from '../../stores/friend'
import AddFriendModal from './AddFriendModal'

const itemClass = ({ isActive }: { isActive: boolean }) => 'list-item' + (isActive ? ' active' : '')

// FriendsPage 好友页（FR-09）：左侧是"新的朋友"入口和好友列表，右侧按子路由显示详情。
//   /friends            → 空状态
//   /friends/requests   → 申请列表
//   /friends/:friendId  → 好友详情
// 选中项放在 URL 里，刷新页面后仍停留在同一个好友上。
export default function FriendsPage() {
  const friends = useFriendStore((s) => s.friends)
  const pendingCount = useFriendStore((s) => s.incoming.length)
  const loading = useFriendStore((s) => s.loading)
  const loaded = useFriendStore((s) => s.loaded)
  const error = useFriendStore((s) => s.error)
  const loadAll = useFriendStore((s) => s.loadAll)
  const [filter, setFilter] = useState('')
  const [addOpen, setAddOpen] = useState(false)

  // 每次进入好友页都刷新一次（M4 之前没有实时推送）。错误已记录在 store.error 里，这里不再处理
  useEffect(() => {
    loadAll().catch(() => {})
  }, [loadAll])

  const keyword = filter.trim().toLowerCase()
  const shown = keyword
    ? friends.filter((f) => f.username.toLowerCase().includes(keyword) || String(f.id).includes(keyword))
    : friends

  return (
    <>
      <aside className="list-pane">
        <div className="list-pane-header">
          <span className="list-pane-title">好友</span>
          <Tooltip title="刷新">
            <Button
              type="text"
              aria-label="刷新"
              icon={<ReloadOutlined spin={loading} />}
              onClick={() => loadAll().catch(() => {})}
            />
          </Tooltip>
          <Tooltip title="添加好友">
            <Button type="text" aria-label="添加好友" icon={<UserAddOutlined />} onClick={() => setAddOpen(true)} />
          </Tooltip>
        </div>
        <div className="list-pane-filter">
          <Input
            allowClear
            prefix={<SearchOutlined style={{ color: 'var(--liki-text-3)' }} />}
            placeholder="筛选好友"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
          />
        </div>

        <div className="list-pane-body">
          <NavLink to="/friends/requests" className={itemClass}>
            <div className="icon-tile">
              <SmileOutlined />
            </div>
            <div className="list-item-main">新的朋友</div>
            <Badge count={pendingCount} />
          </NavLink>

          <div className="list-section-title">好友 {loaded ? friends.length : ''}</div>

          {error && (
            <Alert
              type="error"
              title={error}
              showIcon
              action={
                <Button size="small" onClick={() => loadAll().catch(() => {})}>
                  重试
                </Button>
              }
              style={{ margin: '0 4px 8px' }}
            />
          )}
          {!loaded && loading && (
            <div style={{ textAlign: 'center', padding: 24 }}>
              <Spin />
            </div>
          )}
          {loaded && friends.length === 0 && (
            <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="还没有好友，点击右上角添加" />
          )}
          {loaded && friends.length > 0 && shown.length === 0 && (
            <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="没有匹配的好友" />
          )}

          {shown.map((f) => (
            <NavLink key={f.id} to={`/friends/${f.id}`} className={itemClass}>
              <UserAvatar user={f} size={36} />
              <div className="list-item-main">
                <div className="list-item-title">{f.username}</div>
                {/* 用户名可以重名，显示 ID 便于区分 */}
                <div className="list-item-sub">ID {f.id}</div>
              </div>
            </NavLink>
          ))}
        </div>
      </aside>

      <section className="detail-pane">
        <Outlet />
      </section>

      <AddFriendModal open={addOpen} onClose={() => setAddOpen(false)} />
    </>
  )
}

// FriendsEmpty 未选中任何好友时右侧的占位
export function FriendsEmpty() {
  return (
    <div className="placeholder">
      <SmileOutlined className="placeholder-icon" />
      <div>选择一位好友查看详情</div>
    </div>
  )
}
