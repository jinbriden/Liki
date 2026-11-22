import { create } from 'zustand'
import { Code, errorMessage } from '../api/client'
import {
  listMessagesAfter,
  listMessagesBefore,
  markRead,
  MAX_MESSAGE_LEN,
  messageLength,
  type Message,
  type MessagePage,
} from '../api/message'
import * as cache from '../db/chatCache'
import { socket, type SocketStatus } from '../realtime/socket'
import { findRange, topRange, type Range } from '../utils/ranges'
import { useAuthStore } from './auth'
import { applyMessageToList, applyReadToList, useChatListStore } from './chatList'
import type { ChatMessage } from './chatTypes'

export type { ChatMessage, MessageStatus } from './chatTypes'

// 聊天状态：每个会话正在显示的消息窗口、本地待发送的消息、已读推进、连接状态。
//
// ---------- 消息窗口 ----------
// 对话框里显示的是一段连续的 seq 区间 [winLo, winHi]（窗口），它一定落在某个"已同步区间"里（见 utils/ranges.ts），
// 所以窗口内不会有没拉到的消息。窗口三种来源：
//   - 最新一页（打开会话、跳到最新）：先用本地缓存的最新一段，再向服务端增量同步；本地落后太多就直接拉最新一页（FR-17）；
//   - 向上 / 向下扩展（滚动到顶部 / 底部，FR-18）：已同步区间内的读本地缓存，碰到缺口或超出范围再请求服务端；
//   - 跳到第一条未读（FR-15）：从那里开始读一页，之后向下滚动继续加载，直到和最新消息接上。
// atLatest 表示窗口已经到了最新：只有这时新推送的消息才直接追加进窗口，本地待发送的消息才显示在末尾。
//
// ---------- 可靠性（FR-11），和 M4 相同 ----------
//   1. 每条消息生成唯一的 clientMsgId，先显示为"发送中"；
//   2. 5 秒没收到 ACK 就用同一个 clientMsgId 重发（服务端按它去重），断线期间暂停，重连后立刻重发；
//   3. 30 秒仍未确认，标记为发送失败，用户可以点击重试（还是同一个 clientMsgId）；
//   4. 收到推送时发现 seq 跳号，按窗口末尾的 seq 向服务端补拉。
// 待发送的消息同时写进 IndexedDB，刷新页面后显示为"发送失败，点击重试"（FR-16）。
//
// ---------- 已读（FR-15） ----------
// 页面（ChatPanel）用 IntersectionObserver 报告"哪些消息的底边出现在了可视范围内"（reportSeen），
// 这里从当前 readSeq 往后连续推进：碰到第一条没看到的对方消息就停下，自己的消息直接跳过。

export type ScrollTarget = { kind: 'seq'; seq: number; nonce: number } | { kind: 'bottom'; nonce: number }

export interface Conversation {
  peerId: number
  messages: ChatMessage[] // 窗口内已确认的消息，按 seq 升序
  winLo: number // 窗口覆盖的 seq 区间。空窗口为 [1, 0]
  winHi: number
  atLatest: boolean
  hasMoreBefore: boolean // 窗口前面还有更早的消息（winLo > 1）
  ready: boolean // 窗口已经初始化过
  loading: boolean // 正在初始化 / 跳转
  loadingOlder: boolean
  loadingNewer: boolean
  error: string | null
  // 打开会话时第一条未读消息的 seq，"以下为新消息"分隔线画在它上面，离开会话前不变。
  // undefined 表示还不知道（会话列表还没拉下来），null 表示打开时没有未读
  dividerSeq: number | null | undefined
  // 看到过的最大 seq（打开、跳转时清零）。大于 readSeq 说明有未读被跳过了（在屏幕上方没看到），
  // 页面据此显示顶部"N 条新消息"横条；底部"N 条新消息"按钮也用它算还有多少没看到
  maxSeen: number
  scrollTarget: ScrollTarget | null // 要求页面滚动到某处（跳转后）
  // 每次整体替换窗口（跳转、重新加载、离开会话）都加 1。异步加载回来时代数变了，说明结果已经过时，直接丢弃
  epoch: number
}

export type SendResult = { ok: true } | { ok: false; error: string }

