import { Avatar } from 'antd'

// 默认头像底色：低饱和的灰调色，在黑色背景上可区分但不跳脱
const COLORS = ['#3b4a6b', '#4b3f63', '#35574d', '#5e4b3a', '#4a4a55', '#5b3d48', '#3c5662', '#57553a']

interface Props {
  user: { id: number; username: string; avatarUrl?: string }
  size?: number
}

// UserAvatar 有头像地址就显示图片，否则显示用户名首字母。
// 底色按用户 ID 取，同一个人在任何地方颜色都一样。
export function UserAvatar({ user, size = 40 }: Props) {
  const bg = COLORS[user.id % COLORS.length]
  const initial = (user.username.charAt(0) || '?').toUpperCase()
  return (
    <Avatar
      size={size}
      src={user.avatarUrl || undefined}
      style={{ backgroundColor: bg, color: '#e8e8ea', fontSize: Math.round(size * 0.42), flexShrink: 0 }}
    >
      {initial}
    </Avatar>
  )
}
