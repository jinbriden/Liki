import { App, Button, Empty, Tabs, Tag } from 'antd'
import { useState, type ReactNode } from 'react'
import type { FriendRequest, RequestStatus } from '../../api/friend'
import { errorMessage } from '../../api/client'
import { UserAvatar } from '../../components/UserAvatar'
import { useFriendStore } from '../../stores/friend'
import { formatTime } from '../../utils/time'

// RequestsPanel "新的朋友"：收到的待处理申请（可同意/拒绝）和我发出的申请（FR-07）
export default function RequestsPanel() {
  const incoming = useFriendStore((s) => s.incoming)
  const outgoing = useFriendStore((s) => s.outgoing)

  return (
    <div className="requests">
      <h2 className="requests-title">新的朋友</h2>
      <Tabs
        items={[
          {
            key: 'incoming',
            label: `收到的申请${incoming.length ? ` (${incoming.length})` : ''}`,
            children: incoming.length ? (
              incoming.map((r) => <IncomingItem key={r.id} req={r} />)
            ) : (
              <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无待处理的好友申请" />
            ),
          },
          {
            key: 'outgoing',
            label: '我发出的',
            children: outgoing.length ? (
              outgoing.map((r) => <OutgoingItem key={r.id} req={r} />)
            ) : (
              <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="还没有发出过好友申请" />
            ),
          },
        ]}
      />
    </div>
  )
}

function RequestCard({ req, actions }: { req: FriendRequest; actions: ReactNode }) {
  return (
    <div className="card-item">
      <UserAvatar user={req.user} size={44} />
      <div className="card-item-main">
        <div className="card-item-name">
          <span>{req.user.username}</span>
          <span className="card-item-id">ID {req.user.id}</span>
        </div>
        {/* 验证信息按纯文本渲染（React 默认转义），不使用 dangerouslySetInnerHTML（NFR-04） */}
        <div className="card-item-message">{req.message || '（没有填写验证信息）'}</div>
        <div className="card-item-time">{formatTime(req.createdAt)}</div>
      </div>
      <div className="card-item-actions">{actions}</div>
    </div>
  )
}

function IncomingItem({ req }: { req: FriendRequest }) {
  const accept = useFriendStore((s) => s.accept)
  const reject = useFriendStore((s) => s.reject)
  const { message } = App.useApp()
  const [busy, setBusy] = useState<'accept' | 'reject' | null>(null)

  async function handle(kind: 'accept' | 'reject') {
    setBusy(kind)
    try {
      if (kind === 'accept') {
        await accept(req.id)
        message.success(`已添加 ${req.user.username} 为好友`)
      } else {
        await reject(req.id)
        message.info('已拒绝该申请')
      }
      // 成功后这一项会从 store 里移除，组件随之卸载，不需要再 setBusy(null)
    } catch (err) {
      message.error(errorMessage(err))
      setBusy(null)
    }
  }

  return (
    <RequestCard
      req={req}
      actions={
        <>
          <Button disabled={busy === 'accept'} loading={busy === 'reject'} onClick={() => handle('reject')}>
            拒绝
          </Button>
          <Button
            type="primary"
            disabled={busy === 'reject'}
            loading={busy === 'accept'}
            onClick={() => handle('accept')}
          >
            同意
          </Button>
        </>
      }
    />
  )
}

const STATUS_TAG: Record<RequestStatus, { text: string; color?: string }> = {
  pending: { text: '等待验证' },
  accepted: { text: '已同意', color: 'success' },
  rejected: { text: '已拒绝', color: 'error' },
}

function OutgoingItem({ req }: { req: FriendRequest }) {
  const tag = STATUS_TAG[req.status]
  return (
    <RequestCard
      req={req}
      actions={
        <Tag color={tag.color} variant="filled" style={{ marginInlineEnd: 0 }}>
          {tag.text}
        </Tag>
      }
    />
  )
}
