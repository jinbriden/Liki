import { create } from 'zustand'
import { Code, errorMessage } from '../api/client'
import { listMessagesAfter, listMessagesBefore, MAX_MESSAGE_LEN, messageLength, type Message } from '../api/message'
import { socket, type SocketStatus } from '../realtime/socket'
import { useAuthStore } from './auth'

// 聊天状态：每个会话的消息列表、发送中的消息、连接状态。
// 消息只保存在内存里，刷新页面后从服务端重新拉取；本地缓存（IndexedDB）在 M5 实现。
//
// 可靠性（FR-11）在前端的部分：
//   1. 每条消息生成唯一的 clientMsgId，先显示为"发送中"；
//   2. 5 秒没收到 ACK 就用同一个 clientMsgId 重发（服务端按它去重），断线期间暂停，重连后立刻重发；
//   3. 30 秒仍未确认，标记为发送失败，用户可以点击重试（还是同一个 clientMsgId）；
//   4. 收到推送时发现 seq 不连续，或断线重连后，按本地最大 seq 向服务端拉取缺失的消息。

export type MessageStatus = 'sending' | 'sent' | 'failed'

export interface ChatMessage {
  // `${senderId}:${clientMsgId}`：同一条消息从"发送中"变成"已发送"时 key 不变，React 不会重建这一行
  key: string
  clientMsgId: string
  id?: number // 服务端确认后才有
  seq?: number // 服务端确认后才有
  senderId: number
  recipientId: number
  content: string
  createdAt: string
  status: MessageStatus
  error?: string // 发送失败的原因，显示在消息下方
  retryable?: boolean // 能否点击重试（消息过长、非好友等被服务端明确拒绝的不能重试）
}

export interface Conversation {
  peerId: number
  // 已确认的消息按 seq 升序，后面跟着本地的"发送中 / 发送失败"消息（按发送顺序）
  messages: ChatMessage[]
  loaded: boolean // 最新一页已从服务端加载过
  loading: boolean
  loadingOlder: boolean
  hasMoreBefore: boolean
  error: string | null
}

export type SendResult = { ok: true } | { ok: false; error: string }

const ACK_TIMEOUT = 5_000
const FAIL_AFTER = 30_000
const MAX_SYNC_PAGES = 20 // 一次增量同步最多拉 20 页（2000 条），防止异常情况下无限循环

export const TOO_LONG_ERROR = '消息过长，请重新发送'
const SEND_FAILED_ERROR = '发送失败，点击重试'

interface ChatState {
  convs: Record<number, Conversation>
  connection: SocketStatus

  open: (peerId: number) => void
  loadOlder: (peerId: number) => Promise<void>
  send: (peerId: number, text: string) => SendResult
  retry: (peerId: number, key: string) => void
}

const emptyConv = (peerId: number): Conversation => ({
  peerId,
  messages: [],
  loaded: false,
  loading: false,
  loadingOlder: false,
  hasMoreBefore: false,
  error: null,
})

const keyOf = (senderId: number, clientMsgId: string) => `${senderId}:${clientMsgId}`
const myId = () => useAuthStore.getState().user?.id ?? 0
const currentToken = () => useAuthStore.getState().token

// peerOf 消息属于和谁的会话：我发的看接收方，别人发的看发送方
const peerOf = (m: Message) => (m.senderId === myId() ? m.recipientId : m.senderId)

function fromServer(m: Message): ChatMessage {
  return {
    key: keyOf(m.senderId, m.clientMsgId),
    clientMsgId: m.clientMsgId,
    id: m.id,
    seq: m.seq,
    senderId: m.senderId,
    recipientId: m.recipientId,
    content: m.content,
    createdAt: m.createdAt,
    status: 'sent',
  }
}

function sortMessages(list: ChatMessage[]): ChatMessage[] {
  const confirmed = list.filter((m) => m.seq !== undefined).sort((a, b) => a.seq! - b.seq!)
  const local = list.filter((m) => m.seq === undefined) // filter 保持原有顺序，即发送顺序
  return [...confirmed, ...local]
}

// 本地已确认消息的最大 / 最小 seq，没有时为 0
function maxSeq(c: Conversation | undefined) {
  let max = 0
  for (const m of c?.messages ?? []) if (m.seq !== undefined && m.seq > max) max = m.seq
  return max
}

function minSeq(c: Conversation) {
  let min = 0
  for (const m of c.messages) if (m.seq !== undefined && (min === 0 || m.seq < min)) min = m.seq
  return min
}

function newClientMsgId(): string {
  if (typeof crypto.randomUUID === 'function') return crypto.randomUUID()
  // randomUUID 只在安全上下文（HTTPS、localhost、127.0.0.1）可用，其他情况退回 getRandomValues
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) => b.toString(16).padStart(2, '0')).join('')
}