const PAGE = 30 // 打开、翻页、跳转时每次显示的条数
const SYNC_PAGE = 100 // 增量同步每页条数；本地落后超过这么多时不再逐页补齐，直接拉最新一页（FR-17）
const MAX_SYNC_PAGES = 5
const CONTEXT_BEFORE_UNREAD = 3 // 跳到第一条未读时，在它上面多显示几条已读的消息作为上下文
const READ_DEBOUNCE = 300 // 滚动阅读时已读位置变化很频繁，合并后再上报
const ACK_TIMEOUT = 5_000
const FAIL_AFTER = 30_000

export const TOO_LONG_ERROR = '消息过长，请重新发送'
const SEND_FAILED_ERROR = '发送失败，点击重试'

interface ChatState {
  convs: Record<number, Conversation>
  locals: Record<number, ChatMessage[]> // 本地待发送 / 发送失败的消息，按发送顺序
  connection: SocketStatus

  open: (peerId: number) => void
  close: (peerId: number) => void
  loadOlder: (peerId: number) => Promise<void>
  loadNewer: (peerId: number) => Promise<void>
  jumpToUnread: (peerId: number) => Promise<void>
  jumpToLatest: (peerId: number) => void
  markAllRead: (peerId: number) => void
  reportSeen: (peerId: number, seqs: number[]) => void
  send: (peerId: number, text: string) => SendResult
  retry: (peerId: number, key: string) => void
}

const emptyConv = (peerId: number): Conversation => ({
  peerId,
  messages: [],
  winLo: 1,
  winHi: 0,
  atLatest: true,
  hasMoreBefore: false,
  ready: false,
  loading: false,
  loadingOlder: false,
  loadingNewer: false,
  error: null,
  dividerSeq: undefined,
  maxSeen: 0,
  scrollTarget: null,
  epoch: 0,
})

const keyOf = (senderId: number, clientMsgId: string) => `${senderId}:${clientMsgId}`
const myId = () => useAuthStore.getState().user?.id ?? 0
const currentToken = () => useAuthStore.getState().token
const getConv = (peerId: number) => useChatStore.getState().convs[peerId] as Conversation | undefined
const summaryOf = (peerId: number) => useChatListStore.getState().chats[peerId]
// latestSeq 已知的会话最新 seq（来自会话列表，推送会实时更新它）
const latestSeq = (peerId: number) => summaryOf(peerId)?.lastSeq ?? 0

// peerOf 消息属于和谁的会话：我发的看接收方，别人发的看发送方
const peerOf = (m: Message) => (m.senderId === myId() ? m.recipientId : m.senderId)

let nonce = 0
const target = (t: { kind: 'seq'; seq: number } | { kind: 'bottom' }): ScrollTarget => ({ ...t, nonce: ++nonce })

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

// 一页服务端结果覆盖了哪段 seq（用于记录已同步区间）。
// 向前翻页（beforeSeq）：hasMore 时从这一页第一条开始，否则一直到 1；不带 beforeSeq 的最新一页到最后一条为止
function coveredBefore(page: MessagePage, beforeSeq?: number): Range | null {
  if (page.hasMore && page.items.length === 0) return null // 不应出现；出现了也不能据此把区间记到 1
  const lo = page.hasMore ? page.items[0].seq : 1
  const hi = beforeSeq !== undefined ? beforeSeq - 1 : (page.items.at(-1)?.seq ?? 0)
  return lo <= hi ? [lo, hi] : null
}

// 向后同步（afterSeq）：从 afterSeq + 1 到这一页最后一条
function coveredAfter(page: MessagePage, afterSeq: number): Range | null {
  const hi = page.items.at(-1)?.seq ?? afterSeq
  return afterSeq + 1 <= hi ? [afterSeq + 1, hi] : null
}

function newClientMsgId(): string {
  if (typeof crypto.randomUUID === 'function') return crypto.randomUUID()
  // randomUUID 只在安全上下文（HTTPS、localhost、127.0.0.1）可用，其他情况退回 getRandomValues
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) => b.toString(16).padStart(2, '0')).join('')
}

// 每个打开着的会话里"看到过"的 seq（不持久化，离开会话就清空）
const seen = new Map<number, Set<number>>()
let openPeer: number | null = null

