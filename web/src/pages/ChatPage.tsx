import { CloseCircleFilled, CommentOutlined, LoadingOutlined, SearchOutlined } from '@ant-design/icons'
import { Alert, App, Button, Empty, Input, Spin, Tooltip } from 'antd'
import { useEffect, useLayoutEffect, useMemo, useRef, useState, type KeyboardEvent } from 'react'
import { NavLink, useParams } from 'react-router'
import { MAX_MESSAGE_LEN, messageLength } from '../api/message'
import { UserAvatar } from '../components/UserAvatar'
import { useAuthStore } from '../stores/auth'
import { useChatStore, type ChatMessage, type Conversation } from '../stores/chat'
import { useFriendStore } from '../stores/friend'
import { formatTime } from '../utils/time'

const NO_MESSAGES: ChatMessage[] = [] // 固定的空数组：每次渲染都 [] 会生成新引用，让依赖它的 effect 每次都执行

const itemClass = ({ isActive }: { isActive: boolean }) => 'list-item' + (isActive ? ' active' : '')

// ChatPage 聊天页：左侧是可以聊天的好友（M5 改为会话列表，带未读数），右侧是与某人的对话。
//   /chat          → 空状态
//   /chat/:peerId  → 与 peerId 的对话
export default function ChatPage() {
  const { peerId } = useParams()
  const id = Number(peerId)

  return (
    <>
      <ChatList />
      <section className="detail-pane chat-pane">
        {id > 0 ? (
          <ChatPanel key={id} peerId={id} />
        ) : (
          <div className="placeholder">
            <CommentOutlined className="placeholder-icon" />
            <div>选择一位好友开始聊天</div>
          </div>
        )}
      </section>
    </>
  )
}

// ---------- 左侧列表 ----------

function ChatList() {
  const friends = useFriendStore((s) => s.friends)
  const loaded = useFriendStore((s) => s.loaded)
  const loadFriends = useFriendStore((s) => s.loadFriends)
  const convs = useChatStore((s) => s.convs)
  const [filter, setFilter] = useState('')

  useEffect(() => {
    if (!loaded) loadFriends().catch(() => {})
  }, [loaded, loadFriends])

  // 有消息的会话按最后一条消息的时间倒序排在前面，其余按用户名（好友列表本身已排序）
  const items = useMemo(() => {
    const keyword = filter.trim().toLowerCase()
    const list = friends
      .filter((f) => !keyword || f.username.toLowerCase().includes(keyword) || String(f.id).includes(keyword))
      .map((f) => ({ friend: f, last: lastMessage(convs[f.id]) }))
    return list.sort((a, b) => (b.last?.createdAt ?? '').localeCompare(a.last?.createdAt ?? ''))
  }, [friends, convs, filter])

  return (
    <aside className="list-pane">
      <div className="list-pane-header">
        <span className="list-pane-title">聊天</span>
        <ConnectionIndicator />
      </div>
      <div className="list-pane-filter">
        <Input
          allowClear
          prefix={<SearchOutlined style={{ color: 'var(--liki-text-3)' }} />}
          placeholder="搜索好友"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
        />
      </div>
      <div className="list-pane-body">
        {loaded && friends.length === 0 && (
          <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="还没有好友，先去好友页添加吧" />
        )}
        {items.map(({ friend, last }) => (
          <NavLink key={friend.id} to={`/chat/${friend.id}`} className={itemClass}>
            <UserAvatar user={friend} size={36} />
            <div className="list-item-main">
              <div className="chat-item-head">
                <span className="list-item-title">{friend.username}</span>
                {last && <span className="chat-item-time">{formatTime(last.createdAt)}</span>}
              </div>
              <div className="list-item-sub list-item-title">
                {last ? previewOf(last) : `ID ${friend.id}`}
              </div>
            </div>
          </NavLink>
        ))}
      </div>
    </aside>
  )
}

function lastMessage(c: Conversation | undefined): ChatMessage | undefined {
  return c?.messages[c.messages.length - 1]
}

function previewOf(m: ChatMessage) {
  const prefix = m.status === 'failed' ? '[发送失败] ' : m.status === 'sending' ? '[发送中] ' : ''
  return prefix + m.content.replace(/\s+/g, ' ')
}

// ConnectionIndicator 连接不正常时在标题旁显示状态
function ConnectionIndicator() {
  const connection = useChatStore((s) => s.connection)
  if (connection === 'open' || connection === 'idle') return null
  return (
    <span className="chat-connection">
      <LoadingOutlined /> {connection === 'connecting' ? '连接中' : '连接已断开，正在重连'}
    </span>
  )
}

// ---------- 右侧对话 ----------

