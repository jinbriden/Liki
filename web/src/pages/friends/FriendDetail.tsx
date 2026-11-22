import { DeleteOutlined, MessageOutlined, UserOutlined } from '@ant-design/icons'
import { App, Button, Tooltip, Typography } from 'antd'
import { useNavigate, useParams } from 'react-router'
import { errorMessage } from '../../api/client'
import { UserAvatar } from '../../components/UserAvatar'
import { useFriendStore } from '../../stores/friend'
import { formatDate } from '../../utils/time'

// FriendDetail 好友详情：发消息、删除好友（FR-08、FR-09）
export default function FriendDetail() {
  const { friendId } = useParams()
  const id = Number(friendId)
  const friend = useFriendStore((s) => s.friends.find((f) => f.id === id))
  const loaded = useFriendStore((s) => s.loaded)
  const remove = useFriendStore((s) => s.remove)
  const navigate = useNavigate()
  const { modal, message } = App.useApp()

  if (!friend) {
    // 列表还没加载完时不显示"不是好友"，避免刷新页面时闪一下
    if (!loaded) return null
    return (
      <div className="placeholder">
        <UserOutlined className="placeholder-icon" />
        <div>该用户不在你的好友列表中</div>
      </div>
    )
  }

  const openChat = () => navigate(`/chat/${friend.id}`)

  const confirmDelete = () => {
    modal.confirm({
      title: `删除好友"${friend.username}"？`,
      content: '删除后你们将从彼此的好友列表中移除，聊天记录会保留。',
      okText: '删除',
      okButtonProps: { danger: true },
      cancelText: '取消',
      // onOk 返回 Promise 时，确定按钮会显示加载状态直到它结束
      onOk: async () => {
        try {
          await remove(friend.id)
          message.success('已删除好友')
          navigate('/friends', { replace: true })
        } catch (err) {
          message.error(errorMessage(err))
        }
      },
    })
  }

  return (
    <div className="profile">
      {/* FR-09：点击好友头像进入与其的对话框 */}
      <Tooltip title="发消息">
        <span className="profile-avatar" onClick={openChat}>
          <UserAvatar user={friend} size={88} />
        </span>
      </Tooltip>
      <h2 className="profile-name">{friend.username}</h2>

      <div className="profile-meta">
        <div className="profile-meta-row">
          <span>用户 ID</span>
          <Typography.Text copyable={{ text: String(friend.id) }}>{friend.id}</Typography.Text>
        </div>
        <div className="profile-meta-row">
          <span>成为好友</span>
          <span>{formatDate(friend.since)}</span>
        </div>
      </div>

      <div style={{ display: 'flex', gap: 12, width: '100%' }}>
        <Button type="primary" icon={<MessageOutlined />} block onClick={openChat}>
          发消息
        </Button>
        <Button danger icon={<DeleteOutlined />} block onClick={confirmDelete}>
          删除好友
        </Button>
      </div>
    </div>
  )
}