export const useChatStore = create<ChatState>()((_set, get) => ({
  convs: {},
  locals: {},
  connection: socket.getStatus(),

  open: (peerId) => {
    openPeer = peerId
    seen.set(peerId, new Set())
    const c = getConv(peerId)
    updateConv(peerId, (c) => ({ ...c, dividerSeq: dividerFrom(peerId), maxSeen: 0, scrollTarget: null }))
    if (c?.ready && c.atLatest) void syncAfter(peerId) // 内存里已经是最新的窗口：补齐离开期间的消息即可
    else void showLatest(peerId)
  },

  close: (peerId) => {
    if (openPeer === peerId) openPeer = null
    seen.delete(peerId)
    void flushRead(peerId)
    updateConv(peerId, (c) => {
      const next = { ...c, epoch: c.epoch + 1, dividerSeq: undefined, maxSeen: 0, scrollTarget: null }
      next.loading = next.loadingOlder = next.loadingNewer = false
      if (!c.atLatest) return { ...next, ready: false } // 下次打开重新定位到最新
      // 只在内存里留最新一页，避免来回翻看之后窗口越来越大；更早的消息还在 IndexedDB 里
      if (c.messages.length > PAGE) {
        const msgs = c.messages.slice(-PAGE)
        return { ...next, messages: msgs, winLo: msgs[0].seq!, hasMoreBefore: msgs[0].seq! > 1 }
      }
      return next
    })
  },

  loadOlder: async (peerId) => {
    const c = getConv(peerId)
    if (!c?.ready || c.loadingOlder || !c.hasMoreBefore) return
    const alive = guard(peerId)
    updateConv(peerId, (c) => ({ ...c, loadingOlder: true }))
    try {
      const lo = c.winLo
      const r = findRange(await cache.loadRanges(peerId), lo)
      if (!alive()) return
      if (r && r[0] < lo) {
        // 前面还在同一段已同步区间里：读本地
        const msgs = await cache.readMessages(peerId, r[0], lo - 1, PAGE, 'end')
        if (!alive()) return
        prependWindow(peerId, msgs, msgs.length >= PAGE ? msgs[0].seq : r[0])
        return
      }
      // 到了缺口或本地范围之外：向服务端拉
      const page = await listMessagesBefore(peerId, lo, PAGE)
      if (!alive()) return
      const cov = coveredBefore(page, lo)
      if (cov) await cache.saveMessages(peerId, page.items, cov)
      if (!alive()) return
      prependWindow(peerId, page.items, cov ? cov[0] : lo)
    } catch (err) {
      if (alive()) updateConv(peerId, (c) => ({ ...c, error: errorMessage(err) }))
    } finally {
      if (alive()) updateConv(peerId, (c) => ({ ...c, loadingOlder: false }))
    }
  },

  loadNewer: async (peerId) => {
    const c = getConv(peerId)
    if (!c?.ready || c.atLatest || c.loadingNewer) return
    const alive = guard(peerId)
    updateConv(peerId, (c) => ({ ...c, loadingNewer: true }))
    try {
      const hi = c.winHi
      const r = findRange(await cache.loadRanges(peerId), hi)
      if (!alive()) return
      if (r && r[1] > hi) {
        const msgs = await cache.readMessages(peerId, hi + 1, r[1], PAGE, 'start')
        if (!alive()) return
        const newHi = msgs.length >= PAGE ? msgs.at(-1)!.seq : r[1]
        appendWindow(peerId, msgs, newHi)
        if (newHi >= latestSeq(peerId)) {
          updateConv(peerId, (c) => ({ ...c, atLatest: true }))
          void syncAfter(peerId) // 本地缓存到头了，再问一次服务端有没有更新的
        }
        return
      }
      const page = await listMessagesAfter(peerId, hi, PAGE)
      if (!alive()) return
      const cov = coveredAfter(page, hi)
      if (cov) await cache.saveMessages(peerId, page.items, cov)
      if (!alive()) return
      appendWindow(peerId, page.items, cov?.[1])
      if (!page.hasMore) updateConv(peerId, (c) => ({ ...c, atLatest: true }))
    } catch (err) {
      if (alive()) updateConv(peerId, (c) => ({ ...c, error: errorMessage(err) }))
    } finally {
      if (alive()) updateConv(peerId, (c) => ({ ...c, loadingNewer: false }))
    }
  },

  // jumpToUnread 跳到当前第一条未读（readSeq + 1）。不用打开时的分隔线位置：之后可能已经读了一部分
  jumpToUnread: async (peerId) => {
    const c = getConv(peerId)
    const sum = summaryOf(peerId)
    if (!c?.ready || !sum || sum.readSeq >= sum.lastSeq) return
    const seq = sum.readSeq + 1
    if (c.messages.length > 0 && seq >= c.winLo && seq <= c.winHi) {
      updateConv(peerId, (c) => ({ ...c, maxSeen: 0, scrollTarget: target({ kind: 'seq', seq }) }))
      return
    }

    // 第一条未读不在窗口里：以它为起点重新建一个窗口
    const alive = bump(peerId)
    updateConv(peerId, (c) => ({ ...c, loading: true, error: null }))
    const start = Math.max(1, seq - CONTEXT_BEFORE_UNREAD)
    const scrollTarget = target({ kind: 'seq', seq })
    try {
      const r = findRange(await cache.loadRanges(peerId), seq)
      if (!alive()) return
      if (r) {
        const lo = Math.max(r[0], start)
        const msgs = await cache.readMessages(peerId, lo, r[1], PAGE, 'start')
        if (!alive()) return
        const hi = msgs.length >= PAGE ? msgs.at(-1)!.seq : r[1]
        const atLatest = hi >= latestSeq(peerId)
        setWindow(peerId, msgs, lo, hi, atLatest, { scrollTarget, maxSeen: 0 })
        if (atLatest) void syncAfter(peerId)
        return
      }
      const page = await listMessagesAfter(peerId, start - 1, PAGE)
      if (!alive()) return
      const cov = coveredAfter(page, start - 1)
      if (cov) await cache.saveMessages(peerId, page.items, cov)
      if (!alive()) return
      setWindow(peerId, page.items, start, cov ? cov[1] : start - 1, !page.hasMore, { scrollTarget, maxSeen: 0 })
    } catch (err) {
      if (alive()) updateConv(peerId, (c) => ({ ...c, loading: false, error: errorMessage(err) }))
    }
  },

  jumpToLatest: (peerId) => {
    const c = getConv(peerId)
    if (c?.ready && c.atLatest) {
      updateConv(peerId, (c) => ({ ...c, scrollTarget: target({ kind: 'bottom' }) }))
      return
    }
    void showLatest(peerId)
  },

  // markAllRead "标记为已读"：推进到此刻已知的最新 seq（FR-15）
  markAllRead: (peerId) => {
    const c = getConv(peerId)
    const latest = Math.max(latestSeq(peerId), c?.winHi ?? 0)
    if (latest > 0) reportRead(peerId, latest)
  },

  reportSeen: (peerId, seqs) => {
    const set = seen.get(peerId)
    const c = getConv(peerId)
    if (!set || !c?.ready) return // 会话没打开（页面已经离开）
    let maxSeen = c.maxSeen
    for (const s of seqs) {
      set.add(s)
      if (s > maxSeen) maxSeen = s
    }
    if (maxSeen !== c.maxSeen) updateConv(peerId, (c) => ({ ...c, maxSeen }))

    const sum = summaryOf(peerId)
    if (!sum) return
    // 从当前已读位置往后连续推进。窗口不包含 readSeq 的下一条时（上面还有没加载出来的未读），一条也不能推进
    let read = sum.readSeq
    if (c.winLo > read + 1) return
    const me = myId()
    let passedAll = true
    for (const m of c.messages) {
      if (m.seq! <= read) continue
      if (m.senderId === me || set.has(m.seq!)) {
        read = m.seq!
      } else {
        passedAll = false
        break
      }
    }
    // 窗口里的消息全部看过了：窗口末尾如果有被删除的 seq（M6），也一并越过
    if (passedAll) read = Math.max(read, c.winHi)
    if (read > sum.readSeq) reportRead(peerId, read)
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
    const m = get().locals[peerId]?.find((x) => x.key === key)
    if (!m || m.status !== 'failed' || !m.retryable) return
    updateLocal(peerId, key, (x) => ({ ...x, status: 'sending', error: undefined, retryable: undefined }))
    startPending(m.clientMsgId, peerId) // 同一个 clientMsgId：如果其实已经写库了，服务端会直接返回已有的那条
  },
}))