function ChatPanel({ peerId }: { peerId: number }) {
  const me = useAuthStore((s) => s.user!)
  const friend = useFriendStore((s) => s.friends.find((f) => f.id === peerId))
  const friendsLoaded = useFriendStore((s) => s.loaded)
  const conv = useChatStore((s) => s.convs[peerId])
  const open = useChatStore((s) => s.open)
  const loadOlder = useChatStore((s) => s.loadOlder)

  useEffect(() => {
    open(peerId)
  }, [open, peerId])

  // 好友列表加载完、对方不在其中：可以查看历史（FR-08 删除好友后记录保留），但不能发消息
  const notFriend = friendsLoaded && !friend
  const peer = friend ?? { id: peerId, username: `用户 ${peerId}`, avatarUrl: '' }
  const messages = conv?.messages ?? NO_MESSAGES

  const listRef = useRef<HTMLDivElement>(null)
  const stickToBottom = useRef(true) // 用户停留在底部附近时，新消息到来自动滚到底；往上翻看历史时不打扰
  const prependAnchor = useRef<number | null>(null) // 加载更早的消息前记下内容高度，加载后保持可视位置不跳

  const onScroll = () => {
    const el = listRef.current
    if (!el) return
    stickToBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80
  }

  useLayoutEffect(() => {
    const el = listRef.current
    if (!el) return
    if (prependAnchor.current !== null) {
      el.scrollTop += el.scrollHeight - prependAnchor.current
      prependAnchor.current = null
      return
    }
    const last = messages[messages.length - 1]
    if (stickToBottom.current || (last?.senderId === me.id && last.status !== 'sent')) {
      el.scrollTop = el.scrollHeight
      stickToBottom.current = true
    }
  }, [messages, me.id])

  const onLoadOlder = () => {
    prependAnchor.current = listRef.current?.scrollHeight ?? null
    void loadOlder(peerId)
  }

  return (
    <div className="chat">
      <header className="chat-header">
        <UserAvatar user={peer} size={32} />
        <div className="chat-header-name">{peer.username}</div>
        <div className="chat-header-id">ID {peerId}</div>
      </header>

      <div className="chat-messages" ref={listRef} onScroll={onScroll}>
        {conv?.hasMoreBefore && (
          <div className="chat-more">
            <Button type="link" size="small" loading={conv.loadingOlder} onClick={onLoadOlder}>
              查看更早的消息
            </Button>
          </div>
        )}
        {conv?.loading && !conv.loaded && (
          <div className="chat-more">
            <Spin />
          </div>
        )}
        {conv?.error && <Alert type="error" title={conv.error} showIcon style={{ margin: '8px 0' }} />}
        {conv?.loaded && messages.length === 0 && <div className="chat-empty">还没有消息，打个招呼吧</div>}

        {messages.map((m, i) => (
          <div key={m.key}>
            {showTimeDivider(messages[i - 1], m) && <div className="chat-time">{formatTime(m.createdAt)}</div>}
            <MessageRow message={m} mine={m.senderId === me.id} peer={peer} me={me} />
          </div>
        ))}
      </div>

      {notFriend ? (
        <div className="chat-disabled">对方不是你的好友，无法发送消息</div>
      ) : (
        <Composer peerId={peerId} onSent={() => (stickToBottom.current = true)} />
      )}
    </div>
  )
}

// 和上一条相隔超过 5 分钟时，插入一条时间分隔
function showTimeDivider(prev: ChatMessage | undefined, cur: ChatMessage) {
  if (!prev) return true
  return new Date(cur.createdAt).getTime() - new Date(prev.createdAt).getTime() > 5 * 60_000
}

interface RowProps {
  message: ChatMessage
  mine: boolean
  peer: { id: number; username: string; avatarUrl?: string }
  me: { id: number; username: string }
}

function MessageRow({ message: m, mine, peer, me }: RowProps) {
  const retry = useChatStore((s) => s.retry)

  return (
    <div className={'msg-row' + (mine ? ' mine' : '')}>
      <UserAvatar user={mine ? me : peer} size={32} />
      <div className="msg-body">
        <div className="msg-line">
          {mine && m.status === 'sending' && (
            <Tooltip title="发送中">
              <LoadingOutlined className="msg-status" />
            </Tooltip>
          )}
          {mine && m.status === 'failed' && (
            <Tooltip title={m.retryable ? '发送失败，点击重试' : m.error}>
              <CloseCircleFilled
                className={'msg-status failed' + (m.retryable ? ' retryable' : '')}
                onClick={m.retryable ? () => retry(m.recipientId, m.key) : undefined}
              />
            </Tooltip>
          )}
          {/* 消息内容一律按纯文本渲染（NFR-04），React 会自动转义；换行靠 CSS 的 pre-wrap 保留 */}
          <div className="msg-bubble">{m.content}</div>
        </div>
        {mine && m.status === 'failed' && m.error && <div className="msg-error">{m.error}</div>}
      </div>
    </div>
  )
}

// ---------- 输入框 ----------

function Composer({ peerId, onSent }: { peerId: number; onSent: () => void }) {
  const send = useChatStore((s) => s.send)
  const { message } = App.useApp()
  const [text, setText] = useState('')
  const len = messageLength(text)
  const tooLong = len > MAX_MESSAGE_LEN

  const submit = () => {
    if (!text.trim()) return
    const res = send(peerId, text)
    // 不管成功与否都清空输入框：过长的消息也会作为一条"发送失败"的消息留在对话里，内容不会丢
    setText('')
    onSent()
    if (!res.ok && res.error) message.error(`${res.error}（单条消息最多 ${MAX_MESSAGE_LEN} 字）`)
  }

  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    // Enter 发送，Shift+Enter 换行。输入法组字过程中的 Enter 是选词，不能发送
    if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
      e.preventDefault()
      submit()
    }
  }

  return (
    <div className="chat-composer">
      <Input.TextArea
        value={text}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={onKeyDown}
        autoSize={{ minRows: 1, maxRows: 6 }}
        placeholder="输入消息，Enter 发送，Shift + Enter 换行"
        variant="borderless"
      />
      <div className="chat-composer-footer">
        {/* 接近上限时提示字数；超出也允许点发送，消息会被标记为发送失败（只保存在本地） */}
        <span className={'chat-count' + (tooLong ? ' over' : '')}>
          {len > MAX_MESSAGE_LEN - 100 ? `${len} / ${MAX_MESSAGE_LEN}` : ''}
        </span>
        <Button type="primary" disabled={!text.trim()} onClick={submit}>
          发送
        </Button>
      </div>
    </div>
  )
}
