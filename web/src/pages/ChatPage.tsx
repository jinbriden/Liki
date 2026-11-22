import {
  ArrowDownOutlined,
  CloseCircleFilled,
  CommentOutlined,
  DeleteOutlined,
  LoadingOutlined,
  MoreOutlined,
  PaperClipOutlined,
  SearchOutlined,
  SmileOutlined,
  UserAddOutlined,
} from '@ant-design/icons'
import { Alert, App, Badge, Button, Checkbox, Dropdown, Empty, Input, Popover, Spin, Tooltip } from 'antd'
import { useEffect, useLayoutEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from 'react'
import { NavLink, useNavigate, useParams } from 'react-router'
import { errorMessage } from '../api/client'
import { MAX_REQUEST_MESSAGE_LEN, type Profile } from '../api/friend'
import { MAX_DELETE_BATCH, MAX_MESSAGE_LEN, messageLength } from '../api/message'
import { UserAvatar } from '../components/UserAvatar'
import { useAuthStore } from '../stores/auth'
import { useChatStore, type ChatMessage, type Conversation } from '../stores/chat'
import { unreadOf, useChatListStore } from '../stores/chatList'
import { useFriendStore } from '../stores/friend'
import { formatClock, formatTime } from '../utils/time'

const NO_MESSAGES: ChatMessage[] = [] // 固定的空数组：每次渲染都 [] 会生成新引用，让依赖它的 effect 每次都执行
const EDGE = 200 // 距离顶部 / 底部多少像素以内就开始加载更早 / 更新的消息
const NEAR_BOTTOM = 80 // 距离底部多少像素以内算"在底部"：新消息到来时自动滚到底
const TIME_DIVIDER_GAP = 5 * 60_000 // 和上一条相隔超过 5 分钟，插入时间分隔
const GROUP_GAP = 3 * 60_000 // 同一发送者 3 分钟内的连续消息归为一组（只在组的第一条显示头像）

const itemClass = ({ isActive }: { isActive: boolean }) => 'list-item' + (isActive ? ' active' : '')
const displayName = (p: Profile) => p.username || `用户 ${p.id}`
const time = (iso: string | undefined) => (iso ? Date.parse(iso) : 0)

// ChatPage 聊天页：左侧是会话列表（FR-14），右侧是与某人的对话。
//   /chat          → 空状态
//   /chat/:peerId  → 与 peerId 的对话
export default function ChatPage() {
  const { peerId } = useParams()
  const id = Number(peerId)

  return (
    <>
      <ChatList activePeer={id > 0 ? id : null} />
      <section className="detail-pane chat-pane">
        {id > 0 ? (
          <ChatPanel key={id} peerId={id} />
        ) : (
          <div className="placeholder">
            <CommentOutlined className="placeholder-icon" />
            <div>选择一个会话开始聊天</div>
          </div>
        )}
      </section>
    </>
  )
}

// usePeer 对方的资料：会话列表里有就用它（包括已经不是好友的人），其次是好友列表、从搜索结果进来时记下的资料，
// 都没有时只显示 ID
function usePeer(peerId: number): Profile {
  const fromChat = useChatListStore((s) => s.chats[peerId]?.peer)
  const hint = useChatListStore((s) => s.peers[peerId])
  const friend = useFriendStore((s) => s.friends.find((f) => f.id === peerId))
  if (fromChat?.username) return fromChat
  if (friend) return { id: friend.id, username: friend.username, avatarUrl: friend.avatarUrl }
  return hint ?? fromChat ?? { id: peerId, username: '', avatarUrl: '' }
}

// ---------- 左侧会话列表 ----------

interface ListItem {
  peer: Profile
  lastAt: number
  preview: string
  unread: number
  temp: boolean // 临时项：还没有任何消息，只是当前打开着
}

function ChatList({ activePeer }: { activePeer: number | null }) {
  const chats = useChatListStore((s) => s.chats)
  const listLoaded = useChatListStore((s) => s.loaded)
  const listError = useChatListStore((s) => s.error)
  const locals = useChatStore((s) => s.locals)
  const hints = useChatListStore((s) => s.peers)
  const friends = useFriendStore((s) => s.friends)
  const friendsLoaded = useFriendStore((s) => s.loaded)
  const loadFriends = useFriendStore((s) => s.loadFriends)
  const [filter, setFilter] = useState('')

  // 好友列表用于：临时项显示对方资料、判断能不能发消息
  useEffect(() => {
    if (!friendsLoaded) loadFriends().catch(() => {})
  }, [friendsLoaded, loadFriends])

  // 列表 = 有消息的会话 ∪ 只有本地待发送消息的会话 ∪ 当前打开的临时项；按最后一条消息的时间倒序，临时项在最前
  const items = useMemo(() => {
    const ids = new Set<number>(Object.keys(chats).map(Number))
    for (const [peer, list] of Object.entries(locals)) if (list.length > 0) ids.add(Number(peer))
    if (activePeer !== null) ids.add(activePeer)

    const out: ListItem[] = []
    for (const id of ids) {
      const s = chats[id]
      const local = locals[id]?.at(-1)
      const friend = friends.find((f) => f.id === id)
      const peer = s?.peer.username ? s.peer : (friend ?? hints[id] ?? s?.peer ?? { id, username: '', avatarUrl: '' })
      const serverAt = time(s?.lastMessageAt)
      const localAt = time(local?.createdAt)
      let preview = ''
      if (local && localAt >= serverAt) {
        preview = (local.status === 'failed' ? '[发送失败] ' : '[发送中] ') + local.content
      } else if (s?.lastMessage) {
        preview = s.lastMessage.content
      }
      out.push({
        peer,
        lastAt: Math.max(serverAt, localAt),
        preview: preview.replace(/\s+/g, ' '),
        unread: unreadOf(s),
        temp: !s && !local,
      })
    }

    const keyword = filter.trim().toLowerCase()
    return out
      .filter((it) => !keyword || it.peer.username.toLowerCase().includes(keyword) || String(it.peer.id).includes(keyword))
      .sort((a, b) => Number(b.temp) - Number(a.temp) || b.lastAt - a.lastAt)
  }, [chats, locals, hints, friends, activePeer, filter])

  return (
    <aside className="list-pane">
      <div className="list-pane-header">
        <span className="list-pane-title">聊天</span>
        {/* 连接正常时显示会话数，断线时换成连接状态 */}
        <ConnectionIndicator>
          <span className="list-pane-count">{Object.keys(chats).length} 个会话</span>
        </ConnectionIndicator>
      </div>
      <div className="list-pane-filter">
        <Input
          allowClear
          prefix={<SearchOutlined style={{ color: 'var(--liki-text-2)', fontSize: 15 }} />}
          placeholder="搜索会话"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
        />
      </div>
      <div className="list-pane-body">
        {listError && !listLoaded && <Alert type="error" title={listError} showIcon style={{ margin: 8 }} />}
        {listLoaded && items.length === 0 && (
          <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="还没有会话，去好友页找人聊聊吧" />
        )}
        {items.map((it) => (
          <NavLink key={it.peer.id} to={`/chat/${it.peer.id}`} className={itemClass}>
            <UserAvatar user={it.peer} size={40} />
            <div className="list-item-main">
              <div className="chat-item-head">
                <span className="list-item-title">{displayName(it.peer)}</span>
                {it.lastAt > 0 && <span className="chat-item-time">{formatTime(new Date(it.lastAt).toISOString())}</span>}
              </div>
              <div className="chat-item-foot">
                <span className="list-item-sub list-item-title">{it.temp ? `ID ${it.peer.id}` : it.preview}</span>
                {it.unread > 0 && <Badge count={it.unread} overflowCount={99} size="small" />}
              </div>
            </div>
          </NavLink>
        ))}
      </div>
    </aside>
  )
}

// ConnectionIndicator 连接不正常时在标题旁显示状态，正常时显示 children
function ConnectionIndicator({ children }: { children?: ReactNode }) {
  const connection = useChatStore((s) => s.connection)
  if (connection === 'open' || connection === 'idle') return children
  return (
    <span className="chat-connection">
      <LoadingOutlined /> {connection === 'connecting' ? '连接中' : '连接已断开，正在重连'}
    </span>
  )
}

// ---------- 右侧对话 ----------

// compose 对话框里实际显示的消息：窗口内已确认的消息 + 时间落在窗口范围内的本地消息（发送中、发送失败），按时间合并。
// 窗口不在最新时，只显示时间在窗口最后一条之前的本地消息；新发的消息要等回到最新才显示。
function compose(conv: Conversation | undefined, locals: ChatMessage[]): ChatMessage[] {
  const win = conv?.messages ?? NO_MESSAGES
  if (!conv?.ready) return locals.length > 0 ? locals : NO_MESSAGES
  const lo = conv.hasMoreBefore && win.length > 0 ? time(win[0].createdAt) : -Infinity
  const hi = conv.atLatest ? Infinity : time(win.at(-1)?.createdAt)
  const shown = locals.filter((m) => time(m.createdAt) >= lo && time(m.createdAt) <= hi)
  if (shown.length === 0) return win
  const out: ChatMessage[] = []
  let j = 0
  for (const m of win) {
    while (j < shown.length && time(shown[j].createdAt) < time(m.createdAt)) out.push(shown[j++])
    out.push(m)
  }
  return out.concat(shown.slice(j))
}

function ChatPanel({ peerId }: { peerId: number }) {
  const me = useAuthStore((s) => s.user!)
  const peer = usePeer(peerId)
  const friend = useFriendStore((s) => s.friends.find((f) => f.id === peerId))
  const friendsLoaded = useFriendStore((s) => s.loaded)
  const summary = useChatListStore((s) => s.chats[peerId])
  const conv = useChatStore((s) => s.convs[peerId])
  const locals = useChatStore((s) => s.locals[peerId]) ?? NO_MESSAGES
  const open = useChatStore((s) => s.open)
  const close = useChatStore((s) => s.close)
  const loadOlder = useChatStore((s) => s.loadOlder)
  const loadNewer = useChatStore((s) => s.loadNewer)
  const jumpToUnread = useChatStore((s) => s.jumpToUnread)
  const jumpToLatest = useChatStore((s) => s.jumpToLatest)
  const markAllRead = useChatStore((s) => s.markAllRead)
  const reportSeen = useChatStore((s) => s.reportSeen)
  const deleteMessages = useChatStore((s) => s.deleteMessages)
  const clearChat = useChatStore((s) => s.clearChat)
  const { modal, message } = App.useApp()

  useEffect(() => {
    open(peerId)
    return () => close(peerId)
  }, [open, close, peerId])

  // 好友列表加载完、对方不在其中：按非好友限制发送（FR-12）——对方回复或成为好友前只能发一条。
  // 这里只是提前提示，最终以服务端为准：本地判断错了（例如对方删了我、本地还不知道），发送时服务端返回 40004，消息标记为失败
  const notFriend = friendsLoaded && !friend
  // 最后一条消息是自己发的（或者还有发送中的消息）就不能再发。lastMessage 不是最后一条时（M6 删除后摘要会落到更早的消息）
  // 不知道最后一条是谁发的，不拦，交给服务端判断
  const last = summary?.lastMessage
  const lastIsMine = !!last && last.seq === summary?.lastSeq && last.senderId === me.id
  const strangerBlocked = notFriend && (lastIsMine || locals.some((m) => m.status === 'sending'))
  const loadIncoming = useFriendStore((s) => s.loadIncoming)
  const loadOutgoing = useFriendStore((s) => s.loadOutgoing)
  useEffect(() => {
    // 头部"添加好友"按钮要知道是否已经申请过 / 对方是否申请了我
    if (notFriend) void Promise.all([loadIncoming(), loadOutgoing()]).catch(() => {})
  }, [notFriend, loadIncoming, loadOutgoing])

  const messages = useMemo(() => compose(conv, locals), [conv, locals])

  const listRef = useRef<HTMLDivElement>(null)
  const stickToBottom = useRef(true) // 用户停留在底部附近时，新消息到来自动滚到底；往上翻看历史时不打扰
  const [nearBottom, setNearBottom] = useState(true)
  // 加载更早的消息前记下第一条消息的 DOM 节点和它的位置，加载后按它移动的距离补偿，可视位置不跳。
  // 不能用"内容总高度的变化"来补偿：等待期间底部也可能追加了消息（向下加载、新推送），会把视图多推下去一截
  const prepend = useRef<{ node: HTMLElement; top: number; firstKey: string } | null>(null)

  // checkEdges 滚动、内容变化后调用：靠近顶部就加载更早的，靠近底部（且窗口不在最新）就加载更新的（FR-18）。
  // 内容不满一屏时 scrollTop 为 0，也会触发向上加载，直到填满或没有更早的消息。
  // 上次加载出错时只在用户主动滚动时重试，否则每次重新渲染都会立刻再请求一次
  const checkEdges = (byUser: boolean) => {
    const el = listRef.current
    if (!el || !conv?.ready || conv.loading) return
    const dist = el.scrollHeight - el.scrollTop - el.clientHeight
    stickToBottom.current = dist < NEAR_BOTTOM && conv.atLatest
    setNearBottom(dist < NEAR_BOTTOM)
    if (conv.error && !byUser) return
    if (el.scrollTop < EDGE && conv.hasMoreBefore && !conv.loadingOlder) {
      const node = el.querySelector<HTMLElement>('.msg-wrap')
      prepend.current = node && messages[0] ? { node, top: node.offsetTop, firstKey: messages[0].key } : null
      void loadOlder(peerId)
    }
    if (dist < EDGE && !conv.atLatest && !conv.loadingNewer) void loadNewer(peerId)
  }

  useLayoutEffect(() => {
    const el = listRef.current
    if (!el) return
    const p = prepend.current
    if (p && messages[0]?.key !== p.firstKey) {
      prepend.current = null
      // 只有原来的第一条还在（说明是在前面插入了更早的消息）才补偿；整个窗口被替换（跳转）时不补偿
      if (p.node.isConnected && messages.some((m) => m.key === p.firstKey)) {
        el.scrollTop += p.node.offsetTop - p.top
        return
      }
    }
    if (stickToBottom.current) el.scrollTop = el.scrollHeight
  }, [messages])

  // 跳转：滚到第一条 seq >= 目标的消息（它上面就是"以下为新消息"分隔线），或滚到底。放在上一个 effect 之后，覆盖它
  const scrollTarget = conv?.scrollTarget
  useLayoutEffect(() => {
    const el = listRef.current
    if (!el || !scrollTarget) return
    if (scrollTarget.kind === 'bottom') {
      el.scrollTop = el.scrollHeight
      stickToBottom.current = true
      return
    }
    const rows = el.querySelectorAll<HTMLElement>('[data-msg-seq]')
    const row = [...rows].find((r) => Number(r.dataset.msgSeq) >= scrollTarget.seq)
    if (row) {
      el.scrollTop = row.offsetTop - 48 // 留出顶部横条的位置
      stickToBottom.current = false
    }
  }, [scrollTarget])

  useEffect(() => checkEdges(false)) // 每次渲染后检查一次（内容变化、加载完成）

  // 已读（FR-15）：每条已确认消息的底部有一个 1px 的标记，标记进入可视范围 = 这条消息的底边出现在了屏幕上。
  // 页面在后台（切到别的标签页、最小化）时看不到，不算；回到前台时把当前可见的补报一次
  const observer = useRef<IntersectionObserver | null>(null)
  useEffect(() => {
    const root = listRef.current
    if (!root) return
    const inView = new Set<number>()
    const flush = () => {
      if (document.visibilityState === 'visible' && inView.size > 0) reportSeen(peerId, [...inView])
    }
    const obs = new IntersectionObserver(
      (entries) => {
        for (const e of entries) {
          const seq = Number((e.target as HTMLElement).dataset.seenSeq)
          if (e.isIntersecting) inView.add(seq)
          else inView.delete(seq)
        }
        flush()
      },
      { root },
    )
    observer.current = obs
    root.querySelectorAll('[data-seen-seq]').forEach((n) => obs.observe(n))
    document.addEventListener('visibilitychange', flush)
    return () => {
      obs.disconnect()
      observer.current = null
      document.removeEventListener('visibilitychange', flush)
    }
  }, [peerId, reportSeen])

  useLayoutEffect(() => {
    const root = listRef.current
    const obs = observer.current
    if (!root || !obs) return
    root.querySelectorAll('[data-seen-seq]').forEach((n) => obs.observe(n)) // 重复 observe 同一个元素没有副作用
  }, [messages])

  // 已读位置或最新 seq 变了（会话列表刚拉下来、其他标签页读了、新消息到了）：用已经看到过的消息重新推进一次
  useEffect(() => {
    reportSeen(peerId, [])
  }, [peerId, reportSeen, summary?.readSeq, summary?.lastSeq, conv?.messages])

  // 打开后稍等一下再决定要不要显示"N 条新消息"横条：未读全在屏幕内时，已读会在第一轮检测后立即推进，横条不会闪一下
  const [settled, setSettled] = useState(false)
  useEffect(() => {
    const t = setTimeout(() => setSettled(true), 300)
    return () => clearTimeout(t)
  }, [])

  const unread = unreadOf(summary)
  const divider = conv?.dividerSeq
  // 顶部横条：有未读被跳过了——看到过更靠后的消息，第一条未读却没看到（打开时停在最新而未读更早、
  // 停在底部时一下来很多条、滚动时跳过了一段）。点"跳转"回到第一条未读，或直接"标记为已读"
  const showTopBar = settled && !!summary && unread > 0 && (conv?.maxSeen ?? 0) > summary.readSeq
  // 分隔线只在窗口真的包含第一条未读时才画（画在 seq >= 它的第一条上面，第一条未读被删除时也能落到下一条）。
  // 窗口从更晚的位置开始时（打开停在最新、未读更早），第一条未读不在这里，不能画在窗口第一条上面
  const dividerKey =
    divider && conv && conv.winLo <= divider ? messages.find((m) => m.seq !== undefined && m.seq >= divider)?.key : undefined
  // 底部按钮：不在底部、或窗口不在最新时显示；有没看到的新消息时显示条数
  const rows = useMemo(() => layoutRows(messages, dividerKey), [messages, dividerKey])
  const newBelow = summary ? Math.max(0, summary.lastSeq - Math.max(summary.readSeq, conv?.maxSeen ?? 0)) : 0
  const showBottom = !!conv?.ready && (!conv.atLatest || !nearBottom)

  const toBottom = () => {
    if (conv?.atLatest) {
      const el = listRef.current
      if (el) el.scrollTop = el.scrollHeight
      stickToBottom.current = true
    } else {
      jumpToLatest(peerId)
    }
  }

  // ---------- 删除（FR-19）：单条删除、多选批量删除（最多 30 条）、清空聊天记录 ----------
  const [selecting, setSelecting] = useState(false)
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set())
  // 只算还在对话框里的：选中之后在其他标签页被删掉的不算
  const selectedKeys = useMemo(() => messages.filter((m) => selected.has(m.key)).map((m) => m.key), [messages, selected])

  const exitSelect = () => {
    setSelecting(false)
    setSelected(new Set())
  }

  const toggleSelect = (m: ChatMessage) => {
    if (m.status === 'sending') return // 发送中的不能删：可能马上就写库了
    if (!selected.has(m.key) && selectedKeys.length >= MAX_DELETE_BATCH) {
      message.warning(`一次最多选择 ${MAX_DELETE_BATCH} 条消息`)
      return
    }
    setSelected((cur) => {
      const next = new Set(cur)
      if (!next.delete(m.key)) next.add(m.key)
      return next
    })
  }

  const confirmDelete = (keys: string[]) => {
    if (keys.length === 0) return
    modal.confirm({
      title: keys.length === 1 ? '删除这条消息？' : `删除选中的 ${keys.length} 条消息？`,
      content: '删除后在你登录的所有设备上都不再显示，对方仍能看到。',
      okText: '删除',
      okButtonProps: { danger: true },
      cancelText: '取消',
      onOk: async () => {
        try {
          await deleteMessages(peerId, keys)
          exitSelect()
        } catch (err) {
          message.error(errorMessage(err))
        }
      },
    })
  }

  const confirmClear = () => {
    modal.confirm({
      title: `清空与"${displayName(peer)}"的聊天记录？`,
      content: '清空后在你登录的所有设备上都不再显示，对方仍能看到。此操作无法撤销。',
      okText: '清空',
      okButtonProps: { danger: true },
      cancelText: '取消',
      onOk: async () => {
        try {
          await clearChat(peerId)
          exitSelect()
        } catch (err) {
          message.error(errorMessage(err))
        }
      },
    })
  }

  const onMessageAction = (action: 'delete' | 'select', m: ChatMessage) => {
    if (action === 'delete') {
      confirmDelete([m.key])
      return
    }
    setSelecting(true)
    setSelected(new Set(m.status === 'sending' ? [] : [m.key]))
  }

  const onSent = () => {
    // 发送即已读：窗口不在最新时（正在看更早的消息）先回到最新，刚发出的消息才看得到
    if (conv && !conv.atLatest) jumpToLatest(peerId)
    stickToBottom.current = true
  }

  return (
    <div className="chat">
      <header className="chat-header">
        <UserAvatar user={peer} size={32} />
        <div className="chat-header-name">{displayName(peer)}</div>
        <div className="chat-header-id">ID {peerId}</div>
        {notFriend && <div className="chat-header-tag">非好友</div>}
        {/* 占位：功能还没做，先平衡宽屏下的左右视觉重心 */}
        <div className="chat-header-actions">
          {notFriend && <AddFriendAction peerId={peerId} />}
          <ComingSoon title="搜索聊天记录" icon={<SearchOutlined />} />
          <ComingSoon title="会话信息" icon={<PanelRightIcon />} />
          <Dropdown
            trigger={['click']}
            placement="bottomRight"
            menu={{
              items: [
                { key: 'select', label: '多选消息', disabled: messages.length === 0 },
                { key: 'clear', label: '清空聊天记录', danger: true, disabled: messages.length === 0 },
              ],
              onClick: ({ key }) => (key === 'clear' ? confirmClear() : setSelecting(true)),
            }}
          >
            <Button type="text" size="small" icon={<MoreOutlined />} aria-label="更多操作" />
          </Dropdown>
        </div>
      </header>

      <div className="chat-body">
        {showTopBar && (
          <div className="chat-unread-bar">
            <span>{unread > 99 ? '99+' : unread} 条新消息</span>
            <span className="chat-unread-actions">
              <Button type="link" size="small" onClick={() => void jumpToUnread(peerId)}>
                跳转
              </Button>
              <Button type="text" size="small" onClick={() => markAllRead(peerId)}>
                标记为已读
              </Button>
            </span>
          </div>
        )}

        <div className="chat-messages" ref={listRef} onScroll={() => checkEdges(true)}>
          {/* 限宽居中的消息流。不能设 position，否则消息行的 offsetParent 会变成它，跳转定位会算错 */}
          <div className="chat-stream">
            {(conv?.loadingOlder || (conv?.loading && !conv.ready)) && (
              <div className="chat-more">
                <Spin size="small" />
              </div>
            )}
            {conv?.error && <Alert type="error" title={conv.error} showIcon style={{ margin: '8px 0' }} />}
            {conv?.ready && messages.length === 0 && <div className="chat-empty">还没有消息，打个招呼吧</div>}

            {rows.map(({ message: m, timeDivider, pos }) => (
              <div key={m.key} className="msg-wrap" data-msg-seq={m.seq}>
                {m.key === dividerKey && (
                  <div className="chat-divider">
                    <span>以下为新消息</span>
                  </div>
                )}
                {timeDivider && (
                  <div className="chat-time">
                    <span>{formatTime(m.createdAt)}</span>
                  </div>
                )}
                <MessageRow
                  message={m}
                  mine={m.senderId === me.id}
                  pos={pos}
                  peer={peer}
                  selecting={selecting}
                  selected={selected.has(m.key)}
                  onToggle={toggleSelect}
                  onAction={onMessageAction}
                />
                {m.seq !== undefined && <span className="msg-seen" data-seen-seq={m.seq} />}
              </div>
            ))}

            {conv?.loadingNewer && (
              <div className="chat-more">
                <Spin size="small" />
              </div>
            )}
          </div>
        </div>

        {conv?.loading && conv.ready && (
          <div className="chat-loading-mask">
            <Spin />
          </div>
        )}

        {showBottom && (
          <Button className="chat-jump-bottom" shape="round" icon={<ArrowDownOutlined />} onClick={toBottom}>
            {newBelow > 0 ? `${newBelow > 99 ? '99+' : newBelow} 条新消息` : '回到最新'}
          </Button>
        )}
      </div>

      <div className="chat-composer-wrap">
        {selecting ? (
          <div className="chat-select-bar">
            <span className="chat-select-count">
              已选 {selectedKeys.length} / {MAX_DELETE_BATCH}
            </span>
            <Button onClick={exitSelect}>取消</Button>
            <Button danger type="primary" icon={<DeleteOutlined />} disabled={selectedKeys.length === 0} onClick={() => confirmDelete(selectedKeys)}>
              删除
            </Button>
          </div>
        ) : strangerBlocked ? (
          <div className="chat-disabled">对方回复或成为好友前，只能发送一条消息</div>
        ) : (
          <>
            {notFriend && <div className="chat-stranger-hint">你们还不是好友，对方回复前你只能发送一条消息</div>}
            <Composer peerId={peerId} onSent={onSent} />
          </>
        )}
      </div>
    </div>
  )
}