// initChat 登录后（主框架挂载时）调用：打开本地缓存，恢复会话列表快照和待发送的消息，再从服务端拉会话列表
export async function initChat(userId: number) {
  cache.openCache(userId)
  const token = currentToken()
  await useChatListStore.getState().loadSnapshot()
  const stored = await cache.loadAllLocal()
  if (currentToken() !== token) return

  const restored: Record<number, ChatMessage[]> = {}
  for (const { peerId, ...m } of stored) {
    let msg: ChatMessage = m
    if (m.status === 'sending') {
      // 上次关闭页面时还没确认：不自动重发，交给用户决定（FR-16）
      msg = { ...m, status: 'failed', error: SEND_FAILED_ERROR, retryable: true }
      void cache.putLocal(peerId, msg)
    }
    ;(restored[peerId] ??= []).push(msg)
  }
  useChatStore.setState((s) => {
    const locals = { ...s.locals }
    for (const [peer, list] of Object.entries(restored)) {
      const cur = locals[Number(peer)] ?? []
      const keys = new Set(cur.map((m) => m.key))
      locals[Number(peer)] = [...list.filter((m) => !keys.has(m.key)), ...cur].sort(
        (a, b) => Date.parse(a.createdAt) - Date.parse(b.createdAt),
      )
    }
    return { locals }
  })
  void useChatListStore.getState().load()
}

