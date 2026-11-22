import { App, Button, Empty, Input, Modal, Spin } from 'antd'
import { useRef, useState, type ReactNode } from 'react'
import { useNavigate } from 'react-router'
import { MAX_REQUEST_MESSAGE_LEN, searchUsers, type Relation, type SearchItem } from '../../api/friend'
import { Code, errorMessage, isApiError } from '../../api/client'
import { UserAvatar } from '../../components/UserAvatar'
import { useAuthStore } from '../../stores/auth'
import { useFriendStore } from '../../stores/friend'

// AddFriendModal 搜索用户并发送好友申请（FR-05、FR-06）
export default function AddFriendModal({ open, onClose }: { open: boolean; onClose: () => void }) {
  return (
    // destroyOnHidden：关闭后卸载内容，下次打开时搜索框和结果都是空的
    <Modal open={open} title="添加好友" footer={null} onCancel={onClose} destroyOnHidden width={520}>
      <AddFriendContent onClose={onClose} />
    </Modal>
  )
}

function AddFriendContent({ onClose }: { onClose: () => void }) {
  const { message } = App.useApp()
  const [results, setResults] = useState<SearchItem[] | null>(null) // null 表示还没搜索过
  const [searching, setSearching] = useState(false)
  const [composingId, setComposingId] = useState<number | null>(null) // 正在填写验证信息的用户
  // 连续搜索时只采用最后一次的结果，丢弃先发出但后返回的响应
  const seq = useRef(0)

  async function onSearch(value: string) {
    const q = value.trim()
    if (!q) return
    const mySeq = ++seq.current
    setSearching(true)
    try {
      const items = await searchUsers(q)
      if (mySeq !== seq.current) return
      setResults(items)
      setComposingId(null)
    } catch (err) {
      if (mySeq === seq.current) message.error(errorMessage(err))
    } finally {
      if (mySeq === seq.current) setSearching(false)
    }
  }

  // 发送申请后，或者发现本地关系已过时，更新这一条结果的关系
  function setRelation(id: number, relation: Relation) {
    setResults((rs) => rs?.map((r) => (r.id === id ? { ...r, relation } : r)) ?? null)
  }

  return (
    <>
      <Input.Search
        autoFocus
        placeholder="输入用户名或 10 位用户 ID"
        enterButton="搜索"
        maxLength={32}
        loading={searching}
        onSearch={onSearch}
      />
      <div className="search-results">
        {searching && results === null && (
          <div style={{ textAlign: 'center', padding: 24 }}>
            <Spin />
          </div>
        )}
        {results?.length === 0 && (
          <Empty
            image={Empty.PRESENTED_IMAGE_SIMPLE}
            description="没有找到该用户（用户名需完整输入，不区分大小写）"
          />
        )}
        {results?.map((u) => (
          <ResultItem
            key={u.id}
            user={u}
            composing={composingId === u.id}
            onCompose={() => setComposingId(u.id)}
            onCancelCompose={() => setComposingId(null)}
            onRelationChange={(rel) => setRelation(u.id, rel)}
            onClose={onClose}
          />
        ))}
      </div>
    </>
  )
}

interface ResultItemProps {
  user: SearchItem
  composing: boolean
  onCompose: () => void
  onCancelCompose: () => void
  onRelationChange: (r: Relation) => void
  onClose: () => void
}

function ResultItem({ user, composing, onCompose, onCancelCompose, onRelationChange, onClose }: ResultItemProps) {
  const navigate = useNavigate()
  const me = useAuthStore((s) => s.user)!
  const send = useFriendStore((s) => s.send)
  const { message } = App.useApp()
  const [note, setNote] = useState(`我是 ${me.username}`)
  const [sending, setSending] = useState(false)

  async function onSend() {
    setSending(true)
    try {
      await send(user.id, note.trim())
      message.success('好友申请已发送')
      onRelationChange('pending_out')
      onCancelCompose()
    } catch (err) {
      message.error(errorMessage(err))
      // 关系在搜索之后变了，按服务端的判断更新按钮
      if (isApiError(err, Code.ReverseRequestPending)) onRelationChange('pending_in')
      if (isApiError(err, Code.AlreadyFriends)) onRelationChange('friend')
    } finally {
      setSending(false)
    }
  }

  const goto = (path: string) => {
    navigate(path)
    onClose()
  }

  let action: ReactNode
  switch (user.relation) {
    case 'friend':
      action = <Button onClick={() => goto(`/friends/${user.id}`)}>已是好友，查看</Button>
      break
    case 'pending_out':
      action = <Button disabled>等待验证</Button>
      break
    case 'pending_in':
      // 对方已经申请了我：不能反向再申请（后端返回 30003），引导去申请列表直接同意
      action = (
        <Button type="primary" onClick={() => goto('/friends/requests')}>
          对方已申请，去处理
        </Button>
      )
      break
    default:
      action = composing ? null : (
        <Button type="primary" onClick={onCompose}>
          添加
        </Button>
      )
  }

  return (
    <div className="card-item">
      <UserAvatar user={user} size={40} />
      <div className="card-item-main">
        <div className="card-item-name">
          <span>{user.username}</span>
          <span className="card-item-id">ID {user.id}</span>
        </div>
        {composing && (
          <div className="compose">
            <Input.TextArea
              autoFocus
              value={note}
              onChange={(e) => setNote(e.target.value)}
              maxLength={MAX_REQUEST_MESSAGE_LEN}
              showCount
              autoSize={{ minRows: 2, maxRows: 4 }}
              placeholder="填写验证信息（可选）"
            />
            <div className="compose-actions">
              <Button onClick={onCancelCompose} disabled={sending}>
                取消
              </Button>
              <Button type="primary" loading={sending} onClick={onSend}>
                发送申请
              </Button>
            </div>
          </div>
        )}
      </div>
      {action && <div className="card-item-actions">{action}</div>}
    </div>
  )
}