// ---------- 连续消息分组 ----------
//
// 只根据已有的 senderId、createdAt 在渲染时计算，不改消息的数据结构，也不改 store：
// 已确认的消息、发送中 / 发送失败的本地消息都有这两个字段，统一处理。
// 同一发送者、和上一条相隔不超过 GROUP_GAP 的消息归为一组；遇到时间分隔、"以下为新消息"分隔线时强制另起一组，
// 这样分隔线上下的消息不会被画成连在一起的气泡。

type GroupPos = 'single' | 'first' | 'middle' | 'last'

interface Row {
  message: ChatMessage
  timeDivider: boolean // 在这条上面插入时间分隔
  pos: GroupPos // 在组里的位置：决定是否显示头像、气泡圆角和间距
}

// 和上一条相隔超过 5 分钟时，插入一条时间分隔
function showTimeDivider(prev: ChatMessage | undefined, cur: ChatMessage) {
  if (!prev) return true
  return time(cur.createdAt) - time(prev.createdAt) > TIME_DIVIDER_GAP
}

// startsGroup cur 是否要另起一组（和 prev 不能连在一起）
function startsGroup(prev: ChatMessage | undefined, cur: ChatMessage, dividerKey: string | undefined) {
  if (!prev || showTimeDivider(prev, cur) || cur.key === dividerKey) return true
  return prev.senderId !== cur.senderId || time(cur.createdAt) - time(prev.createdAt) > GROUP_GAP
}