// ---------- 内部工具：只在本文件里使用，页面通过上面的 action 调用 ----------

function updateConv(peerId: number, fn: (c: Conversation) => Conversation) {
  useChatStore.setState((s) => ({ convs: { ...s.convs, [peerId]: fn(s.convs[peerId] ?? emptyConv(peerId)) } }))
}

// guard 记下当前账号和窗口代数，返回"结果是否仍然有效"的检查函数
function guard(peerId: number) {
  const token = currentToken()
  const epoch = getConv(peerId)?.epoch ?? 0
  return () => currentToken() === token && (getConv(peerId)?.epoch ?? 0) === epoch
}

// bump 准备整体替换窗口：作废所有进行中的加载，返回新一代的检查函数
function bump(peerId: number) {
  updateConv(peerId, (c) => ({ ...c, epoch: c.epoch + 1 }))
  return guard(peerId)
}

function dividerFrom(peerId: number): number | null | undefined {
  const s = summaryOf(peerId)
  if (!s) return undefined
  return s.readSeq < s.lastSeq ? s.readSeq + 1 : null
}

function setWindow(
  peerId: number,
  msgs: Message[],
  lo: number,
  hi: number,
  atLatest: boolean,
  extra: Partial<Conversation> = {},
) {
  updateConv(peerId, (c) => ({
    ...c,
    messages: msgs.map(fromServer),
    winLo: lo,
    winHi: hi,
    atLatest,
    hasMoreBefore: lo > 1,
    ready: true,
    loading: false,
    loadingOlder: false,
    loadingNewer: false,
    error: null,
    ...extra,
  }))
  confirmLocals(peerId, msgs)
}

// appendWindow 在窗口末尾接上更新的消息（seq 都大于 winHi），hi 是这批消息覆盖到的 seq
function appendWindow(peerId: number, msgs: Message[], hi?: number) {
  updateConv(peerId, (c) => {
    const fresh = msgs.filter((m) => m.seq > c.winHi).map(fromServer)
    const winHi = Math.max(c.winHi, hi ?? 0, fresh.at(-1)?.seq ?? 0)
    if (fresh.length === 0 && winHi === c.winHi) return c
    return { ...c, messages: [...c.messages, ...fresh], winHi }
  })
  confirmLocals(peerId, msgs)
}

// prependWindow 在窗口前面接上更早的消息（seq 都小于 winLo），lo 是这批消息覆盖到的 seq
function prependWindow(peerId: number, msgs: Message[], lo: number) {
  updateConv(peerId, (c) => {
    const older = msgs.filter((m) => m.seq < c.winLo).map(fromServer)
    const winLo = Math.min(c.winLo, lo)
    return { ...c, messages: [...older, ...c.messages], winLo, hasMoreBefore: winLo > 1 }
  })
  confirmLocals(peerId, msgs)
}

// showLatest 把窗口定位到最新：优先用本地缓存的最新一段，再增量同步；本地没有或落后太多时直接拉服务端最新一页
async function showLatest(peerId: number) {
  const alive = bump(peerId)
  updateConv(peerId, (c) => ({ ...c, loading: true, error: null }))
  const scrollTarget = target({ kind: 'bottom' })
  const top = topRange(await cache.loadRanges(peerId))
  if (!alive()) return
  if (top && latestSeq(peerId) - top[1] <= SYNC_PAGE) {
    const msgs = await cache.readMessages(peerId, top[0], top[1], PAGE, 'end')
    if (!alive()) return
    setWindow(peerId, msgs, msgs.length >= PAGE ? msgs[0].seq : top[0], top[1], true, { scrollTarget })
    await syncAfter(peerId)
    return
  }
  await loadLatestFromServer(peerId, alive, scrollTarget)
}