export const useChatStore = create<ChatState>()((_set, get) => ({
  convs: {},
  connection: socket.getStatus(),

  open: (peerId) => {
    const c = get().convs[peerId]
    if (!c?.loaded) void loadLatest(peerId)
    else void syncAfter(peerId) // 已经加载过：补齐离开期间可能漏掉的消息（FR-17）
  },

  loadOlder: async (peerId) => {
    const c = get().convs[peerId]
    if (!c || c.loadingOlder || !c.hasMoreBefore) return
    const before = minSeq(c)
    const token = currentToken()
    updateConv(peerId, (c) => ({ ...c, loadingOlder: true }))
    try {
      const page = await listMessagesBefore(peerId, before || undefined)
      if (currentToken() !== token) return
      mergeServer(peerId, page.items)
      updateConv(peerId, (c) => ({ ...c, loadingOlder: false, hasMoreBefore: page.hasMore }))
    } catch (err) {
      if (currentToken() !== token) return
      updateConv(peerId, (c) => ({ ...c, loadingOlder: false, error: errorMessage(err) }))
    }
  },

  send: (peerId, text) => {
    if (!text.trim()) return { ok: false, error: '' }
    const me = myId()
    const clientMsgId = newClientMsgId()
    const msg: ChatMessage = {
      key: keyOf(me, clientMsgId),
      clientMsgId,
      senderId: me,
      recipientId: peerId,
      content: text,
      createdAt: new Date().toISOString(),
      status: 'sending',
    }

    // 超过 1500 字：不发给服务端，直接作为一条"发送失败"的消息留在本地，只有自己能看到
    if (messageLength(text) > MAX_MESSAGE_LEN) {
      addLocal(peerId, { ...msg, status: 'failed', error: TOO_LONG_ERROR, retryable: false })
      return { ok: false, error: TOO_LONG_ERROR }
    }

    addLocal(peerId, msg)
    startPending(clientMsgId, peerId)
    return { ok: true }
  },

  retry: (peerId, key) => {
    const m = get().convs[peerId]?.messages.find((x) => x.key === key)
    if (!m || m.status !== 'failed' || !m.retryable) return
    updateMessage(peerId, key, (x) => ({ ...x, status: 'sending', error: undefined, retryable: undefined }))
    startPending(m.clientMsgId, peerId) // 同一个 clientMsgId：如果其实已经写库了，服务端会直接返回已有的那条
  },
}))

// ---------- 内部工具：只在本文件里使用，页面通过上面的 action 调用 ----------

function updateConv(peerId: number, fn: (c: Conversation) => Conversation) {
  useChatStore.setState((s) => ({ convs: { ...s.convs, [peerId]: fn(s.convs[peerId] ?? emptyConv(peerId)) } }))
}

function updateMessage(peerId: number, key: string, fn: (m: ChatMessage) => ChatMessage) {
  updateConv(peerId, (c) => ({ ...c, messages: c.messages.map((m) => (m.key === key ? fn(m) : m)) }))
}

function addLocal(peerId: number, m: ChatMessage) {
  updateConv(peerId, (c) => ({ ...c, messages: [...c.messages, m] }))
}

// mergeServer 把服务端的消息合并进会话：已有同一条（按 key）就替换，否则插入，再按 seq 排序。
// 推送、ACK、增量同步、翻页拉到的消息都走这里，同一条消息到达多次也只显示一条。
function mergeServer(peerId: number, msgs: Message[]) {
  if (msgs.length === 0) return
  for (const m of msgs) if (m.senderId === myId()) clearPending(m.clientMsgId)
  updateConv(peerId, (c) => {
    const byKey = new Map(c.messages.map((m, i) => [m.key, i]))
    const next = [...c.messages]
    for (const m of msgs) {
      const cm = fromServer(m)
      const i = byKey.get(cm.key)
      if (i === undefined) {
        byKey.set(cm.key, next.length)
        next.push(cm)
      } else {
        next[i] = cm // 本地"发送中 / 发送失败"的那条被服务端确认的版本替换
      }
    }
    return { ...c, messages: sortMessages(next) }
  })
}

async function loadLatest(peerId: number) {
  if (useChatStore.getState().convs[peerId]?.loading) return
  const token = currentToken()
  updateConv(peerId, (c) => ({ ...c, loading: true, error: null }))
  try {
    const page = await listMessagesBefore(peerId) // 首次打开只加载最近一页（FR-18）
    if (currentToken() !== token) return
    mergeServer(peerId, page.items)
    updateConv(peerId, (c) => ({ ...c, loading: false, loaded: true, hasMoreBefore: page.hasMore }))
  } catch (err) {
    if (currentToken() !== token) return
    updateConv(peerId, (c) => ({ ...c, loading: false, error: errorMessage(err) }))
  }
}