function layoutRows(messages: ChatMessage[], dividerKey: string | undefined): Row[] {
  return messages.map((m, i) => {
    const first = startsGroup(messages[i - 1], m, dividerKey)
    const next = messages[i + 1]
    const last = !next || startsGroup(m, next, dividerKey)
    const pos: GroupPos = first ? (last ? 'single' : 'first') : last ? 'last' : 'middle'
    return { message: m, timeDivider: showTimeDivider(messages[i - 1], m), pos }
  })
}

interface RowProps {
  message: ChatMessage
  mine: boolean
  pos: GroupPos
  peer: Profile
  selecting: boolean // 多选模式：每行前面显示勾选框，点击整行切换选中
  selected: boolean
  onToggle: (m: ChatMessage) => void
  onAction: (action: 'delete' | 'select', m: ChatMessage) => void
}

const MESSAGE_MENU = [
  { key: 'select', label: '多选' },
  { key: 'delete', label: '删除', danger: true },
]

function MessageRow({ message: m, mine, pos, peer, selecting, selected, onToggle, onAction }: RowProps) {
  const retry = useChatStore((s) => s.retry)
  // 只有对方的消息显示头像（组的第一条）；自己的消息靠右、实心气泡已经足够区分，不显示头像
  const showAvatar = !mine && (pos === 'first' || pos === 'single')
  const sending = m.status === 'sending' // 发送中的消息不能删除、不能选

  const cls = `msg-row ${pos}` + (mine ? ' mine' : '') + (selecting ? ' selecting' : '') + (selected ? ' selected' : '')
  return (
    <div className={cls} onClick={selecting ? () => onToggle(m) : undefined}>
      {selecting && (
        <div className="msg-check">
          <Checkbox checked={selected} disabled={sending} />
        </div>
      )}
      {/* 组内后面的消息不重复渲染头像，但保留同样宽度的位置，气泡对齐 */}
      {!mine && <div className="msg-avatar">{showAvatar && <UserAvatar user={peer} size={32} />}</div>}
      <div className="msg-body">
        <div className="msg-line">
          {/* 消息内容一律按纯文本渲染（NFR-04），React 会自动转义；换行靠 CSS 的 pre-wrap 保留 */}
          <div className="msg-bubble">{m.content}</div>
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
          {/* 具体时间默认隐藏，悬停到这一行时淡入 */}
          <span className="msg-time" title={formatTime(m.createdAt)}>
            {formatClock(m.createdAt)}
          </span>
          {/* 悬停时出现的操作菜单（FR-19）。多选模式下点击整行就是选中，不再显示 */}
          {!selecting && !sending && (
            <Dropdown
              trigger={['click']}
              placement={mine ? 'bottomRight' : 'bottomLeft'}
              menu={{ items: MESSAGE_MENU, onClick: ({ key }) => onAction(key as 'delete' | 'select', m) }}
            >
              <Button type="text" size="small" className="msg-more" icon={<MoreOutlined />} aria-label="消息操作" />
            </Dropdown>
          )}
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
        {/* 占位：表情、附件第一阶段不做 */}
        <div className="chat-composer-tools">
          <ComingSoon title="表情" icon={<SmileOutlined />} />
          <ComingSoon title="附件" icon={<PaperClipOutlined />} />
        </div>
        {/* 接近上限时提示字数；超出也允许点发送，消息会被标记为发送失败（只保存在本地） */}
        <span className={'chat-count' + (tooLong ? ' over' : '')}>
          {len > MAX_MESSAGE_LEN - 100 ? `${len} / ${MAX_MESSAGE_LEN}` : ''}
        </span>
        {/* 空输入时是禁用态（低饱和），有内容时立即点亮为强调色 */}
        <Button className="chat-send" type="primary" disabled={!text.trim()} onClick={submit}>
          发送
        </Button>
      </div>
    </div>
  )
}