async function loadLatestFromServer(peerId: number, alive: () => boolean, scrollTarget: ScrollTarget | null) {
  updateConv(peerId, (c) => ({ ...c, loading: true, error: null }))
  try {
    const page = await listMessagesBefore(peerId, undefined, PAGE) // 只加载最近一页（FR-18）
    if (!alive()) return
    const cov = coveredBefore(page)
    if (cov || page.items.length > 0) await cache.saveMessages(peerId, page.items, cov ?? undefined)
    if (!alive()) return
    setWindow(peerId, page.items, cov ? cov[0] : 1, cov ? cov[1] : 0, true, { scrollTarget })
    // 加载期间到达的推送可能不在这一页里（推送只在窗口就绪后才直接追加）
    if (latestSeq(peerId) > (getConv(peerId)?.winHi ?? 0)) void syncAfter(peerId)
  } catch (err) {
    if (alive()) updateConv(peerId, (c) => ({ ...c, loading: false, error: errorMessage(err) }))
  }
}

// 同一会话同一时刻只跑一个增量同步；同步期间又有新的同步请求，就在结束后再跑一次
const syncing = new Map<number, boolean>() // peerId → 是否需要再跑一次

// syncAfter 窗口在最新时，从 winHi 往后向服务端补齐（FR-17）
async function syncAfter(peerId: number) {
  if (syncing.has(peerId)) {
    syncing.set(peerId, true)
    return
  }
  syncing.set(peerId, false)
  const token = currentToken()
  try {
    for (let i = 0; ; i++) {
      const c = getConv(peerId)
      if (!c?.ready || !c.atLatest) return
      // 落后太多（例如离线很久后重连）：不逐页补，直接拉最新一页，中间留下缺口，翻到时再补（FR-17）
      if (i >= MAX_SYNC_PAGES || latestSeq(peerId) - c.winHi > SYNC_PAGE) {
        await loadLatestFromServer(peerId, bump(peerId), null)
        return
      }
      const alive = guard(peerId)
      const page = await listMessagesAfter(peerId, c.winHi, SYNC_PAGE)
      if (!alive()) return
      const cov = coveredAfter(page, c.winHi)
      if (cov) await cache.saveMessages(peerId, page.items, cov)
      if (!alive()) return
      for (const m of page.items) applyMessageToList(m)
      appendWindow(peerId, page.items, cov?.[1])
      if (!page.hasMore) return
    }
  } catch (err) {
    console.warn('[chat] sync failed', err) // 下次推送或重连时还会再同步
  } finally {
    const rerun = syncing.get(peerId)
    syncing.delete(peerId)
    if (rerun && currentToken() === token) void syncAfter(peerId)
  }
}

// receive 收到一条已确认的消息（message.new 推送或自己的 ACK）
function receive(m: Message) {
  const peerId = peerOf(m)
  applyMessageToList(m)
  void cache.saveMessages(peerId, [m], [m.seq, m.seq]) // 单条也是一段已同步区间，和相邻的区间自动合并
  const c = getConv(peerId)
  if (!c?.ready || !c.atLatest) {
    confirmLocals(peerId, [m]) // 窗口不在最新（或还没打开）：只存起来，滚到那里时从本地读
    return
  }
  if (m.seq <= c.winHi + 1) {
    appendWindow(peerId, [m], m.seq) // 正好接在末尾（或者重复收到，appendWindow 会忽略）
  } else {
    confirmLocals(peerId, [m])
    void syncAfter(peerId) // seq 跳号：中间有消息没收到（例如推送丢了），补拉
  }
}

// ---------- 已读上报 ----------

const readTimers = new Map<number, { seq: number; timer: ReturnType<typeof setTimeout> }>()

// reportRead 本地立即更新（角标马上变化），合并 300 毫秒内的多次推进后再上报服务端
function reportRead(peerId: number, seq: number) {
  applyReadToList(peerId, seq)
  const p = readTimers.get(peerId)
  clearTimeout(p?.timer)
  readTimers.set(peerId, {
    seq: Math.max(seq, p?.seq ?? 0),
    timer: setTimeout(() => void flushRead(peerId), READ_DEBOUNCE),
  })
}

