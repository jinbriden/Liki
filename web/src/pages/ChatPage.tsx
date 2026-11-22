import { CommentOutlined } from '@ant-design/icons'
import { useParams } from 'react-router'
import { useFriendStore } from '../stores/friend'

// ChatPage 聊天页占位，M4（实时单聊）、M5（会话列表）实现
export default function ChatPage() {
  const { peerId } = useParams()
  const friend = useFriendStore((s) => s.friends.find((f) => String(f.id) === peerId))

  return (
    <div className="placeholder" style={{ flex: 1 }}>
      <CommentOutlined className="placeholder-icon" />
      <div style={{ color: 'var(--liki-text-2)', fontSize: 16 }}>聊天功能开发中</div>
      <div>{friend ? `与 ${friend.username} 的对话将在 M4 实现` : '会话列表和单聊将在 M4、M5 实现'}</div>
    </div>
  )
}