// ---------- 非好友：添加好友入口（FR-12） ----------

// AddFriendAction 对话框头部的"添加好友"：已申请时显示等待验证，对方已申请我时引导去处理，否则弹出验证信息直接申请
function AddFriendAction({ peerId }: { peerId: number }) {
  const navigate = useNavigate()
  const me = useAuthStore((s) => s.user!)
  const send = useFriendStore((s) => s.send)
  const incoming = useFriendStore((s) => s.incoming.some((r) => r.user.id === peerId))
  const outgoing = useFriendStore((s) => s.outgoing.some((r) => r.user.id === peerId && r.status === 'pending'))
  const { message } = App.useApp()
  const [open, setOpen] = useState(false)
  const [note, setNote] = useState(`我是 ${me.username}`)
  const [sending, setSending] = useState(false)

  if (incoming) {
    return (
      <Button type="text" size="small" className="chat-header-text-btn" onClick={() => navigate('/friends/requests')}>
        对方已申请，去处理
      </Button>
    )
  }
  if (outgoing) {
    return (
      <Button type="text" size="small" className="chat-header-text-btn" disabled>
        等待验证
      </Button>
    )
  }

  const onSend = async () => {
    setSending(true)
    try {
      await send(peerId, note.trim()) // 失败时 store 会按错误码刷新申请列表 / 好友列表，按钮随之变化
      message.success('好友申请已发送')
      setOpen(false)
    } catch (err) {
      message.error(errorMessage(err))
    } finally {
      setSending(false)
    }
  }

  const content = (
    <div className="chat-add-friend">
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
        <Button size="small" onClick={() => setOpen(false)} disabled={sending}>
          取消
        </Button>
        <Button size="small" type="primary" loading={sending} onClick={onSend}>
          发送申请
        </Button>
      </div>
    </div>
  )

  return (
    <Popover open={open} onOpenChange={setOpen} trigger="click" placement="bottomRight" title="添加好友" content={content}>
      <Button type="text" size="small" className="chat-header-text-btn" icon={<UserAddOutlined />}>
        添加好友
      </Button>
    </Popover>
  )
}

// PanelRightIcon 右侧面板图标（会话信息），线条风格和 antd 的 Outlined 图标一致
function PanelRightIcon() {
  return (
    <span className="anticon" role="img" aria-hidden>
      <svg width="1em" height="1em" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8">
        <rect x="3.5" y="4.5" width="17" height="15" rx="2.5" />
        <path d="M14.5 4.5v15" />
      </svg>
    </span>
  )
}

// ComingSoon 还没实现的功能的占位图标按钮：低存在感，点击只提示
function ComingSoon({ title, icon }: { title: string; icon: ReactNode }) {
  const { message } = App.useApp()
  return (
    <Tooltip title={title}>
      <Button type="text" size="small" icon={icon} aria-label={title} onClick={() => message.info(`${title}功能开发中`)} />
    </Tooltip>
  )
}