async function flushRead(peerId: number) {
  const p = readTimers.get(peerId)
  if (!p) return
  clearTimeout(p.timer)
  readTimers.delete(peerId)
  const token = currentToken()
  try {
    const res = await markRead(peerId, p.seq)
    if (currentToken() === token) applyReadToList(peerId, res.readSeq)
  } catch (err) {
    console.warn('[chat] mark read failed', err) // 不重试：下次看到新消息、或重新打开会话时还会再报
  }
}

// ---------- 本地待发送的消息 ----------

function addLocal(peerId: number, m: ChatMessage) {
  useChatStore.setState((s) => ({ locals: { ...s.locals, [peerId]: [...(s.locals[peerId] ?? []), m] } }))
  void cache.putLocal(peerId, m)
}

function updateLocal(peerId: number, key: string, fn: (m: ChatMessage) => ChatMessage) {
  const list = useChatStore.getState().locals[peerId]
  const cur = list?.find((m) => m.key === key)
  if (!cur) return
  const next = fn(cur)
  useChatStore.setState((s) => ({
    locals: { ...s.locals, [peerId]: (s.locals[peerId] ?? []).map((m) => (m.key === key ? next : m)) },
  }))
  void cache.putLocal(peerId, next)
}

// confirmLocals 自己发的消息被服务端确认了（ACK、推送、或在其他标签页发出后同步回来）：从待发送里移除
function confirmLocals(peerId: number, msgs: Message[]) {
  const me = myId()
  const keys = new Set<string>()
  for (const m of msgs) {
    if (m.senderId !== me) continue
    clearPending(m.clientMsgId)
    keys.add(keyOf(m.senderId, m.clientMsgId))
  }
  const list = useChatStore.getState().locals[peerId]
  if (keys.size === 0 || !list?.some((m) => keys.has(m.key))) return
  useChatStore.setState((s) => ({
    locals: { ...s.locals, [peerId]: (s.locals[peerId] ?? []).filter((m) => !keys.has(m.key)) },
  }))
  for (const k of keys) void cache.deleteLocal(k)
}

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
  const m = useChatStore.getState().locals[p.peerId]?.find((x) => x.key === keyOf(myId(), clientMsgId))
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
  updateLocal(p.peerId, keyOf(myId(), clientMsgId), (m) => ({ ...m, status: 'failed', error, retryable }))
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
    // 包括"已标记失败之后才迟到的 ACK"：消息其实已经写库，改为已发送
    receive(ack.message)
    return
  }
  if (ack.code === Code.Internal) return // 服务端临时故障：保持发送中，ACK 计时器到了会重发
  // 被服务端明确拒绝（过长、非好友等），重发也没用，标记失败，不能重试
  markFailed(ack.clientMsgId, ack.code === Code.MessageTooLong ? TOO_LONG_ERROR : ack.msg, false)
})

socket.on('message.new', (data) => receive((data as { message: Message }).message))

// 每次连上：先重发还没确认的消息。断线重连时还要刷新会话列表（离线期间的新消息、未读数），
// 再把内存里处于最新的窗口补齐（FR-17）；列表先拉完，补齐时才知道落后了多少
socket.onOpen(({ resumed }) => {
  flushPending()
  if (!resumed) return
  const token = currentToken()
  void useChatListStore
    .getState()
    .load()
    .then(() => {
      if (currentToken() !== token) return
      for (const c of Object.values(useChatStore.getState().convs)) {
        if (c.ready && c.atLatest) void syncAfter(c.peerId)
      }
    })
})

socket.onStatus((connection) => useChatStore.setState({ connection }))

// 会话列表晚于会话打开才拉下来（例如直接刷新 /chat/:peerId）：这时才能确定"以下为新消息"画在哪里
useChatListStore.subscribe(() => {
  if (openPeer === null) return
  const c = getConv(openPeer)
  if (c && c.dividerSeq === undefined) {
    const d = dividerFrom(openPeer)
    if (d !== undefined) updateConv(openPeer, (c) => ({ ...c, dividerSeq: d }))
  }
})

// 退出登录或切换账号时清空聊天数据和所有计时器，关闭本地缓存（是否删除由 logout 决定）
useAuthStore.subscribe((state, prev) => {
  if (state.token === prev.token) return
  for (const id of [...pending.keys()]) clearPending(id)
  for (const p of readTimers.values()) clearTimeout(p.timer)
  readTimers.clear()
  syncing.clear()
  seen.clear()
  openPeer = null
  cache.closeCache()
  useChatStore.setState({ convs: {}, locals: {} })
})