// 同一会话同一时刻只跑一个增量同步；同步期间又有新的同步请求，就在结束后再跑一次
const syncing = new Map<number, boolean>() // peerId → 是否需要再跑一次

async function syncAfter(peerId: number) {
  if (syncing.has(peerId)) {
    syncing.set(peerId, true)
    return
  }
  syncing.set(peerId, false)
  const token = currentToken()
  try {
    for (let i = 0; i < MAX_SYNC_PAGES; i++) {
      const after = maxSeq(useChatStore.getState().convs[peerId])
      const page = await listMessagesAfter(peerId, after)
      if (currentToken() !== token) return
      mergeServer(peerId, page.items)
      if (!page.hasMore) break
    }
  } catch (err) {
    console.warn('[chat] sync failed', err) // 下次推送或重连时还会再同步
  } finally {
    const rerun = syncing.get(peerId)
    syncing.delete(peerId)
    if (rerun && currentToken() === token) void syncAfter(peerId)
  }
}

// ---------- 发送中的消息 ----------

interface Pending {
  peerId: number
  ackTimer?: ReturnType<typeof setTimeout>
  failTimer?: ReturnType<typeof setTimeout>
}

const pending = new Map<string, Pending>() // clientMsgId → 发送状态（只有自己发的消息）

function startPending(clientMsgId: string, peerId: number) {
  clearPending(clientMsgId)
  const p: Pending = { peerId }
  pending.set(clientMsgId, p)
  p.failTimer = setTimeout(() => markFailed(clientMsgId, SEND_FAILED_ERROR, true), FAIL_AFTER)
  transmit(clientMsgId)
}

// transmit 通过 WebSocket 发出；连接没建立时什么也不做，等重连后 flushPending 统一重发
function transmit(clientMsgId: string) {
  const p = pending.get(clientMsgId)
  if (!p) return
  const m = useChatStore.getState().convs[p.peerId]?.messages.find((x) => x.key === keyOf(myId(), clientMsgId))
  if (!m) return
  clearTimeout(p.ackTimer)
  const sent = socket.send('message.send', { clientMsgId, toUserId: p.peerId, content: m.content })
  if (sent) p.ackTimer = setTimeout(() => transmit(clientMsgId), ACK_TIMEOUT)
}

function flushPending() {
  for (const id of pending.keys()) transmit(id)
}

function clearPending(clientMsgId: string) {
  const p = pending.get(clientMsgId)
  if (!p) return
  clearTimeout(p.ackTimer)
  clearTimeout(p.failTimer)
  pending.delete(clientMsgId)
}

function markFailed(clientMsgId: string, error: string, retryable: boolean) {
  const p = pending.get(clientMsgId)
  if (!p) return
  clearPending(clientMsgId)
  updateMessage(p.peerId, keyOf(myId(), clientMsgId), (m) =>
    m.status === 'sent' ? m : { ...m, status: 'failed', error, retryable },
  )
}

// ---------- WebSocket 数据包 ----------

interface AckData {
  clientMsgId: string
  code: number
  msg: string
  message?: Message
}

socket.on('message.ack', (data) => {
  const ack = data as AckData
  if (ack.code === Code.OK && ack.message) {
    // 包括"已标记失败之后才迟到的 ACK"：消息其实已经写库，改回已发送
    mergeServer(peerOf(ack.message), [ack.message])
    return
  }
  if (ack.code === Code.Internal) return // 服务端临时故障：保持发送中，ACK 计时器到了会重发
  // 被服务端明确拒绝（过长、非好友等），重发也没用，标记失败，不能重试
  markFailed(ack.clientMsgId, ack.code === Code.MessageTooLong ? TOO_LONG_ERROR : ack.msg, false)
})

socket.on('message.new', (data) => {
  const m = (data as { message: Message }).message
  const peerId = peerOf(m)
  const c = useChatStore.getState().convs[peerId]
  // seq 不连续说明中间有消息没收到（例如推送丢了），按本地最大 seq 补拉（FR-17）。
  // 没打开过的会话不检查：打开时会加载最新一页
  const gap = c?.loaded && m.seq > maxSeq(c) + 1
  mergeServer(peerId, [m])
  if (gap) void syncAfter(peerId)
})

// 每次连上（包括断线重连）：先重发还没确认的消息，再把打开过的会话补齐
socket.onOpen(() => {
  flushPending()
  for (const c of Object.values(useChatStore.getState().convs)) {
    if (c.loaded) void syncAfter(c.peerId)
  }
})

socket.onStatus((connection) => useChatStore.setState({ connection }))

// 退出登录或切换账号时清空聊天数据和所有计时器
useAuthStore.subscribe((state, prev) => {
  if (state.token === prev.token) return
  for (const id of [...pending.keys()]) clearPending(id)
  syncing.clear()
  useChatStore.setState({ convs: {} })
})
